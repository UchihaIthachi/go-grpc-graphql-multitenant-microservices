package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/payment/ledger"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/testpg"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/db"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/money"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/tenancy"
)

const (
	tenantA = tenancy.ID("11111111-1111-4111-8111-111111111111")
	tenantB = tenancy.ID("22222222-2222-4222-8222-222222222222")
)

func setup(t *testing.T) (ledger.Store, *testpg.DB) {
	pg := testpg.New(t, ledger.Migrations, "migrations", ledger.Schema)
	return ledger.Store{DB: pg.App}, pg
}

func usd(minor int64) money.Money { return money.MustNew(minor, "USD") }

func transfer(key, from, to string, amt int64) ledger.EntryRequest {
	return ledger.EntryRequest{IdempotencyKey: key, Kind: "transfer", Postings: []ledger.Posting{
		{AccountID: to, Amount: usd(amt)}, {AccountID: from, Amount: usd(-amt)},
	}}
}

func TestPostBalancesAccounts(t *testing.T) {
	s, _ := setup(t)
	ctx := tenancy.With(context.Background(), tenantA)
	clearing, _ := s.EnsureAccount(ctx, "psp_clearing", "USD", ledger.Asset, true)
	customer, _ := s.EnsureAccount(ctx, "customer:42", "USD", ledger.Liability, false)

	e, replayed, err := s.Post(ctx, transfer("pay-1", clearing.ID, customer.ID, 2500))
	if err != nil || replayed || len(e.Postings) != 2 {
		t.Fatalf("post: %v replayed=%v %+v", err, replayed, e)
	}
	c, _ := s.Account(ctx, customer.ID)
	k, _ := s.Account(ctx, clearing.ID)
	if c.Balance.Amount != 2500 || k.Balance.Amount != -2500 {
		t.Fatalf("balances customer=%v clearing=%v", c.Balance, k.Balance)
	}
}

func TestIdempotentReplayAndConflict(t *testing.T) {
	s, _ := setup(t)
	ctx := tenancy.With(context.Background(), tenantA)
	a, _ := s.EnsureAccount(ctx, "a", "USD", ledger.Asset, true)
	b, _ := s.EnsureAccount(ctx, "b", "USD", ledger.Asset, true)

	first, _, err := s.Post(ctx, transfer("k1", a.ID, b.ID, 100))
	if err != nil {
		t.Fatal(err)
	}
	again, replayed, err := s.Post(ctx, transfer("k1", a.ID, b.ID, 100))
	if err != nil || !replayed || again.ID != first.ID {
		t.Fatalf("replay: err=%v replayed=%v id %s vs %s", err, replayed, again.ID, first.ID)
	}
	if acc, _ := s.Account(ctx, b.ID); acc.Balance.Amount != 100 {
		t.Fatalf("replay moved money again: %v", acc.Balance)
	}
	if _, _, err := s.Post(ctx, transfer("k1", a.ID, b.ID, 999)); !errors.Is(err, ledger.ErrIdempotencyConflict) {
		t.Fatalf("same key, different amount: %v", err)
	}
}

func TestConcurrentDuplicateRequestsPostOnce(t *testing.T) {
	s, _ := setup(t)
	ctx := tenancy.With(context.Background(), tenantA)
	a, _ := s.EnsureAccount(ctx, "a", "USD", ledger.Asset, true)
	b, _ := s.EnsureAccount(ctx, "b", "USD", ledger.Asset, true)

	var wg sync.WaitGroup
	ids := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, _, err := s.Post(ctx, transfer("same-key", a.ID, b.ID, 700))
			if err != nil {
				t.Error(err)
				return
			}
			ids <- e.ID
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}
	if acc, _ := s.Account(ctx, b.ID); len(seen) != 1 || acc.Balance.Amount != 700 {
		t.Fatalf("20 identical requests: %d distinct entries, balance %v (want 1 entry, 7.00)", len(seen), acc.Balance)
	}
}

func TestConcurrentTransfersKeepTheBooksBalanced(t *testing.T) {
	s, _ := setup(t)
	ctx := tenancy.With(context.Background(), tenantA)
	var accs []ledger.Account
	for i := 0; i < 5; i++ {
		a, err := s.EnsureAccount(ctx, fmt.Sprintf("acc%d", i), "USD", ledger.Asset, true)
		if err != nil {
			t.Fatal(err)
		}
		accs = append(accs, a)
	}
	var wg sync.WaitGroup
	for w := 0; w < 10; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				from, to := accs[rand.IntN(5)], accs[rand.IntN(5)]
				if from.ID == to.ID {
					continue
				}
				if _, _, err := s.Post(ctx, transfer(fmt.Sprintf("w%d-%d", w, i), from.ID, to.ID, int64(rand.IntN(5000)+1))); err != nil {
					t.Errorf("transfer: %v", err)
				}
			}
		}(w)
	}
	wg.Wait()

	var total int64
	for _, a := range accs {
		acc, _ := s.Account(ctx, a.ID)
		total += acc.Balance.Amount
	}
	if total != 0 {
		t.Fatalf("money was created or destroyed: balances sum to %d", total)
	}
	if bad, err := s.Verify(ctx); err != nil || len(bad) > 0 {
		t.Fatalf("cached balances disagree with postings: %v %v", bad, err)
	}
}

func TestInsufficientFunds(t *testing.T) {
	s, _ := setup(t)
	ctx := tenancy.With(context.Background(), tenantA)
	wallet, _ := s.EnsureAccount(ctx, "wallet", "USD", ledger.Liability, false)
	sink, _ := s.EnsureAccount(ctx, "sink", "USD", ledger.Asset, true)
	if _, _, err := s.Post(ctx, transfer("overdraw", wallet.ID, sink.ID, 1)); !errors.Is(err, ledger.ErrInsufficientFunds) {
		t.Fatalf("overdraw of a no-negative account: %v", err)
	}
	if acc, _ := s.Account(ctx, wallet.ID); acc.Balance.Amount != 0 {
		t.Fatalf("failed entry left a balance: %v", acc.Balance)
	}
}

func TestDatabaseRejectsUnbalancedEntry(t *testing.T) {
	s, pg := setup(t)
	ctx := tenancy.With(context.Background(), tenantA)
	a, _ := s.EnsureAccount(ctx, "a", "USD", ledger.Asset, true)
	b, _ := s.EnsureAccount(ctx, "b", "USD", ledger.Asset, true)

	// bypass the Go validation entirely: write an unbalanced entry with raw SQL as the app role
	err := db.InTenantTx(ctx, pg.App, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO ledger.entries (tenant_id, idempotency_key, request_hash, kind)
			VALUES (current_setting('app.tenant_id')::uuid, 'raw', '\x00', 'hack') RETURNING id`).Scan(&id); err != nil {
			return err
		}
		for _, p := range []struct {
			acc string
			amt int64
		}{{a.ID, 500}, {b.ID, -400}} {
			if _, err := tx.Exec(ctx, `INSERT INTO ledger.postings (tenant_id, entry_id, account_id, amount_minor, currency)
				VALUES (current_setting('app.tenant_id')::uuid, $1, $2, $3, 'USD')`, id, p.acc, p.amt); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		t.Fatal("the database accepted an entry that creates 1.00 USD out of nothing")
	}
	if acc, _ := s.Account(ctx, a.ID); acc.Balance.Amount != 0 {
		t.Fatalf("rolled-back entry changed a balance: %v", acc.Balance)
	}
}

func TestHistoryIsAppendOnly(t *testing.T) {
	s, pg := setup(t)
	ctx := tenancy.With(context.Background(), tenantA)
	a, _ := s.EnsureAccount(ctx, "a", "USD", ledger.Asset, true)
	b, _ := s.EnsureAccount(ctx, "b", "USD", ledger.Asset, true)
	if _, _, err := s.Post(ctx, transfer("k", a.ID, b.ID, 100)); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE ledger.postings SET amount_minor = amount_minor * 100`,
		`DELETE FROM ledger.postings`,
		`UPDATE ledger.accounts SET balance_minor = 1000000`,
	} {
		err := db.InTenantTx(ctx, pg.App, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, q); return err })
		if err == nil {
			t.Errorf("app role was allowed: %s", q)
		}
	}
	// even the owner can't rewrite history
	if _, err := pg.Owner.Exec(ctx, `UPDATE ledger.postings SET amount_minor = 1`); err == nil {
		t.Error("owner was allowed to update a posting")
	}
}

func TestTenantsCannotSeeOrUseEachOthersAccounts(t *testing.T) {
	s, _ := setup(t)
	ctxA := tenancy.With(context.Background(), tenantA)
	ctxB := tenancy.With(context.Background(), tenantB)
	a1, _ := s.EnsureAccount(ctxA, "a1", "USD", ledger.Asset, true)
	a2, _ := s.EnsureAccount(ctxA, "a2", "USD", ledger.Asset, true)
	b1, _ := s.EnsureAccount(ctxB, "b1", "USD", ledger.Asset, true)
	if _, _, err := s.Post(ctxA, transfer("x", a1.ID, a2.ID, 100)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Account(ctxB, a1.ID); !errors.Is(err, ledger.ErrUnknownAccount) {
		t.Errorf("tenant B read tenant A's account: %v", err)
	}
	// B knows A's account id and tries to move money out of it
	if _, _, err := s.Post(ctxB, transfer("steal", a1.ID, b1.ID, 100)); !errors.Is(err, ledger.ErrUnknownAccount) {
		t.Errorf("tenant B posted against tenant A's account: %v", err)
	}
	// B's verify sees only B's books
	if bad, err := s.Verify(ctxB); err != nil || len(bad) != 0 {
		t.Errorf("verify B: %v %v", bad, err)
	}
	if acc, _ := s.Account(ctxA, a1.ID); acc.Balance.Amount != -100 {
		t.Errorf("A's balance changed by B: %v", acc.Balance)
	}
}

func TestNoTenantNoAccess(t *testing.T) {
	s, _ := setup(t)
	if _, err := s.EnsureAccount(context.Background(), "x", "USD", ledger.Asset, true); !errors.Is(err, tenancy.ErrMissing) {
		t.Fatalf("call without a tenant: %v", err)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]ledger.EntryRequest{
		"one posting":    {IdempotencyKey: "k", Kind: "x", Postings: []ledger.Posting{{AccountID: "a", Amount: usd(1)}}},
		"unbalanced":     {IdempotencyKey: "k", Kind: "x", Postings: []ledger.Posting{{AccountID: "a", Amount: usd(1)}, {AccountID: "b", Amount: usd(-2)}}},
		"zero":           {IdempotencyKey: "k", Kind: "x", Postings: []ledger.Posting{{AccountID: "a", Amount: usd(0)}, {AccountID: "b", Amount: usd(0)}}},
		"no key":         {Kind: "x", Postings: []ledger.Posting{{AccountID: "a", Amount: usd(1)}, {AccountID: "b", Amount: usd(-1)}}},
		"mixed currency": {IdempotencyKey: "k", Kind: "x", Postings: []ledger.Posting{{AccountID: "a", Amount: usd(1)}, {AccountID: "b", Amount: money.MustNew(-1, "EUR")}}},
	}
	for name, r := range cases {
		if err := r.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
