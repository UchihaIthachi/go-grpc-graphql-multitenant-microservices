package e2e

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/bus"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/db"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/money"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/outbox"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/tenancy"
)

// sagaEnv runs catalog, payment and order as libraries, wired the way the binaries wire them:
// outbox relays publish to a bus, and each service consumes from it. Only the transport
// differs (in-memory bus instead of Kafka).
type sagaEnv struct {
	t      *testing.T
	ctx    context.Context
	pg     *testpg.DB
	store  catalog.Store
	pay    payment.Service
	engine order.Engine
	mode   atomic.Value
}

func newSagaEnv(t *testing.T, dup bool, ttl, pspLatency time.Duration) *sagaEnv {
	t.Helper()
	pg := testpg.NewSets(t,
		testpg.Set{FS: catalog.Migrations, Dir: "migrations", Schema: catalog.Schema},
		testpg.Set{FS: ledger.Migrations, Dir: "migrations", Schema: ledger.Schema},
		testpg.Set{FS: payment.Migrations, Dir: "migrations", Schema: payment.Schema},
		testpg.Set{FS: order.Migrations, Dir: "migrations", Schema: order.Schema})
	log := quiet()
	e := &sagaEnv{t: t, pg: pg, ctx: tenancy.With(context.Background(), "11111111-1111-4111-8111-111111111111")}
	e.mode.Store(order.Choreography)

	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payment.WebhookHandler(e.pay, whsec, log).ServeHTTP(w, r)
	}))
	t.Cleanup(hook.Close)
	fp := fakepsp.New(fakepsp.Config{APIKey: "sk", WebhookURL: hook.URL, WebhookSecret: whsec, Latency: pspLatency,
		RefundDelay: 50 * time.Millisecond, Log: log})
	api := httptest.NewServer(fp)
	t.Cleanup(api.Close)
	t.Cleanup(fp.Wait)

	e.store = catalog.Store{DB: pg.App, TTL: ttl}
	e.pay = payment.Service{DB: pg.App, PSP: psp.HTTPClient{BaseURL: api.URL, APIKey: "sk"}}
	e.engine = order.Engine{DB: pg.App, Log: log, Backoff: 10 * time.Millisecond,
		Catalog: local.Catalog{Store: e.store}, Payments: local.Payments{Service: e.pay},
		Mode: func() string { return e.mode.Load().(string) }}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	b := &bus.Memory{Duplicate: dup}
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	for _, table := range []string{"orders.outbox", "payment.outbox", "catalog.outbox"} {
		r := outbox.Relay{DB: pg.App, Table: table, Pub: b, Interval: 20 * time.Millisecond, Log: log}
		run(func() { r.Run(ctx) })
	}
	opts := func(svc string) bus.Options {
		return bus.Options{Service: svc, Backoff: 20 * time.Millisecond, Log: log}
	}
	run(func() {
		_ = bus.Consume(ctx, b, catalog.ConsumerGroup, catalog.ConsumeTopics, e.store.HandleEvent, opts("catalog"))
	})
	run(func() {
		_ = bus.Consume(ctx, b, payment.ConsumerGroup, payment.ConsumeTopics, e.pay.HandleEvent, opts("payment"))
	})
	run(func() {
		_ = bus.Consume(ctx, b, order.ConsumerGroup, order.ConsumeTopics, e.engine.HandleEvent, opts("order"))
	})
	run(func() { e.engine.RunRecovery(ctx, 50*time.Millisecond) })
	return e
}

func (e *sagaEnv) product(sku string, price int64, stock int) string {
	e.t.Helper()
	p, err := e.store.CreateProduct(e.ctx, sku, sku, "", money.MustNew(price, "USD"), stock)
	if err != nil {
		e.t.Fatal(err)
	}
	return p.ID
}

func (e *sagaEnv) place(req, pm string, lines ...order.Line) order.Order {
	e.t.Helper()
	o, err := e.engine.Place(e.ctx, order.PlaceInput{RequestID: req, CustomerID: "cus_1", PaymentMethod: pm, Lines: lines}, 10*time.Second)
	if err != nil {
		e.t.Fatalf("place: %v", err)
	}
	return o
}

func (e *sagaEnv) finished(ids ...string) []order.Order {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var out []order.Order
		done := true
		for _, id := range ids {
			o, err := e.engine.Get(e.ctx, id)
			if err != nil {
				e.t.Fatal(err)
			}
			done = done && order.Terminal(o.Status)
			out = append(out, o)
		}
		if done {
			return out
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("orders did not finish: %+v", out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *sagaEnv) stock(id string) (onHand, reserved int) {
	e.t.Helper()
	ps, err := e.store.Products(e.ctx, []string{id})
	if err != nil {
		e.t.Fatal(err)
	}
	return ps[0].OnHand, ps[0].Reserved
}

func (e *sagaEnv) payStatus(orderID string) string {
	it, err := e.pay.Get(e.ctx, orderID)
	if errors.Is(err, payment.ErrNotFound) {
		return "none"
	}
	if err != nil {
		e.t.Fatal(err)
	}
	return it.Status
}

func (e *sagaEnv) mustEq(what string, got, want any) {
	e.t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		e.t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func ln(id string, q int) order.Line { return order.Line{ProductID: id, Qty: q} }

func TestChoreographyOutcomes(t *testing.T) {
	e := newSagaEnv(t, false, 0, 0)
	mug := e.product("MUG", 1000, 5)

	ok := e.finished(e.place("ok", "pm_ok", ln(mug, 2)).ID)[0]
	e.mustEq("confirmed", ok.Status, order.Confirmed)
	e.mustEq("payment", e.payStatus(ok.ID), payment.Captured)

	dec := e.finished(e.place("dec", "pm_insufficient", ln(mug, 1)).ID)[0]
	e.mustEq("declined", dec.Status+" "+dec.Reason, "REJECTED payment_declined: insufficient_funds")

	cap := e.finished(e.place("cap", "pm_expire_auth", ln(mug, 1)).ID)[0]
	if cap.Status != order.Rejected || !strings.HasPrefix(cap.Reason, "capture_failed") {
		t.Fatalf("capture failure: %s %q", cap.Status, cap.Reason)
	}
	e.mustEq("voided", e.payStatus(cap.ID), payment.Voided)

	oos := e.finished(e.place("oos", "pm_ok", ln(mug, 9)).ID)[0]
	if oos.Status != order.Rejected || !strings.HasPrefix(oos.Reason, "out_of_stock") {
		t.Fatalf("out of stock: %s %q", oos.Status, oos.Reason)
	}
	e.mustEq("no payment for out of stock", e.payStatus(oos.ID), "none")

	onHand, reserved := e.stock(mug)
	e.mustEq("stock", [2]int{onHand, reserved}, [2]int{3, 0})
}

func TestChoreographyRefundsWhenStockHoldExpires(t *testing.T) {
	// processor slower than the stock hold: payment captures after the reservation expired
	e := newSagaEnv(t, false, 300*time.Millisecond, 250*time.Millisecond)
	mug := e.product("MUG", 1000, 5)
	o := e.finished(e.place("slow", "pm_ok", ln(mug, 1)).ID)[0]
	e.mustEq("rejected", o.Status+" "+o.Reason, "REJECTED stock_reservation_expired")
	deadline := time.Now().Add(5 * time.Second)
	for e.payStatus(o.ID) != payment.Refunded && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	e.mustEq("refunded", e.payStatus(o.ID), payment.Refunded)
	_, reserved := e.stock(mug)
	e.mustEq("released", reserved, 0)
}

// Every event delivered twice, both saga styles running side by side: every order still ends
// with stock, payments and the ledger agreeing.
func TestBothStylesWithDuplicateDelivery(t *testing.T) {
	e := newSagaEnv(t, true, 0, 0)
	mug := e.product("MUG", 1000, 12)
	pms := []string{"pm_ok", "pm_ok", "pm_decline", "pm_expire_auth", "pm_ok"}
	var ids []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mode := order.Orchestration
			if i%2 == 0 {
				mode = order.Choreography
			}
			// the mode switch is process-wide; place each order under its own choice
			mu.Lock()
			e.mode.Store(mode)
			o, err := e.engine.Place(e.ctx, order.PlaceInput{RequestID: fmt.Sprint("r", i), CustomerID: "c",
				PaymentMethod: pms[i%len(pms)], Lines: []order.Line{ln(mug, 1)}}, 0)
			mu.Unlock()
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			ids = append(ids, o.ID)
			mu.Unlock()
		}()
	}
	wg.Wait()
	orders := e.finished(ids...)

	confirmed := 0
	modes := map[string]int{}
	for _, o := range orders {
		var mode string
		_ = db.InTenantTx(e.ctx, e.pg.App, func(tx pgx.Tx) error {
			return tx.QueryRow(e.ctx, `SELECT saga_mode FROM orders.orders WHERE id = $1`, o.ID).Scan(&mode)
		})
		modes[mode]++
		st := e.payStatus(o.ID)
		switch o.Status {
		case order.Confirmed:
			confirmed++
			e.mustEq("confirmed "+mode, st, payment.Captured)
		case order.Rejected:
			if st == payment.Authorized || st == payment.Captured {
				t.Fatalf("%s order %s rejected (%s) but payment %s", mode, o.ID, o.Reason, st)
			}
		}
	}
	if modes[order.Choreography] == 0 || modes[order.Orchestration] == 0 {
		t.Fatalf("both styles should have run: %v", modes)
	}
	onHand, reserved := e.stock(mug)
	e.mustEq("reserved", reserved, 0)
	e.mustEq("on hand", onHand, 12-confirmed)
	var sales int64
	_ = db.InTenantTx(e.ctx, e.pg.App, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT coalesce(-sum(balance_minor), 0) FROM ledger.accounts WHERE code = 'sales'`).Scan(&sales)
	})
	e.mustEq("ledger sales", sales, int64(confirmed)*1000)
	t.Logf("%d confirmed; modes %v", confirmed, modes)
}
