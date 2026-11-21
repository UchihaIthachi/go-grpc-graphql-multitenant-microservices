package order_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/catalog"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/fakepsp"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/order"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/order/local"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/payment"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/payment/ledger"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/payment/psp"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/testpg"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/db"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/money"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/tenancy"
)

const (
	tenantA = tenancy.ID("11111111-1111-4111-8111-111111111111")
	tenantB = tenancy.ID("22222222-2222-4222-8222-222222222222")
	secret  = "whsec_test"
)

// env wires the real catalog and payment code (and the fake processor) into one database,
// so a saga runs against everything except the network between services.
type env struct {
	t       *testing.T
	ctx     context.Context
	pg      *testpg.DB
	store   catalog.Store
	pay     payment.Service
	psp     *fakepsp.Server
	engine  order.Engine
	catalog order.Catalog
	payment order.Payments
}

func setup(t *testing.T, cfg fakepsp.Config, ttl time.Duration) *env {
	t.Helper()
	pg := testpg.NewSets(t,
		testpg.Set{FS: ledger.Migrations, Dir: "migrations", Schema: ledger.Schema},
		testpg.Set{FS: payment.Migrations, Dir: "migrations", Schema: payment.Schema},
		testpg.Set{FS: catalog.Migrations, Dir: "migrations", Schema: catalog.Schema},
		testpg.Set{FS: order.Migrations, Dir: "migrations", Schema: order.Schema})
	e := &env{t: t, pg: pg, ctx: tenancy.With(context.Background(), tenantA)}
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payment.WebhookHandler(e.pay, secret, slog.Default()).ServeHTTP(w, r)
	}))
	t.Cleanup(hook.Close)
	cfg.APIKey, cfg.WebhookURL, cfg.WebhookSecret = "sk_test", hook.URL, secret
	e.psp = fakepsp.New(cfg)
	api := httptest.NewServer(e.psp)
	t.Cleanup(api.Close)
	t.Cleanup(e.psp.Wait)
	e.store = catalog.Store{DB: pg.App, TTL: ttl}
	e.pay = payment.Service{DB: pg.App, PSP: psp.HTTPClient{BaseURL: api.URL, APIKey: "sk_test"}}
	e.catalog = local.Catalog{Store: e.store}
	e.payment = local.Payments{Service: e.pay}
	e.engine = order.Engine{DB: pg.App, Catalog: e.catalog, Payments: e.payment,
		Backoff: 5 * time.Millisecond, StepTimeout: 3 * time.Second, Lease: 2 * time.Second, MaxAttempts: 4}
	return e
}

func (e *env) product(sku string, price int64, stock int) string {
	e.t.Helper()
	p, err := e.store.CreateProduct(e.ctx, sku, "Product "+sku, "", money.MustNew(price, "USD"), stock)
	if err != nil {
		e.t.Fatal(err)
	}
	return p.ID
}

func (e *env) place(req, pm string, lines ...order.Line) order.Order {
	e.t.Helper()
	o, err := e.engine.Place(e.ctx, order.PlaceInput{RequestID: req, CustomerID: "cus_1", PaymentMethod: pm, Lines: lines}, 10*time.Second)
	if err != nil {
		e.t.Fatalf("place %s: %v", req, err)
	}
	return o
}

func (e *env) stock(id string) (onHand, reserved int) {
	e.t.Helper()
	ps, err := e.store.Products(e.ctx, []string{id})
	if err != nil || len(ps) != 1 {
		e.t.Fatalf("product %s: %v", id, err)
	}
	return ps[0].OnHand, ps[0].Reserved
}

func (e *env) history(id string) []string {
	e.t.Helper()
	h, err := e.engine.History(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return h
}

func (e *env) paymentStatus(orderID string) string {
	e.t.Helper()
	it, err := e.pay.Get(e.ctx, orderID)
	if errors.Is(err, payment.ErrNotFound) {
		return "none"
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return it.Status
}

// recoverAll runs the recovery loop until every order is terminal (or fails the test).
func (e *env) recoverAll(ids ...string) {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := e.engine.Recover(context.Background(), 100); err != nil {
			e.t.Fatal(err)
		}
		done := true
		for _, id := range ids {
			o, err := e.engine.Get(e.ctx, id)
			if err != nil {
				e.t.Fatal(err)
			}
			done = done && order.Terminal(o.Status)
		}
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatal("orders did not finish")
}

func line(id string, qty int) order.Line { return order.Line{ProductID: id, Qty: qty} }

func eq(t *testing.T, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestHappyPath(t *testing.T) {
	e := setup(t, fakepsp.Config{}, 0)
	mug, tee := e.product("MUG", 1250, 5), e.product("TEE", 2000, 3)
	o := e.place("r1", "pm_ok", line(mug, 2), line(tee, 1), line(mug, 1))

	eq(t, o.Status, order.Confirmed)
	eq(t, o.Total, money.MustNew(3*1250+2000, "USD"))
	eq(t, len(o.Lines), 2) // mug lines merged
	eq(t, e.history(o.ID), []string{order.Pending, order.StockReserved, order.PaymentAuthorized, order.PaymentCaptured, order.Confirmed})
	onHand, reserved := e.stock(mug)
	eq(t, [2]int{onHand, reserved}, [2]int{2, 0})
	eq(t, e.paymentStatus(o.ID), payment.Captured)

	// every transition produced exactly one event
	var events int
	if err := e.pg.Owner.QueryRow(e.ctx, `SELECT count(*) FROM orders.outbox WHERE key = $1`, o.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	eq(t, events, 5)
}

func TestPlaceIsIdempotent(t *testing.T) {
	e := setup(t, fakepsp.Config{}, 0)
	mug := e.product("MUG", 1250, 5)
	a := e.place("same", "pm_ok", line(mug, 1))
	b := e.place("same", "pm_ok", line(mug, 1))
	eq(t, a.ID, b.ID)
	onHand, _ := e.stock(mug)
	eq(t, onHand, 4)
	eq(t, e.psp.Charges(), 1)
}

func TestOutOfStockIsRejectedWithoutTouchingPayment(t *testing.T) {
	e := setup(t, fakepsp.Config{}, 0)
	mug, tee := e.product("MUG", 1250, 5), e.product("TEE", 2000, 1)
	o := e.place("r1", "pm_ok", line(mug, 1), line(tee, 2))
	eq(t, o.Status, order.Rejected)
	eq(t, e.history(o.ID), []string{order.Pending, order.Rejected})
	_, reserved := e.stock(mug)
	eq(t, reserved, 0) // all lines or none
	eq(t, e.paymentStatus(o.ID), "none")
}

func TestDeclinedCardReleasesStock(t *testing.T) {
	e := setup(t, fakepsp.Config{}, 0)
	mug := e.product("MUG", 1250, 5)
	o := e.place("r1", "pm_insufficient", line(mug, 3))
	eq(t, o.Status, order.Rejected)
	eq(t, o.Reason, "payment_declined: insufficient_funds")
	eq(t, e.history(o.ID), []string{order.Pending, order.StockReserved, order.ReleasingStock, order.Rejected})
	onHand, reserved := e.stock(mug)
	eq(t, [2]int{onHand, reserved}, [2]int{5, 0})
}

func TestFailedCaptureVoidsAndReleases(t *testing.T) {
	e := setup(t, fakepsp.Config{}, 0)
	mug := e.product("MUG", 1250, 5)
	o := e.place("r1", "pm_expire_auth", line(mug, 1))
	eq(t, o.Status, order.Rejected)
	eq(t, e.history(o.ID), []string{order.Pending, order.StockReserved, order.PaymentAuthorized,
		order.VoidingPayment, order.ReleasingStock, order.Rejected})
	eq(t, e.paymentStatus(o.ID), payment.Voided)
	_, reserved := e.stock(mug)
	eq(t, reserved, 0)
}

// slowCapture holds the capture until the stock reservation has expired.
type slowCapture struct {
	order.Payments
	d time.Duration
}

func (s slowCapture) Capture(ctx context.Context, id string) error {
	time.Sleep(s.d)
	return s.Payments.Capture(ctx, id)
}

func TestExpiredReservationRefundsTheCapturedPayment(t *testing.T) {
	e := setup(t, fakepsp.Config{}, 300*time.Millisecond)
	mug := e.product("MUG", 1250, 5)
	e.engine.Payments = slowCapture{e.payment, 500 * time.Millisecond}
	o := e.place("r1", "pm_ok", line(mug, 2))
	eq(t, o.Status, order.Rejected)
	eq(t, o.Reason, "stock_reservation_expired")
	eq(t, e.history(o.ID), []string{order.Pending, order.StockReserved, order.PaymentAuthorized, order.PaymentCaptured,
		order.RefundingPayment, order.ReleasingStock, order.Rejected})
	e.psp.Wait() // refund.succeeded webhook
	eq(t, e.paymentStatus(o.ID), payment.Refunded)
	onHand, reserved := e.stock(mug)
	eq(t, [2]int{onHand, reserved}, [2]int{5, 0})
}

// flaky fails the first n calls of one step with a transient error.
type flaky struct {
	order.Payments
	n *atomic.Int32
}

func (f flaky) Authorize(ctx context.Context, id, cus string, amt money.Money, pm string) error {
	if f.n.Add(-1) >= 0 {
		return errors.New("payment service unavailable")
	}
	return f.Payments.Authorize(ctx, id, cus, amt, pm)
}

func TestTransientFailureIsRetriedByRecovery(t *testing.T) {
	e := setup(t, fakepsp.Config{}, 0)
	mug := e.product("MUG", 1250, 5)
	n := &atomic.Int32{}
	n.Store(2)
	e.engine.Payments = flaky{e.payment, n}
	e.engine.Backoff = 50 * time.Millisecond

	o, err := e.engine.Place(e.ctx, order.PlaceInput{RequestID: "r1", CustomerID: "cus_1", PaymentMethod: "pm_ok",
		Lines: []order.Line{line(mug, 1)}}, 0) // don't wait: the request returns immediately
	if err != nil {
		t.Fatal(err)
	}
	if order.Terminal(o.Status) {
		t.Fatalf("expected an unfinished order, got %s", o.Status)
	}
	e.recoverAll(o.ID)
	o, _ = e.engine.Get(e.ctx, o.ID)
	eq(t, o.Status, order.Confirmed)
	eq(t, o.Attempts, 0)
}

func TestGivingUpCompensates(t *testing.T) {
	e := setup(t, fakepsp.Config{}, 0)
	mug := e.product("MUG", 1250, 5)
	n := &atomic.Int32{}
	n.Store(1 << 30)
	e.engine.Payments = flaky{e.payment, n} // payments never answer
	o := e.place("r1", "pm_ok", line(mug, 1))
	e.recoverAll(o.ID)
	o, _ = e.engine.Get(e.ctx, o.ID)
	eq(t, o.Status, order.Rejected)
	eq(t, o.Reason, "gave up on stock_reserved after 4 attempts: payment service unavailable")
	eq(t, e.history(o.ID), []string{order.Pending, order.StockReserved, order.VoidingPayment, order.ReleasingStock, order.Rejected})
	_, reserved := e.stock(mug)
	eq(t, reserved, 0)
}

func TestCrashedRunnerIsRecoveredAfterLease(t *testing.T) {
	e := setup(t, fakepsp.Config{}, 0)
	mug := e.product("MUG", 1250, 5)
	// simulate a process that stored the order, took the lease and died
	o, err := e.engine.Place(e.ctx, order.PlaceInput{RequestID: "r1", CustomerID: "cus_1", PaymentMethod: "pm_ok",
		Lines: []order.Line{line(mug, 1)}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pg.Owner.Exec(e.ctx, `UPDATE orders.orders SET lease_until = now() + interval '300 ms' WHERE id = $1`, o.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.engine.Recover(context.Background(), 10); n != 0 {
		t.Fatalf("recovery took a leased order")
	}
	time.Sleep(350 * time.Millisecond)
	e.recoverAll(o.ID)
	o, _ = e.engine.Get(e.ctx, o.ID)
	eq(t, o.Status, order.Confirmed)
}

func TestNoOversellUnderConcurrency(t *testing.T) {
	e := setup(t, fakepsp.Config{}, 0)
	mug := e.product("MUG", 1250, 10)
	var wg sync.WaitGroup
	ids := make([]string, 30)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids[i] = e.place(fmt.Sprintf("r%d", i), "pm_ok", line(mug, 1)).ID
		}()
	}
	wg.Wait()
	e.recoverAll(ids...)
	confirmed := 0
	for _, id := range ids {
		o, _ := e.engine.Get(e.ctx, id)
		if o.Status == order.Confirmed {
			confirmed++
		}
	}
	eq(t, confirmed, 10)
	onHand, reserved := e.stock(mug)
	eq(t, [2]int{onHand, reserved}, [2]int{0, 0})
}

// With the processor failing every third request, every order still ends up either confirmed
// with its money booked once, or rejected with its stock back and nothing charged.
func TestChaosKeepsBooksAndStockConsistent(t *testing.T) {
	e := setup(t, fakepsp.Config{FailEvery: 3, DuplicateWebhooks: true}, 0)
	mug := e.product("MUG", 1000, 8)
	e.engine.MaxAttempts = 3
	pms := []string{"pm_ok", "pm_ok", "pm_decline", "pm_expire_auth", "pm_ok"}
	var ids []string
	for i := 0; i < 15; i++ {
		ids = append(ids, e.place(fmt.Sprintf("r%d", i), pms[i%len(pms)], line(mug, 1)).ID)
	}
	e.recoverAll(ids...)
	e.psp.Wait()

	confirmed := 0
	for _, id := range ids {
		o, _ := e.engine.Get(e.ctx, id)
		st := e.paymentStatus(id)
		switch o.Status {
		case order.Confirmed:
			confirmed++
			eq(t, st, payment.Captured)
		case order.Rejected:
			if st == payment.Captured || st == payment.Authorized || st == payment.Pending {
				t.Fatalf("rejected order %s left payment %s (%s)", id, st, o.Reason)
			}
		}
	}
	onHand, reserved := e.stock(mug)
	eq(t, reserved, 0)
	eq(t, onHand, 8-confirmed)

	var sales int64
	_ = db.InTenantTx(e.ctx, e.pg.App, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT coalesce(-sum(balance_minor), 0) FROM ledger.accounts WHERE code = 'sales'`).Scan(&sales)
	})
	eq(t, sales, int64(confirmed)*1000)
	t.Logf("confirmed %d of %d with the processor failing every 3rd call", confirmed, len(ids))
}

func TestTenantIsolation(t *testing.T) {
	e := setup(t, fakepsp.Config{}, 0)
	mug := e.product("MUG", 1250, 5)
	o := e.place("r1", "pm_ok", line(mug, 1))
	ctxB := tenancy.With(context.Background(), tenantB)
	if _, err := e.engine.Get(ctxB, o.ID); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("tenant B read tenant A's order: %v", err)
	}
	list, err := e.engine.List(ctxB, "cus_1", 10)
	eq(t, err, nil)
	eq(t, len(list), 0)
	// and B cannot order A's products
	_, err = e.engine.Place(ctxB, order.PlaceInput{RequestID: "r1", CustomerID: "cus_1", PaymentMethod: "pm_ok",
		Lines: []order.Line{line(mug, 1)}}, time.Second)
	if !errors.Is(err, order.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

func TestValidation(t *testing.T) {
	e := setup(t, fakepsp.Config{}, 0)
	mug := e.product("MUG", 1250, 5)
	for name, in := range map[string]order.PlaceInput{
		"no lines":    {RequestID: "r", CustomerID: "c", PaymentMethod: "pm_ok"},
		"zero qty":    {RequestID: "r", CustomerID: "c", PaymentMethod: "pm_ok", Lines: []order.Line{line(mug, 0)}},
		"no request":  {CustomerID: "c", PaymentMethod: "pm_ok", Lines: []order.Line{line(mug, 1)}},
		"bad product": {RequestID: "r", CustomerID: "c", PaymentMethod: "pm_ok", Lines: []order.Line{line("00000000-0000-4000-8000-000000000000", 1)}},
		"not a uuid":  {RequestID: "r", CustomerID: "c", PaymentMethod: "pm_ok", Lines: []order.Line{line("nope", 1)}},
	} {
		if _, err := e.engine.Place(e.ctx, in, time.Second); !errors.Is(err, order.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}
