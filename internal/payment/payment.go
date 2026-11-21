// Package payment owns payment intents: authorize, capture, void and refund one order's payment
// through a card processor, and record every money movement in the double-entry ledger in the
// same transaction as the state change.
//
// Each method is safe to call again with the same order id: that is what the order saga does
// after a timeout or a crash. Processor calls carry idempotency keys derived from the intent id,
// and state transitions are conditional updates, so retries never charge twice.
package payment

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/payment/ledger"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/payment/psp"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/db"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/money"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/outbox"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/tenancy"
)

//go:embed migrations/*.sql
var Migrations embed.FS

// Schema of the payment tables (the ledger has its own schema in the same database).
const Schema = "payment"

// Topic carries payment.* events.
const Topic = "payment.events"

// Statuses.
const (
	Pending       = "pending"
	Authorized    = "authorized"
	Captured      = "captured"
	Voided        = "voided"
	RefundPending = "refund_pending"
	Refunded      = "refunded"
	Failed        = "failed"
)

var (
	ErrNotFound     = errors.New("payment: not found")
	ErrConflict     = errors.New("payment: order already has a payment with different details")
	ErrInvalidState = errors.New("payment: not allowed in the current state")
	ErrDeclined     = errors.New("payment: declined") // permanent; wraps the processor's reason
	ErrUnavailable  = errors.New("payment: processor unavailable, retry")
	ErrInvalid      = errors.New("payment: invalid request")
)

// Ledger account codes, one set per tenant and currency.
const (
	AccReceivable = "psp_receivable" // asset: what the processor owes us
	AccSales      = "sales"          // revenue
	AccFees       = "psp_fees"       // expense
)

// Intent is one order's payment.
type Intent struct {
	ID            string
	OrderID       string
	CustomerID    string
	Amount        money.Money
	PaymentMethod string
	Status        string
	ChargeID      string
	RefundID      string
	Fee           int64
	RefundAttempt int
	FailureReason string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Service implements the payment operations.
type Service struct {
	DB  db.Beginner
	PSP psp.Client
}

// AuthorizeInput describes the payment to hold on the customer's card.
type AuthorizeInput struct {
	OrderID       string
	CustomerID    string
	Amount        money.Money
	PaymentMethod string
}

// Authorize holds the amount on the card. Calling it again for the same order returns the
// current intent; the processor is only called while the intent is still pending.
// A declined card returns the failed intent and ErrDeclined.
func (s Service) Authorize(ctx context.Context, in AuthorizeInput) (Intent, error) {
	if in.OrderID == "" || in.CustomerID == "" || in.PaymentMethod == "" || in.Amount.Amount <= 0 {
		return Intent{}, fmt.Errorf("%w: order, customer, payment method and a positive amount are required", ErrInvalid)
	}
	tid, err := tenancy.From(ctx)
	if err != nil {
		return Intent{}, err
	}
	var it Intent
	err = db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO payment.intents (tenant_id, order_id, customer_id, amount_minor, currency, payment_method)
			VALUES (current_setting('app.tenant_id')::uuid, $1, $2, $3, $4, $5)
			ON CONFLICT (tenant_id, order_id) DO NOTHING`,
			in.OrderID, in.CustomerID, in.Amount.Amount, in.Amount.Currency, in.PaymentMethod); err != nil {
			return err
		}
		it, err = get(ctx, tx, in.OrderID)
		return err
	})
	if err != nil {
		return Intent{}, err
	}
	if it.Amount != in.Amount || it.CustomerID != in.CustomerID {
		return it, ErrConflict
	}
	if it.Status != Pending {
		if it.Status == Failed {
			return it, fmt.Errorf("%w: %s", ErrDeclined, it.FailureReason)
		}
		return it, nil
	}

	ch, perr := s.PSP.Authorize(ctx, psp.AuthorizeRequest{
		IdempotencyKey: "auth:" + it.ID, Amount: in.Amount.Amount, Currency: in.Amount.Currency,
		PaymentMethod: in.PaymentMethod,
		Metadata:      map[string]string{"tenant_id": string(tid), "order_id": in.OrderID, "intent_id": it.ID},
	})
	switch {
	case errors.Is(perr, psp.ErrDeclined):
		reason := ch.DeclineReason
		if reason == "" {
			reason = strings.TrimPrefix(perr.Error(), psp.ErrDeclined.Error()+": ")
		}
		it, err = s.transition(ctx, it.OrderID, Pending, Failed, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment.intents SET failure_reason = $2, psp_charge_id = NULLIF($3, '') WHERE order_id = $1`, it.OrderID, reason, ch.ID)
			return err
		}, "payment.failed")
		if err != nil {
			return it, err
		}
		return it, fmt.Errorf("%w: %s", ErrDeclined, reason)
	case perr != nil:
		return it, fmt.Errorf("%w: %v", ErrUnavailable, perr)
	}
	return s.transition(ctx, it.OrderID, Pending, Authorized, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment.intents SET psp_charge_id = $2 WHERE order_id = $1`, it.OrderID, ch.ID)
		return err
	}, "payment.authorized")
}

// Capture takes the authorized money and books it: receivable from the processor up, sales up,
// processor fee booked as an expense. Ledger entry and status change commit together.
func (s Service) Capture(ctx context.Context, orderID string) (Intent, error) {
	it, err := s.Get(ctx, orderID)
	if err != nil {
		return Intent{}, err
	}
	switch it.Status {
	case Captured, RefundPending, Refunded:
		return it, nil
	case Authorized:
	default:
		return it, fmt.Errorf("%w: capture a %s payment", ErrInvalidState, it.Status)
	}
	ch, perr := s.PSP.Capture(ctx, it.ChargeID, "capture:"+it.ID, it.Amount.Amount)
	if errors.Is(perr, psp.ErrDeclined) {
		return it, fmt.Errorf("%w: %v", ErrDeclined, perr)
	}
	if perr != nil {
		return it, fmt.Errorf("%w: %v", ErrUnavailable, perr)
	}
	return s.transition(ctx, orderID, Authorized, Captured, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE payment.intents SET fee_minor = $2 WHERE order_id = $1`, orderID, ch.Fee); err != nil {
			return err
		}
		cur := it.Amount.Currency
		acc, err := accounts(ctx, tx, cur)
		if err != nil {
			return err
		}
		ps := []ledger.Posting{
			{AccountID: acc[AccReceivable], Amount: money.Money{Amount: it.Amount.Amount, Currency: cur}},
			{AccountID: acc[AccSales], Amount: money.Money{Amount: -it.Amount.Amount, Currency: cur}},
		}
		if ch.Fee > 0 {
			ps = append(ps,
				ledger.Posting{AccountID: acc[AccFees], Amount: money.Money{Amount: ch.Fee, Currency: cur}},
				ledger.Posting{AccountID: acc[AccReceivable], Amount: money.Money{Amount: -ch.Fee, Currency: cur}})
		}
		_, _, err = ledger.PostTx(ctx, tx, ledger.EntryRequest{
			IdempotencyKey: "capture:" + it.ID, Kind: "capture", Memo: "charge:" + it.ChargeID, Postings: ps})
		return err
	}, "payment.captured")
}

// Void releases an authorization that will not be captured. A pending intent may have been
// authorized by an attempt whose response was lost, so the processor is asked first.
func (s Service) Void(ctx context.Context, orderID string) (Intent, error) {
	it, err := s.Get(ctx, orderID)
	if err != nil {
		return Intent{}, err
	}
	switch it.Status {
	case Voided, Failed:
		return it, nil
	case Pending:
		ch, perr := s.PSP.ChargeByKey(ctx, "auth:"+it.ID)
		if errors.Is(perr, psp.ErrNotFound) {
			return s.transition(ctx, orderID, Pending, Voided, nil, "payment.voided") // never reached the processor
		}
		if perr != nil {
			return it, fmt.Errorf("%w: %v", ErrUnavailable, perr)
		}
		if ch.Status == psp.Authorized { // adopt the lost authorization, then void it below
			if it, err = s.transition(ctx, orderID, Pending, Authorized, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE payment.intents SET psp_charge_id = $2 WHERE order_id = $1`, orderID, ch.ID)
				return err
			}, "payment.authorized"); err != nil {
				return it, err
			}
		} else {
			return s.transition(ctx, orderID, Pending, Voided, nil, "payment.voided")
		}
	case Authorized:
	default:
		return it, fmt.Errorf("%w: void a %s payment (refund it instead)", ErrInvalidState, it.Status)
	}
	if _, perr := s.PSP.Void(ctx, it.ChargeID, "void:"+it.ID); perr != nil {
		return it, fmt.Errorf("%w: %v", ErrUnavailable, perr)
	}
	return s.transition(ctx, orderID, Authorized, Voided, nil, "payment.voided")
}

// Refund asks the processor to return a captured payment. The intent moves to refund_pending
// BEFORE the processor is called, so the processor's webhook (refund.succeeded / refund.failed,
// handled by HandleWebhook) always finds it there, however fast it arrives. Calling Refund again
// while pending re-sends the same request (same idempotency key); after a failed refund a new
// attempt gets a new key.
func (s Service) Refund(ctx context.Context, orderID string) (Intent, error) {
	it, err := s.Get(ctx, orderID)
	if err != nil {
		return Intent{}, err
	}
	switch it.Status {
	case Refunded:
		return it, nil
	case RefundPending:
		if it.RefundID != "" {
			return it, nil
		}
	case Captured:
		if it, err = s.transition(ctx, orderID, Captured, RefundPending, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment.intents SET refund_attempts = refund_attempts + 1,
				psp_refund_id = NULL, failure_reason = '' WHERE order_id = $1`, orderID)
			return err
		}, "payment.refund_requested"); err != nil {
			return it, err
		}
		if it.Status != RefundPending || it.RefundID != "" {
			return it, nil // a concurrent call got there first
		}
	default:
		return it, fmt.Errorf("%w: refund a %s payment", ErrInvalidState, it.Status)
	}
	rf, perr := s.PSP.Refund(ctx, it.ChargeID, fmt.Sprintf("refund:%s:%d", it.ID, it.RefundAttempt), it.Amount.Amount)
	if perr != nil {
		return it, fmt.Errorf("%w: %v", ErrUnavailable, perr) // stays refund_pending; retry re-sends
	}
	err = db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE payment.intents SET psp_refund_id = $2
			WHERE order_id = $1 AND psp_refund_id IS NULL`, orderID, rf.ID); err != nil {
			return err
		}
		it, err = get(ctx, tx, orderID)
		return err
	})
	return it, err
}

// Get returns an order's payment.
func (s Service) Get(ctx context.Context, orderID string) (Intent, error) {
	var it Intent
	err := db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		var err error
		it, err = get(ctx, tx, orderID)
		return err
	})
	return it, err
}

// transition moves the intent from → to (only if it is still in from), runs extra in the same
// transaction, and enqueues an event. If another caller already moved it, it returns the
// intent as it is now, so concurrent retries converge.
func (s Service) transition(ctx context.Context, orderID, from, to string, extra func(pgx.Tx) error, event string) (Intent, error) {
	var it Intent
	err := db.InTenantTx(ctx, s.DB, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE payment.intents SET status = $3, updated_at = now() WHERE order_id = $1 AND status = $2`, orderID, from, to)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			if extra != nil {
				if err := extra(tx); err != nil {
					return err
				}
			}
		}
		if it, err = get(ctx, tx, orderID); err != nil {
			return err
		}
		if tag.RowsAffected() == 1 && event != "" {
			return outbox.Enqueue(ctx, tx, "payment.outbox", outbox.Event{Topic: Topic, Key: orderID, Type: event, Payload: eventPayload(it)})
		}
		return nil
	})
	return it, err
}

func eventPayload(it Intent) map[string]any {
	return map[string]any{
		"order_id": it.OrderID, "payment_id": it.ID, "status": it.Status,
		"amount": it.Amount.Amount, "currency": it.Amount.Currency, "reason": it.FailureReason,
	}
}

func get(ctx context.Context, tx pgx.Tx, orderID string) (Intent, error) {
	var it Intent
	var cur string
	var charge, refund *string
	err := tx.QueryRow(ctx, `SELECT id, order_id, customer_id, amount_minor, currency, payment_method, status,
		psp_charge_id, psp_refund_id, fee_minor, refund_attempts, failure_reason, created_at, updated_at
		FROM payment.intents WHERE order_id = $1`, orderID).Scan(&it.ID, &it.OrderID, &it.CustomerID, &it.Amount.Amount, &cur,
		&it.PaymentMethod, &it.Status, &charge, &refund, &it.Fee, &it.RefundAttempt, &it.FailureReason, &it.CreatedAt, &it.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Intent{}, ErrNotFound
	}
	it.Amount.Currency = strings.TrimSpace(cur)
	if charge != nil {
		it.ChargeID = *charge
	}
	if refund != nil {
		it.RefundID = *refund
	}
	return it, err
}

// accounts returns the tenant's ledger account ids for a currency, creating them on first use.
func accounts(ctx context.Context, tx pgx.Tx, currency string) (map[string]string, error) {
	out := map[string]string{}
	for code, kind := range map[string]string{AccReceivable: ledger.Asset, AccSales: ledger.Revenue, AccFees: ledger.Expense} {
		a, err := ledger.EnsureAccountTx(ctx, tx, code, currency, kind, true)
		if err != nil {
			return nil, err
		}
		out[code] = a.ID
	}
	return out, nil
}
