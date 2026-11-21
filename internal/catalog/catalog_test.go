package catalog_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/catalog"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/testpg"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/money"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/tenancy"
)

const (
	tenantA = tenancy.ID("11111111-1111-4111-8111-111111111111")
	tenantB = tenancy.ID("22222222-2222-4222-8222-222222222222")
)

func setup(t *testing.T) (catalog.Store, context.Context) {
	pg := testpg.New(t, catalog.Migrations, "migrations", catalog.Schema)
	return catalog.Store{DB: pg.App}, tenancy.With(context.Background(), tenantA)
}

func product(t *testing.T, s catalog.Store, ctx context.Context, sku string, stock int) catalog.Product {
	t.Helper()
	p, err := s.CreateProduct(ctx, sku, "Product "+sku, "", money.MustNew(1999, "USD"), stock)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func get(t *testing.T, s catalog.Store, ctx context.Context, id string) catalog.Product {
	t.Helper()
	ps, err := s.Products(ctx, []string{id})
	if err != nil || len(ps) != 1 {
		t.Fatalf("get %s: %v %v", id, ps, err)
	}
	return ps[0]
}

func TestFiftyBuyersTenUnitsNoOversell(t *testing.T) {
	s, ctx := setup(t)
	p := product(t, s, ctx, "LIMITED", 10)
	var ok, out atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.Reserve(ctx, fmt.Sprintf("order-%d", i), []catalog.Line{{ProductID: p.ID, Qty: 1}})
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, catalog.ErrInsufficientStock):
				out.Add(1)
			default:
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	got := get(t, s, ctx, p.ID)
	if ok.Load() != 10 || out.Load() != 40 || got.Reserved != 10 || got.Available() != 0 {
		t.Fatalf("50 buyers for 10 units: %d reserved, %d out of stock, stock %+v", ok.Load(), out.Load(), got)
	}
}

func TestDuplicateReserveHoldsOnce(t *testing.T) {
	s, ctx := setup(t)
	p := product(t, s, ctx, "A", 5)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Reserve(ctx, "order-1", []catalog.Line{{ProductID: p.ID, Qty: 2}}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := get(t, s, ctx, p.ID); got.Reserved != 2 {
		t.Fatalf("the same order reserved 10 times holds %d units (want 2)", got.Reserved)
	}
}

func TestAllLinesOrNone(t *testing.T) {
	s, ctx := setup(t)
	a := product(t, s, ctx, "A", 5)
	b := product(t, s, ctx, "B", 1)
	_, err := s.Reserve(ctx, "o1", []catalog.Line{{ProductID: a.ID, Qty: 3}, {ProductID: b.ID, Qty: 2}})
	var oos *catalog.OutOfStockError
	if !errors.As(err, &oos) || oos.ProductID != b.ID || oos.Available != 1 {
		t.Fatalf("want out of stock on B: %v", err)
	}
	if got := get(t, s, ctx, a.ID); got.Reserved != 0 {
		t.Fatalf("A was left reserved after B failed: %+v", got)
	}
}

func TestCommitAndRelease(t *testing.T) {
	s, ctx := setup(t)
	p := product(t, s, ctx, "A", 5)
	_, _ = s.Reserve(ctx, "sold", []catalog.Line{{ProductID: p.ID, Qty: 2}})
	_, _ = s.Reserve(ctx, "cancelled", []catalog.Line{{ProductID: p.ID, Qty: 1}})
	if err := s.Commit(ctx, "sold"); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(ctx, "sold"); err != nil {
		t.Fatalf("commit twice: %v", err)
	}
	if err := s.Release(ctx, "cancelled"); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, "cancelled"); err != nil {
		t.Fatalf("release twice: %v", err)
	}
	got := get(t, s, ctx, p.ID)
	if got.OnHand != 3 || got.Reserved != 0 {
		t.Fatalf("after commit 2 / release 1: %+v (want on hand 3, reserved 0)", got)
	}
	if err := s.Release(ctx, "sold"); !errors.Is(err, catalog.ErrInvalidState) {
		t.Fatalf("release of a committed order: %v", err)
	}
	if err := s.Commit(ctx, "cancelled"); !errors.Is(err, catalog.ErrReservationGone) {
		t.Fatalf("commit of a released order: %v", err)
	}
	if _, err := s.Reserve(ctx, "cancelled", []catalog.Line{{ProductID: p.ID, Qty: 1}}); !errors.Is(err, catalog.ErrReservationGone) {
		t.Fatalf("re-reserve of a released order: %v", err)
	}
}

func TestExpiredReservationsAreReleased(t *testing.T) {
	pg := testpg.New(t, catalog.Migrations, "migrations", catalog.Schema)
	ctx := tenancy.With(context.Background(), tenantA)
	short := catalog.Store{DB: pg.App, TTL: 50 * time.Millisecond}
	p := product(t, short, ctx, "A", 3)
	_, _ = short.Reserve(ctx, "slow-order", []catalog.Line{{ProductID: p.ID, Qty: 3}})
	time.Sleep(100 * time.Millisecond)
	if err := short.Commit(ctx, "slow-order"); !errors.Is(err, catalog.ErrReservationGone) {
		t.Fatalf("commit after expiry: %v", err)
	}
	// the sweeper runs without a tenant and still reaches every tenant's reservations
	if n, err := short.ExpireReservations(context.Background(), 100); err != nil || n != 1 {
		t.Fatalf("expire: n=%d err=%v", n, err)
	}
	if got := get(t, short, ctx, p.ID); got.Available() != 3 {
		t.Fatalf("stock not returned: %+v", got)
	}
}

func TestSearch(t *testing.T) {
	s, ctx := setup(t)
	_, _ = s.CreateProduct(ctx, "K1", "Mechanical keyboard", "brown switches", money.MustNew(8900, "USD"), 1)
	_, _ = s.CreateProduct(ctx, "M1", "Wireless mouse", "pairs with any keyboard", money.MustNew(2900, "USD"), 1)
	_, _ = s.CreateProduct(ctx, "C1", "USB-C cable", "", money.MustNew(900, "USD"), 1)
	items, _, err := s.Search(ctx, "keyboard", 10, "")
	if err != nil || len(items) != 2 || items[0].SKU != "K1" {
		t.Fatalf("search: %v %v (want keyboard in the name ranked first)", items, err)
	}
	page1, next, _ := s.Search(ctx, "", 2, "")
	page2, last, _ := s.Search(ctx, "", 2, next)
	if len(page1) != 2 || len(page2) != 1 || next == "" || last != "" {
		t.Fatalf("paging: %d + %d, next=%q last=%q", len(page1), len(page2), next, last)
	}
}

func TestTenantIsolation(t *testing.T) {
	s, ctx := setup(t)
	p := product(t, s, ctx, "A", 5)
	ctxB := tenancy.With(context.Background(), tenantB)
	if ps, _ := s.Products(ctxB, []string{p.ID}); len(ps) != 0 {
		t.Fatal("tenant B sees tenant A's product")
	}
	if _, err := s.Reserve(ctxB, "b-order", []catalog.Line{{ProductID: p.ID, Qty: 1}}); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("tenant B reserved tenant A's stock: %v", err)
	}
	if _, err := s.CreateProduct(ctxB, "A", "same sku, other tenant", "", money.MustNew(100, "USD"), 1); err != nil {
		t.Fatalf("SKUs are per tenant: %v", err)
	}
}
