// Package order places orders and runs each one through a persisted, orchestrated saga:
//
//	PENDING ─reserve stock→ STOCK_RESERVED ─authorize→ PAYMENT_AUTHORIZED ─capture→ PAYMENT_CAPTURED ─commit stock→ CONFIRMED
//	   │ out of stock             │ declined                 │ capture failed              │ reservation gone
//	   ▼                          ▼                          ▼                             ▼
//	REJECTED          RELEASING_STOCK ◄─ VOIDING_PAYMENT ◄───┘          REFUNDING_PAYMENT ─┘
//	                        │                                              │
//	                        └──────────────► REJECTED ◄── RELEASING_STOCK ◄┘
//
// The order row is the saga's state. Every transition is a conditional UPDATE committed together
// with a history row and an outbox event. Every step is idempotent on the order id, so a step
// interrupted by a crash or a timeout is simply run again, by the same request or by the
// recovery loop (Recover), which picks up any order whose next attempt is due.
package order

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/db"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/lab"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/money"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/outbox"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/tenancy"
)

//go:embed migrations/*.sql
var Migrations embed.FS

const (
	Schema = "orders"
	Topic  = "order.events"
)

// Saga states.
const (
	Pending           = "PENDING"
	StockReserved     = "STOCK_RESERVED"
	PaymentAuthorized = "PAYMENT_AUTHORIZED"
	PaymentCaptured   = "PAYMENT_CAPTURED"
	Confirmed         = "CONFIRMED"
	VoidingPayment    = "VOIDING_PAYMENT"
	RefundingPayment  = "REFUNDING_PAYMENT"
	ReleasingStock    = "RELEASING_STOCK"
	Rejected          = "REJECTED"
)

// Terminal reports whether no more steps will run.
func Terminal(status string) bool { return status == Confirmed || status == Rejected }

// Errors returned by Catalog and Payments implementations to classify an outcome.
// Anything else is treated as transient and retried with backoff.
var (
	ErrOutOfStock      = errors.New("order: out of stock")
	ErrDeclined        = errors.New("order: payment declined")
	ErrReservationGone = errors.New("order: stock reservation expired")
	ErrWrongState      = errors.New("order: payment is in a different state than expected")
)

var (
	ErrNotFound = errors.New("order: not found")
	ErrInvalid  = errors.New("order: invalid request")
)

// Product is what the order needs to know about a product to price it.
type Product struct {
	ID    string
	SKU   string
	Name  string
	Price money.Money
}

// Line is a product and quantity.
type Line struct {
	ProductID string
	Qty       int
}

// Catalog is the stock side of the saga (catalog service).
type Catalog interface {
	Products(ctx context.Context, ids []string) ([]Product, error)
	Reserve(ctx context.Context, orderID string, lines []Line) error // ErrOutOfStock is final
	Commit(ctx context.Context, orderID string) error                // ErrReservationGone is final
	Release(ctx context.Context, orderID string) error
}

// Payments is the money side of the saga (payment service).
type Payments interface {
	Authorize(ctx context.Context, orderID, customerID string, amount money.Money, paymentMethod string) error // ErrDeclined is final
	Capture(ctx context.Context, orderID string) error                                                         // ErrDeclined is final
	Void(ctx context.Context, orderID string) error                                                            // ErrWrongState: already captured
	Refund(ctx context.Context, orderID string) error
}

// Order as stored.
type Order struct {
	ID            string
	RequestID     string
	CustomerID    string
	PaymentMethod string
	Status        string
	Reason        string
	Total         money.Money
	Lines         []OrderLine
	Attempts      int
	LastError     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type OrderLine struct {
	ProductID string
	SKU       string
	Name      string
	Qty       int
	UnitPrice money.Money
}

// Engine places orders and drives their sagas.
type Engine struct {
	DB       db.Beginner
	Catalog  Catalog
	Payments Payments
	Log      *slog.Logger

	MaxAttempts int           // transient failures of a forward step before compensating (default 6)
	StepTimeout time.Duration // per remote call (default 5 s)
	Lease       time.Duration // how long one runner owns an order (default 30 s)
	Backoff     time.Duration // first retry delay, doubled per attempt, capped at 1 min (default 200 ms)

	// Mode picks the saga style for each new order: Orchestration (default) or Choreography.
	// The Pattern Lab switches it at runtime; an order keeps the style it was placed with.
	Mode func() string
}

func (e Engine) defaults() Engine {
	if e.MaxAttempts == 0 {
		e.MaxAttempts = 6
	}
	if e.StepTimeout == 0 {
		e.StepTimeout = 5 * time.Second
	}
	if e.Lease == 0 {
		e.Lease = 30 * time.Second
	}
	if e.Backoff == 0 {
		e.Backoff = 200 * time.Millisecond
	}
	if e.Log == nil {
		e.Log = slog.Default()
	}
	return e
}

// PlaceInput is a customer's order. RequestID makes placing idempotent: the same RequestID
// returns the order created the first time.
type PlaceInput struct {
	RequestID     string
	CustomerID    string
	PaymentMethod string
	Lines         []Line
}

// Place prices and stores the order, then runs its saga until it finishes or waitFor elapses.
// The returned order may still be in progress; the recovery loop finishes it.
func (e Engine) Place(ctx context.Context, in PlaceInput, waitFor time.Duration) (Order, error) {
	e = e.defaults()
	if in.RequestID == "" || in.CustomerID == "" || in.PaymentMethod == "" || len(in.Lines) == 0 {
		return Order{}, fmt.Errorf("%w: request id, customer, payment method and lines are required", ErrInvalid)
	}
	qty := map[string]int{}
	for _, l := range in.Lines {
		if l.Qty <= 0 || l.Qty > 1000 {
			return Order{}, fmt.Errorf("%w: quantity must be 1..1000", ErrInvalid)
		}
		qty[l.ProductID] += l.Qty
	}
	if existing, err := e.byRequest(ctx, in.RequestID); err == nil {
		return existing, nil
	}

	ids := make([]string, 0, len(qty))
	for id := range qty {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	cctx, cancel := context.WithTimeout(ctx, e.StepTimeout)
	products, err := e.Catalog.Products(cctx, ids)
	cancel()
	if errors.Is(err, ErrInvalid) {
		return Order{}, err
	}
	if err != nil {
		return Order{}, fmt.Errorf("order: price lookup: %w", err)
	}
	byID := map[string]Product{}
	for _, p := range products {
		byID[p.ID] = p
	}
	var lines []OrderLine
	var total money.Money
	for i, id := range ids {
		p, ok := byID[id]
		if !ok {
			return Order{}, fmt.Errorf("%w: unknown product %s", ErrInvalid, id)
		}
		sub, err := p.Price.Mul(int64(qty[id]))
		if err != nil {
			return Order{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		if i == 0 {
			total = sub
		} else if total, err = total.Add(sub); err != nil {
			return Order{}, fmt.Errorf("%w: all products of an order must share a currency", ErrInvalid)
		}
		lines = append(lines, OrderLine{ProductID: id, SKU: p.SKU, Name: p.Name, Qty: qty[id], UnitPrice: p.Price})
	}

	mode := Orchestration
	if e.Mode != nil && e.Mode() == Choreography {
		mode = Choreography
	}
	var id string
	err = db.InTenantTx(ctx, e.DB, func(tx pgx.Tx) error {
		// choreographed orders have no next attempt: the recovery loop never drives them
		err := tx.QueryRow(ctx, `INSERT INTO orders.orders (tenant_id, request_id, customer_id, payment_method, total_minor, currency, saga_mode, next_attempt_at)
			VALUES (current_setting('app.tenant_id')::uuid, $1, $2, $3, $4, $5, $6, CASE WHEN $6 = 'orchestration' THEN now() END)
			ON CONFLICT (tenant_id, request_id) DO NOTHING RETURNING id`,
			in.RequestID, in.CustomerID, in.PaymentMethod, total.Amount, total.Currency, mode).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return errDuplicate // a concurrent Place with the same request id won
		}
		if err != nil {
			return err
		}
		for _, l := range lines {
			if _, err := tx.Exec(ctx, `INSERT INTO orders.lines (order_id, tenant_id, product_id, sku, name, qty, unit_price_minor)
				VALUES ($1, current_setting('app.tenant_id')::uuid, $2, $3, $4, $5, $6)`, id, l.ProductID, l.SKU, l.Name, l.Qty, l.UnitPrice.Amount); err != nil {
				return err
			}
		}
		o := Order{ID: id, CustomerID: in.CustomerID, Status: Pending, Total: total}
		if err := emit(ctx, tx, o, "", Pending, "placed"); err != nil {
			return err
		}
		if mode == Choreography {
			return placedEvent(ctx, tx, o, in.PaymentMethod, lines)
		}
		return nil
	})
	if errors.Is(err, errDuplicate) {
		return e.byRequest(ctx, in.RequestID)
	}
	if err != nil {
		return Order{}, err
	}

	lab.Emit(ctx, lab.Event{Kind: lab.KindState, To: lab.DB(lab.Order), Label: Pending, OK: true, Order: id})
	lab.Emit(ctx, lab.Event{Kind: lab.KindOutbox, To: lab.DB(lab.Order), Label: "order.pending", OK: true, Order: id})
	wctx, cancel := context.WithTimeout(ctx, waitFor)
	defer cancel()
	if mode == Choreography {
		lab.Emit(ctx, lab.Event{Kind: lab.KindOutbox, To: lab.DB(lab.Order), Label: "order.placed", OK: true, Order: id})
		return e.waitFinished(wctx, id) // nobody to call: the other services react to order.placed
	}
	if err := e.Advance(wctx, id); err != nil && wctx.Err() == nil {
		e.Log.Warn("saga paused; recovery will continue", "order", id, "err", err)
	}
	return e.Get(context.WithoutCancel(ctx), id)
}

var errDuplicate = errors.New("duplicate request")

// Advance runs the order's saga as far as it can go now: until it is terminal, a step fails
// transiently (retry scheduled), ctx ends, or another runner holds the lease.
func (e Engine) Advance(ctx context.Context, id string) error {
	e = e.defaults()
	ctx = lab.WithOrder(ctx, id)
	if !e.acquire(ctx, id) {
		return nil // someone else is on it
	}
	defer e.releaseLease(context.WithoutCancel(ctx), id)
	for ctx.Err() == nil {
		o, err := e.Get(ctx, id)
		if err != nil {
			return err
		}
		if Terminal(o.Status) {
			return nil
		}
		next, reason, err := e.step(ctx, o)
		if err != nil {
			return e.retryLater(context.WithoutCancel(ctx), o, err)
		}
		if err := e.move(ctx, o, next, reason); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// step runs the remote call for the current state and decides the next state.
// A non-nil error means "transient: try this step again later".
func (e Engine) step(ctx context.Context, o Order) (next, reason string, err error) {
	sctx, cancel := context.WithTimeout(ctx, e.StepTimeout)
	defer cancel()
	switch o.Status {
	case Pending:
		lines := make([]Line, 0, len(o.Lines))
		for _, l := range o.Lines {
			lines = append(lines, Line{ProductID: l.ProductID, Qty: l.Qty})
		}
		err := e.Catalog.Reserve(sctx, o.ID, lines)
		switch {
		case err == nil:
			return StockReserved, "", nil
		case errors.Is(err, ErrOutOfStock):
			return Rejected, "out_of_stock: " + cause(err), nil
		case errors.Is(err, ErrReservationGone): // reserved earlier (response lost), expired since
			return Rejected, "stock_reservation_expired", nil
		}
		return "", "", err
	case StockReserved:
		err := e.Payments.Authorize(sctx, o.ID, o.CustomerID, o.Total, o.PaymentMethod)
		switch {
		case err == nil:
			return PaymentAuthorized, "", nil
		case errors.Is(err, ErrDeclined):
			return ReleasingStock, "payment_declined: " + cause(err), nil
		}
		return "", "", err
	case PaymentAuthorized:
		err := e.Payments.Capture(sctx, o.ID)
		switch {
		case err == nil:
			return PaymentCaptured, "", nil
		case errors.Is(err, ErrDeclined):
			return VoidingPayment, "capture_failed: " + cause(err), nil
		}
		return "", "", err
	case PaymentCaptured:
		err := e.Catalog.Commit(sctx, o.ID)
		switch {
		case err == nil:
			return Confirmed, "", nil
		case errors.Is(err, ErrReservationGone):
			return RefundingPayment, "stock_reservation_expired", nil
		}
		return "", "", err
	case VoidingPayment:
		err := e.Payments.Void(sctx, o.ID)
		switch {
		case err == nil:
			return ReleasingStock, o.Reason, nil
		case errors.Is(err, ErrWrongState): // it was captured after all: refund instead
			return RefundingPayment, o.Reason, nil
		}
		return "", "", err
	case RefundingPayment:
		if err := e.Payments.Refund(sctx, o.ID); err != nil {
			return "", "", err
		}
		return ReleasingStock, o.Reason, nil
	case ReleasingStock:
		if err := e.Catalog.Release(sctx, o.ID); err != nil {
			return "", "", err
		}
		return Rejected, o.Reason, nil
	}
	return "", "", fmt.Errorf("order: no step for status %s", o.Status)
}

func cause(err error) string {
	s := err.Error()
	for _, p := range []error{ErrOutOfStock, ErrDeclined} {
		s = strings.TrimPrefix(s, p.Error()+": ")
	}
	return s
}

// retryLater records a transient failure. After MaxAttempts on a forward step it gives up on
// the forward path and starts compensating instead (compensations themselves retry forever,
// with capped backoff, and show up in metrics/alerts via attempts).
func (e Engine) retryLater(ctx context.Context, o Order, cause error) error {
	attempts := o.Attempts + 1
	if attempts >= e.MaxAttempts {
		var comp string
		switch o.Status {
		case Pending:
			comp = ReleasingStock // the reserve may have gone through; release is safe either way
		case StockReserved, PaymentAuthorized:
			comp = VoidingPayment // void also finds an authorization whose response was lost
			// PAYMENT_CAPTURED never gives up: the commit may have gone through with its response
			// lost, and refunding a sale that was booked would be worse than waiting. If the
			// catalog stays down past the reservation's TTL, Commit answers ErrReservationGone
			// and the saga refunds then.
		}
		if comp != "" {
			e.Log.Warn("saga step kept failing; compensating", "order", o.ID, "status", o.Status, "attempts", attempts, "err", cause)
			return e.move(ctx, o, comp, fmt.Sprintf("gave up on %s after %d attempts: %v", strings.ToLower(o.Status), attempts, cause))
		}
	}
	lab.Emit(ctx, lab.Event{Kind: lab.KindRetry, To: lab.DB(lab.Order), Label: o.Status, Detail: truncate(cause.Error(), 200), Order: o.ID})
	delay := e.Backoff << min(attempts-1, 10)
	if delay > time.Minute {
		delay = time.Minute
	}
	delay += time.Duration(rand.Int64N(int64(delay)/4 + 1)) // jitter
	return db.InTenantTx(ctx, e.DB, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE orders.orders SET attempts = $2, last_error = $3,
			next_attempt_at = now() + $4::interval, updated_at = now() WHERE id = $1 AND status = $5`,
			o.ID, attempts, truncate(cause.Error(), 500), delay.String(), o.Status)
		return err
	})
}

// move commits a transition (only from the status we saw) with history and an event.
func (e Engine) move(ctx context.Context, o Order, to, reason string) error {
	moved := false
	err := db.InTenantTx(ctx, e.DB, func(tx pgx.Tx) error {
		moved = false
		tag, err := tx.Exec(ctx, `UPDATE orders.orders SET status = $3, reason = $4, attempts = 0, last_error = '',
			next_attempt_at = CASE WHEN $3 IN ('CONFIRMED', 'REJECTED') THEN NULL ELSE now() END, updated_at = now()
			WHERE id = $1 AND status = $2`, o.ID, o.Status, to, reason)
		if err != nil || tag.RowsAffected() == 0 {
			return err // someone else moved it: the loop re-reads and carries on
		}
		o.Reason = reason
		moved = true
		return emit(ctx, tx, o, o.Status, to, reason)
	})
	if err == nil && moved {
		kind := lab.KindState
		if compensating(to) && !compensating(o.Status) {
			kind = lab.KindCompensate
		}
		lab.Emit(ctx, lab.Event{Kind: kind, To: lab.DB(lab.Order), Label: to, OK: to != Rejected, Detail: reason, Order: o.ID})
		lab.Emit(ctx, lab.Event{Kind: lab.KindOutbox, To: lab.DB(lab.Order), Label: "order." + strings.ToLower(to), OK: true, Order: o.ID})
	}
	return err
}

func compensating(s string) bool {
	return s == VoidingPayment || s == RefundingPayment || s == ReleasingStock
}

func emit(ctx context.Context, tx pgx.Tx, o Order, from, to, note string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO orders.history (order_id, tenant_id, from_status, to_status, note)
		VALUES ($1, current_setting('app.tenant_id')::uuid, $2, $3, $4)`, o.ID, from, to, note); err != nil {
		return err
	}
	return outbox.Enqueue(ctx, tx, "orders.outbox", outbox.Event{Topic: Topic, Key: o.ID, Type: "order." + strings.ToLower(to),
		Payload: map[string]any{"order_id": o.ID, "customer_id": o.CustomerID, "status": to, "previous": from, "reason": note,
			"total": o.Total.Amount, "currency": o.Total.Currency}})
}

func (e Engine) acquire(ctx context.Context, id string) bool {
	var ok bool
	_ = db.InTenantTx(ctx, e.DB, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE orders.orders SET lease_until = now() + $2::interval
			WHERE id = $1 AND (lease_until IS NULL OR lease_until < now())`, id, e.Lease.String())
		ok = err == nil && tag.RowsAffected() == 1
		return err
	})
	return ok
}

func (e Engine) releaseLease(ctx context.Context, id string) {
	_ = db.InTenantTx(ctx, e.DB, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE orders.orders SET lease_until = NULL WHERE id = $1`, id)
		return err
	})
}

// Recover advances every order whose next attempt is due, in all tenants. Run it in a loop
// (RunRecovery); it is what finishes sagas after a crash or a dependency outage.
func (e Engine) Recover(ctx context.Context, max int) (int, error) {
	e = e.defaults()
	type due struct{ tenant, id string }
	var list []due
	err := db.InTx(ctx, e.DB, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id, order_id FROM orders.due_sagas($1)`, max)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d due
			if err := rows.Scan(&d.tenant, &d.id); err != nil {
				return err
			}
			list = append(list, d)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}
	for _, d := range list {
		tctx := tenancy.With(ctx, tenancy.ID(d.tenant))
		if err := e.Advance(tctx, d.id); err != nil && ctx.Err() == nil {
			e.Log.Info("recovery: step still failing", "order", d.id, "err", err)
		}
	}
	return len(list), nil
}

// RunRecovery calls Recover every interval until ctx ends.
func (e Engine) RunRecovery(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := e.Recover(ctx, 50); err != nil && ctx.Err() == nil {
			e.defaults().Log.Warn("recovery", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Get returns an order with its lines.
func (e Engine) Get(ctx context.Context, id string) (Order, error) {
	var o Order
	err := db.InTenantTx(ctx, e.DB, func(tx pgx.Tx) error {
		var err error
		o, err = load(ctx, tx, `id = $1`, id)
		return err
	})
	return o, err
}

func (e Engine) byRequest(ctx context.Context, requestID string) (Order, error) {
	var o Order
	err := db.InTenantTx(ctx, e.DB, func(tx pgx.Tx) error {
		var err error
		o, err = load(ctx, tx, `request_id = $1`, requestID)
		return err
	})
	return o, err
}

// List returns a customer's orders, newest first.
func (e Engine) List(ctx context.Context, customerID string, limit int) ([]Order, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var out []Order
	err := db.InTenantTx(ctx, e.DB, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM orders.orders WHERE customer_id = $1 ORDER BY created_at DESC, id LIMIT $2`, customerID, limit)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		for _, id := range ids {
			o, err := load(ctx, tx, `id = $1`, id)
			if err != nil {
				return err
			}
			out = append(out, o)
		}
		return nil
	})
	return out, err
}

// History returns an order's transitions, oldest first.
func (e Engine) History(ctx context.Context, id string) ([]string, error) {
	var out []string
	err := db.InTenantTx(ctx, e.DB, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT to_status FROM orders.history WHERE order_id = $1 ORDER BY id`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

func load(ctx context.Context, tx pgx.Tx, where string, arg any) (Order, error) {
	var o Order
	var cur string
	err := tx.QueryRow(ctx, `SELECT id, request_id, customer_id, payment_method, status, reason, total_minor, currency,
		attempts, last_error, created_at, updated_at FROM orders.orders WHERE `+where, arg).Scan(
		&o.ID, &o.RequestID, &o.CustomerID, &o.PaymentMethod, &o.Status, &o.Reason, &o.Total.Amount, &cur,
		&o.Attempts, &o.LastError, &o.CreatedAt, &o.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) || db.Code(err) == "22P02" {
		return Order{}, ErrNotFound
	}
	if err != nil {
		return Order{}, err
	}
	o.Total.Currency = strings.TrimSpace(cur)
	rows, err := tx.Query(ctx, `SELECT product_id, sku, name, qty, unit_price_minor FROM orders.lines WHERE order_id = $1 ORDER BY product_id`, o.ID)
	if err != nil {
		return Order{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var l OrderLine
		if err := rows.Scan(&l.ProductID, &l.SKU, &l.Name, &l.Qty, &l.UnitPrice.Amount); err != nil {
			return Order{}, err
		}
		l.UnitPrice.Currency = o.Total.Currency
		o.Lines = append(o.Lines, l)
	}
	return o, rows.Err()
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
