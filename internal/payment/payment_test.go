package payment_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/fakepsp"
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

type env struct {
	svc  payment.Service
	psp  *fakepsp.Server
	pg   *testpg.DB
	ctx  context.Context
	hook *httptest.Server
}

func setup(t *testing.T, cfg fakepsp.Config) *env {
	t.Helper()
	pg := testpg.NewSets(t,
		testpg.Set{FS: ledger.Migrations, Dir: "migrations", Schema: ledger.Schema},
		testpg.Set{FS: payment.Migrations, Dir: "migrations", Schema: payment.Schema})
	e := &env{pg: pg, ctx: tenancy.With(context.Background(), tenantA)}
	// payment's webhook endpoint, which the fake processor calls back
	e.hook = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payment.WebhookHandler(e.svc, secret, slog.Default()).ServeHTTP(w, r)
	}))
	t.Cleanup(e.hook.Close)
	cfg.APIKey, cfg.WebhookURL, cfg.WebhookSecret = "sk_test", e.hook.URL, secret
	e.psp = fakepsp.New(cfg)
	api := httptest.NewServer(e.psp)
	t.Cleanup(api.Close)
	e.svc = payment.Service{DB: pg.App, PSP: psp.HTTPClient{BaseURL: api.URL, APIKey: "sk_test"}}
	return e
}

func usd(m int64) money.Money { return money.MustNew(m, "USD") }

func auth(order, pm string, amount int64) payment.AuthorizeInput {
	return payment.AuthorizeInput{OrderID: order, CustomerID: "cus_1", Amount: usd(amount), PaymentMethod: pm}
}

func balances(t *testing.T, e *env) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	_ = db.InTenantTx(e.ctx, e.pg.App, func(tx pgx.Tx) error {
		rows, _ := tx.Query(e.ctx, `SELECT code, balance_minor FROM ledger.accounts`)
		defer rows.Close()
		for rows.Next() {
			var c string
			var b int64
			_ = rows.Scan(&c, &b)
			out[c] = b
		}
		return nil
	})
	return out
}

func events(t *testing.T, e *env) []string {
	t.Helper()
	var out []string
	rows, err := e.pg.Owner.Query(e.ctx, `SELECT type FROM payment.outbox ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func TestAuthorizeCaptureBooksTheLedger(t *testing.T) {
	e := setup(t, fakepsp.Config{})
	it, err := e.svc.Authorize(e.ctx, auth("o1", "pm_ok", 10000))
	if err != nil || it.Status != payment.Authorized || it.ChargeID == "" {
		t.Fatalf("authorize: %+v %v", it, err)
	}
	it, err = e.svc.Capture(e.ctx, "o1")
	if err != nil || it.Status != payment.Captured {
		t.Fatalf("capture: %+v %v", it, err)
	}
	b := balances(t, e)
	fee := int64(320) // 2.9% of 100.00 + 0.30
	if b["sales"] != -10000 || b["psp_fees"] != fee || b["psp_receivable"] != 10000-fee {
		t.Fatalf("ledger after capture: %v", b)
	}
	if got := strings.Join(events(t, e), ","); got != "payment.authorized,payment.captured" {
		t.Fatalf("events: %s", got)
	}
	// capture again: no second ledger entry, no new event
	if _, err := e.svc.Capture(e.ctx, "o1"); err != nil {
		t.Fatal(err)
	}
	if b2 := balances(t, e); b2["sales"] != -10000 || len(events(t, e)) != 2 {
		t.Fatalf("second capture changed something: %v %v", b2, events(t, e))
	}
}

func TestDeclineIsFinal(t *testing.T) {
	e := setup(t, fakepsp.Config{})
	it, err := e.svc.Authorize(e.ctx, auth("o1", "pm_insufficient", 500))
	if !errors.Is(err, payment.ErrDeclined) || it.Status != payment.Failed || it.FailureReason != "insufficient_funds" {
		t.Fatalf("decline: %+v %v", it, err)
	}
	if _, err := e.svc.Authorize(e.ctx, auth("o1", "pm_insufficient", 500)); !errors.Is(err, payment.ErrDeclined) || e.psp.Charges() != 1 {
		t.Fatalf("retry of a declined payment reached the processor again: charges=%d err=%v", e.psp.Charges(), err)
	}
	if _, err := e.svc.Authorize(e.ctx, auth("o1", "pm_ok", 999)); !errors.Is(err, payment.ErrConflict) {
		t.Fatalf("same order, different amount: %v", err)
	}
}

func TestConcurrentAuthorizeChargesOnce(t *testing.T) {
	e := setup(t, fakepsp.Config{})
	var wg sync.WaitGroup
	ids := make(chan string, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			it, err := e.svc.Authorize(e.ctx, auth("o1", "pm_ok", 2500))
			if err != nil {
				t.Error(err)
				return
			}
			ids <- it.ChargeID
		}()
	}
	wg.Wait()
	close(ids)
	for id := range ids {
		if id == "" {
			t.Fatal("authorized without a charge id")
		}
	}
	if n := e.psp.Charges(); n != 1 {
		t.Fatalf("10 concurrent authorizations created %d charges at the processor", n)
	}
}

// lossy returns ErrUnavailable for the first Authorize AFTER the processor accepted it, like a
// timeout on the way back.
type lossy struct {
	psp.Client
	once sync.Once
}

func (l *lossy) Authorize(ctx context.Context, r psp.AuthorizeRequest) (psp.Charge, error) {
	ch, err := l.Client.Authorize(ctx, r)
	lost := false
	l.once.Do(func() { lost = true })
	if lost {
		return psp.Charge{}, psp.ErrUnavailable
	}
	return ch, err
}

func TestLostAuthorizationResponseIsFoundAndVoided(t *testing.T) {
	e := setup(t, fakepsp.Config{})
	e.svc.PSP = &lossy{Client: e.svc.PSP}
	it, err := e.svc.Authorize(e.ctx, auth("o1", "pm_ok", 4000))
	if !errors.Is(err, payment.ErrUnavailable) || it.Status != payment.Pending {
		t.Fatalf("lost response: %+v %v", it, err)
	}
	// the saga gives up on this order and compensates: the hidden authorization must be released
	it, err = e.svc.Void(e.ctx, "o1")
	if err != nil || it.Status != payment.Voided || it.ChargeID == "" {
		t.Fatalf("void: %+v %v", it, err)
	}
	if ch, _ := e.psp.Charge(it.ChargeID); ch.Status != psp.Voided {
		t.Fatalf("the processor still holds the money: %s", ch.Status)
	}
}

func TestExpiredAuthorizationCannotBeCaptured(t *testing.T) {
	e := setup(t, fakepsp.Config{})
	if _, err := e.svc.Authorize(e.ctx, auth("o1", "pm_expire_auth", 700)); err != nil {
		t.Fatal(err)
	}
	it, err := e.svc.Capture(e.ctx, "o1")
	if !errors.Is(err, payment.ErrDeclined) || it.Status != payment.Authorized {
		t.Fatalf("capture: %+v %v", it, err)
	}
	if b := balances(t, e); b["sales"] != 0 {
		t.Fatalf("a failed capture booked revenue: %v", b)
	}
}

func TestRefundCompletesByWebhookOnceDespiteDuplicates(t *testing.T) {
	e := setup(t, fakepsp.Config{DuplicateWebhooks: true})
	_, _ = e.svc.Authorize(e.ctx, auth("o1", "pm_ok", 10000))
	_, _ = e.svc.Capture(e.ctx, "o1")
	it, err := e.svc.Refund(e.ctx, "o1")
	if err != nil || (it.Status != payment.RefundPending && it.Status != payment.Refunded) {
		t.Fatalf("refund: %+v %v", it, err)
	}
	e.psp.Wait() // both copies of refund.succeeded delivered
	it, _ = e.svc.Get(e.ctx, "o1")
	if it.Status != payment.Refunded {
		t.Fatalf("after webhook: %s", it.Status)
	}
	b := balances(t, e)
	if b["sales"] != 0 || b["psp_receivable"] != -320 || b["psp_fees"] != 320 {
		t.Fatalf("ledger after refund (fee is not returned): %v", b)
	}
	var n int
	_ = e.pg.Owner.QueryRow(e.ctx, `SELECT count(*) FROM payment.webhook_events`).Scan(&n)
	if n != 1 {
		t.Fatalf("duplicate webhook recorded %d times", n)
	}
}

func TestRefundFailureReturnsToCaptured(t *testing.T) {
	e := setup(t, fakepsp.Config{})
	_, _ = e.svc.Authorize(e.ctx, auth("o1", "pm_refund_fails", 1000))
	_, _ = e.svc.Capture(e.ctx, "o1")
	_, _ = e.svc.Refund(e.ctx, "o1")
	e.psp.Wait()
	it, _ := e.svc.Get(e.ctx, "o1")
	if it.Status != payment.Captured || !strings.HasPrefix(it.FailureReason, "refund failed") {
		t.Fatalf("after refund.failed: %+v", it)
	}
	// a later retry is a new refund attempt (new idempotency key), not a replay of the failed one
	it, _ = e.svc.Refund(e.ctx, "o1")
	e.psp.Wait()
	if it.RefundAttempt != 2 {
		t.Fatalf("second refund attempt number: %d", it.RefundAttempt)
	}
}

func TestWebhookSecurity(t *testing.T) {
	e := setup(t, fakepsp.Config{})
	body, _ := json.Marshal(psp.Event{ID: "evt_1", Type: "refund.succeeded",
		Data: psp.EventData{ChargeID: "ch_x", Metadata: map[string]string{"tenant_id": string(tenantA), "order_id": "nope"}}})
	now := time.Now()

	if _, err := e.svc.HandleWebhook(context.Background(), secret, body, psp.Sign("wrong-secret", body, now), now); !errors.Is(err, psp.ErrBadSignature) {
		t.Errorf("forged signature: %v", err)
	}
	old := psp.Sign(secret, body, now.Add(-10*time.Minute))
	if _, err := e.svc.HandleWebhook(context.Background(), secret, body, old, now); !errors.Is(err, psp.ErrBadSignature) {
		t.Errorf("10-minute-old signature (replay) accepted: %v", err)
	}
	tampered := []byte(strings.Replace(string(body), "evt_1", "evt_2", 1))
	if _, err := e.svc.HandleWebhook(context.Background(), secret, tampered, psp.Sign(secret, body, now), now); !errors.Is(err, psp.ErrBadSignature) {
		t.Errorf("tampered body accepted: %v", err)
	}
	sig := psp.Sign(secret, body, now)
	if dup, err := e.svc.HandleWebhook(context.Background(), secret, body, sig, now); err == nil && dup {
		t.Errorf("first delivery reported as duplicate")
	}
	if dup, err := e.svc.HandleWebhook(context.Background(), secret, body, sig, now); err != nil || !dup {
		t.Errorf("redelivery: dup=%v err=%v", dup, err)
	}
}

func TestReconciliation(t *testing.T) {
	e := setup(t, fakepsp.Config{})
	for _, o := range []string{"o1", "o2"} {
		_, _ = e.svc.Authorize(e.ctx, auth(o, "pm_ok", 5000))
		_, _ = e.svc.Capture(e.ctx, o)
	}
	_, _ = e.svc.Refund(e.ctx, "o2")
	e.psp.Wait()
	// another tenant's activity must not show up in A's reconciliation
	ctxB := tenancy.With(context.Background(), tenantB)
	_, _ = e.svc.Authorize(ctxB, auth("b1", "pm_ok", 777))
	_, _ = e.svc.Capture(ctxB, "b1")

	res, err := e.svc.Reconcile(e.ctx, time.Now())
	if err != nil || res.Matched != 3 || len(res.Breaks) != 0 {
		t.Fatalf("clean day: matched=%d breaks=%+v err=%v (want 2 captures + 1 refund, no breaks)", res.Matched, res.Breaks, err)
	}
	// the processor reports a capture we never booked (e.g. a charge made outside the platform)
	e.psp.InjectSettlementLine(time.Now(), psp.SettlementLine{Type: "capture", ChargeID: "ch_ghost", Amount: 1234, Fee: 66,
		Currency: "USD", Metadata: map[string]string{"tenant_id": string(tenantA)}})
	res, _ = e.svc.Reconcile(e.ctx, time.Now())
	if len(res.Breaks) != 1 || res.Breaks[0].Kind != "missing_in_ledger" || res.Breaks[0].ChargeID != "ch_ghost" {
		t.Fatalf("ghost capture: %+v", res.Breaks)
	}
}

func TestTenantIsolation(t *testing.T) {
	e := setup(t, fakepsp.Config{})
	_, _ = e.svc.Authorize(e.ctx, auth("o1", "pm_ok", 100))
	ctxB := tenancy.With(context.Background(), tenantB)
	if _, err := e.svc.Get(ctxB, "o1"); !errors.Is(err, payment.ErrNotFound) {
		t.Fatalf("tenant B sees tenant A's payment: %v", err)
	}
	if _, err := e.svc.Capture(ctxB, "o1"); !errors.Is(err, payment.ErrNotFound) {
		t.Fatalf("tenant B captured tenant A's payment: %v", err)
	}
}
