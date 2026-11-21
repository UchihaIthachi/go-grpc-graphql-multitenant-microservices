// Package e2e runs the whole system in one process: the four gRPC services on real TCP
// listeners, the GraphQL gateway, the fake payment processor and its webhooks, against one
// Postgres database (one schema per service; production uses a database per service).
// Everything a client can see is exercised through GraphQL, the way a client would.
package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	accountv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/account/v1"
	catalogv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/catalog/v1"
	orderv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/order/v1"
	paymentv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/payment/v1"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/account"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/catalog"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/fakepsp"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/gateway"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/order"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/order/grpcdeps"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/payment"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/payment/ledger"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/payment/psp"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/testpg"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/auth"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/grpcx"
)

const whsec = "whsec_e2e"

type system struct {
	t       *testing.T
	gateway string
	catalog string // address, for calling a service directly
	pub     ed25519.PublicKey
}

func quiet() *slog.Logger {
	if os.Getenv("E2E_LOG") != "" {
		return slog.Default()
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func start(t *testing.T) *system {
	t.Helper()
	pg := testpg.NewSets(t,
		testpg.Set{FS: account.Migrations, Dir: "migrations", Schema: account.Schema},
		testpg.Set{FS: catalog.Migrations, Dir: "migrations", Schema: catalog.Schema},
		testpg.Set{FS: ledger.Migrations, Dir: "migrations", Schema: ledger.Schema},
		testpg.Set{FS: payment.Migrations, Dir: "migrations", Schema: payment.Schema},
		testpg.Set{FS: order.Migrations, Dir: "migrations", Schema: order.Schema})
	log := quiet()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	secret := bytes.Repeat([]byte("s"), 32)
	verifier := auth.Verifier{Public: pub, ServiceSecret: secret}

	serve := func(register func(*grpc.Server), public ...string) string {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := grpc.NewServer(grpcx.ServerOptions(verifier, log, public...)...)
		register(srv)
		go srv.Serve(lis)
		t.Cleanup(srv.Stop)
		return lis.Addr().String()
	}
	dial := func(addr string, ic grpc.UnaryClientInterceptor) *grpc.ClientConn {
		cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithUnaryInterceptor(ic))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cc.Close() })
		return cc
	}

	// payment + the fake processor calling its webhook endpoint
	var paySvc payment.Service
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payment.WebhookHandler(paySvc, whsec, log).ServeHTTP(w, r)
	}))
	t.Cleanup(hook.Close)
	fp := fakepsp.New(fakepsp.Config{APIKey: "sk_e2e", WebhookURL: hook.URL, WebhookSecret: whsec, Log: log})
	pspAPI := httptest.NewServer(fp)
	t.Cleanup(pspAPI.Close)
	t.Cleanup(fp.Wait)
	paySvc = payment.Service{DB: pg.App, PSP: psp.HTTPClient{BaseURL: pspAPI.URL, APIKey: "sk_e2e"}}

	accountAddr := serve(func(s *grpc.Server) {
		accountv1.RegisterAccountServiceServer(s, account.Server{Svc: account.Service{DB: pg.App,
			Issuer: auth.Issuer{Key: priv}, BcryptCost: bcrypt.MinCost}})
	}, account.PublicMethods...)
	catalogAddr := serve(func(s *grpc.Server) {
		catalogv1.RegisterCatalogServiceServer(s, catalog.Server{Store: catalog.Store{DB: pg.App}})
	})
	paymentAddr := serve(func(s *grpc.Server) { paymentv1.RegisterPaymentServiceServer(s, payment.Server{Svc: paySvc}) })

	asOrder := grpcx.AsService("order", secret)
	engine := order.Engine{DB: pg.App, Log: log, Backoff: 10 * time.Millisecond,
		Catalog:  grpcdeps.Catalog{C: catalogv1.NewCatalogServiceClient(dial(catalogAddr, asOrder))},
		Payments: grpcdeps.Payments{C: paymentv1.NewPaymentServiceClient(dial(paymentAddr, asOrder))}}
	orderAddr := serve(func(s *grpc.Server) { orderv1.RegisterOrderServiceServer(s, order.Server{Engine: engine}) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go engine.RunRecovery(ctx, 50*time.Millisecond)

	fwd := grpcx.ForwardUser()
	gw := httptest.NewServer(gateway.Handler(gateway.Clients{
		Account: accountv1.NewAccountServiceClient(dial(accountAddr, fwd)),
		Catalog: catalogv1.NewCatalogServiceClient(dial(catalogAddr, fwd)),
		Order:   orderv1.NewOrderServiceClient(dial(orderAddr, fwd)),
		Payment: paymentv1.NewPaymentServiceClient(dial(paymentAddr, fwd)),
	}, verifier, log))
	t.Cleanup(gw.Close)
	return &system{t: t, gateway: gw.URL, catalog: catalogAddr, pub: pub}
}

// gql posts a query and returns data and the first error's code ("" if none).
func (s *system) gql(token, query string, vars map[string]any) (map[string]any, string) {
	s.t.Helper()
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, _ := http.NewRequest(http.MethodPost, s.gateway, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		Data   map[string]any
		Errors []struct {
			Message    string
			Extensions map[string]any
		}
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		s.t.Fatalf("decode (HTTP %d): %v", res.StatusCode, err)
	}
	if len(out.Errors) > 0 {
		code, _ := out.Errors[0].Extensions["code"].(string)
		if code == "" {
			s.t.Fatalf("error without code: %s", out.Errors[0].Message)
		}
		return out.Data, code
	}
	return out.Data, ""
}

// must is gql that fails the test on any error.
func (s *system) must(token, query string, vars map[string]any) map[string]any {
	s.t.Helper()
	d, code := s.gql(token, query, vars)
	if code != "" {
		s.t.Fatalf("unexpected %s for %s", code, strings.Fields(query)[0:2])
	}
	return d
}

func get(m any, path ...string) any {
	for _, p := range path {
		m = m.(map[string]any)[p]
	}
	return m
}

const (
	signUp   = `mutation($i: SignUpInput!) { signUp(input: $i) { token user { role shopSlug } } }`
	register = `mutation($i: CredentialsInput!) { register(input: $i) { token user { id role } } }`
	login    = `mutation($i: CredentialsInput!) { login(input: $i) { token } }`
	create   = `mutation($i: CreateProductInput!) { createProduct(input: $i) { id available price { amount minor currency } } }`
	place    = `mutation($i: PlaceOrderInput!) { placeOrder(input: $i) { id status reason history total { amount } lines { sku qty subtotal { amount } } } }`
	getOrder = `query($id: ID!) { order(id: $id) { id status payment { status amount { amount } fee { minor } } } }`
	search   = `query($q: String) { products(query: $q) { items { id name available } nextCursor } }`
)

func (s *system) shop(slug string) (admin string) {
	d := s.must("", signUp, map[string]any{"i": map[string]any{"shopName": strings.ToUpper(slug), "shopSlug": slug,
		"email": "owner@" + slug + ".test", "password": "correct horse battery"}})
	return get(d, "signUp", "token").(string)
}

func (s *system) customer(slug, email string) string {
	d := s.must("", register, map[string]any{"i": map[string]any{"shopSlug": slug, "email": email, "password": "customer password"}})
	return get(d, "register", "token").(string)
}

func (s *system) product(admin, sku, price string, stock int) string {
	d := s.must(admin, create, map[string]any{"i": map[string]any{"sku": sku, "name": "Product " + sku,
		"description": "a fine " + strings.ToLower(sku), "price": map[string]any{"amount": price, "currency": "USD"}, "initialStock": stock}})
	return get(d, "createProduct", "id").(string)
}

func (s *system) order(token, req, pm string, lines ...any) map[string]any {
	s.t.Helper()
	d := s.must(token, place, map[string]any{"i": map[string]any{"requestId": req, "paymentMethod": pm, "lines": lines}})
	return get(d, "placeOrder").(map[string]any)
}

func line(id string, qty int) map[string]any { return map[string]any{"productId": id, "qty": qty} }

func TestShopping(t *testing.T) {
	s := start(t)
	admin := s.shop("acme")
	mug := s.product(admin, "MUG", "12.50", 3)
	tee := s.product(admin, "TEE", "20.00", 10)
	alice := s.customer("acme", "alice@example.com")

	// products: full-text search, and money as decimal strings
	d := s.must(alice, search, map[string]any{"q": "mug"})
	items := get(d, "products", "items").([]any)
	if len(items) != 1 || get(items[0], "name") != "Product MUG" {
		t.Fatalf("search: %v", items)
	}

	// a confirmed order, with its saga history
	o := s.order(alice, "req-1", "pm_ok", line(mug, 2), line(tee, 1))
	if o["status"] != order.Confirmed || get(o, "total", "amount") != "45.00" {
		t.Fatalf("order: %v", o)
	}
	if h := fmt.Sprint(o["history"]); h != "[PENDING STOCK_RESERVED PAYMENT_AUTHORIZED PAYMENT_CAPTURED CONFIRMED]" {
		t.Fatalf("history: %s", h)
	}
	// retrying the same request returns the same order and charges once
	if again := s.order(alice, "req-1", "pm_ok", line(mug, 2), line(tee, 1)); again["id"] != o["id"] {
		t.Fatalf("retry created a second order")
	}
	// stock went down
	d = s.must(alice, `query($id: ID!) { product(id: $id) { available } }`, map[string]any{"id": mug})
	if get(d, "product", "available") != float64(1) {
		t.Fatalf("stock: %v", d)
	}

	// declined card: rejected, stock back
	rej := s.order(alice, "req-2", "pm_decline", line(mug, 1))
	if rej["status"] != order.Rejected || rej["reason"] != "payment_declined: card_declined" {
		t.Fatalf("declined: %v", rej)
	}
	// out of stock: rejected before payment
	oos := s.order(alice, "req-3", "pm_ok", line(mug, 5))
	if oos["status"] != order.Rejected || !strings.HasPrefix(oos["reason"].(string), "out_of_stock") {
		t.Fatalf("out of stock: %v", oos)
	}

	// customers see only their own orders; payments only for admins
	bob := s.customer("acme", "bob@example.com")
	if d := s.must(bob, getOrder, map[string]any{"id": o["id"]}); d["order"] != nil {
		t.Fatalf("bob saw alice's order")
	}
	if d := s.must(alice, getOrder, map[string]any{"id": o["id"]}); get(d, "order", "payment") != nil {
		t.Fatalf("customer saw payment details")
	}
	d = s.must(admin, getOrder, map[string]any{"id": o["id"]})
	if get(d, "order", "payment", "status") != payment.Captured || get(d, "order", "payment", "fee", "minor") != "161" { // 2.9% of 4500 + 30
		t.Fatalf("admin payment view: %v", d)
	}
	d = s.must(alice, `{ orders { id } me { email role } }`, nil)
	if len(d["orders"].([]any)) != 3 || get(d, "me", "role") != auth.RoleCustomer {
		t.Fatalf("orders/me: %v", d)
	}
}

func TestTenantsAreIsolated(t *testing.T) {
	s := start(t)
	acme, globex := s.shop("acme"), s.shop("globex")
	mug := s.product(acme, "MUG", "12.50", 3)
	alice := s.customer("acme", "alice@example.com")
	o := s.order(alice, "r1", "pm_ok", line(mug, 1))

	// globex's admin can't see acme's products or orders, even by id
	if d := s.must(globex, search, map[string]any{"q": ""}); len(get(d, "products", "items").([]any)) != 0 {
		t.Fatal("globex sees acme's products")
	}
	if d := s.must(globex, getOrder, map[string]any{"id": o["id"]}); d["order"] != nil {
		t.Fatal("globex sees acme's order")
	}
	// and can't buy acme's product through its own shop
	eve := s.customer("globex", "eve@example.com")
	if _, code := s.gql(eve, place, map[string]any{"i": map[string]any{"requestId": "x", "paymentMethod": "pm_ok",
		"lines": []any{line(mug, 1)}}}); code != "BAD_USER_INPUT" {
		t.Fatalf("cross-tenant order: %s", code)
	}
	// the same email is a separate account in each shop
	s.customer("globex", "alice@example.com")
}

func TestAuthRules(t *testing.T) {
	s := start(t)
	admin := s.shop("acme")
	alice := s.customer("acme", "alice@example.com")

	if _, code := s.gql(alice, create, map[string]any{"i": map[string]any{"sku": "X", "name": "X",
		"price": map[string]any{"amount": "1.00", "currency": "USD"}}}); code != "FORBIDDEN" {
		t.Fatalf("customer created a product: %s", code)
	}
	if _, code := s.gql("", `{ me { id } }`, nil); code != "UNAUTHENTICATED" {
		t.Fatalf("anonymous me: %s", code)
	}
	if _, code := s.gql("not.a.token", `{ me { id } }`, nil); code != "UNAUTHENTICATED" {
		t.Fatalf("bad token: %s", code)
	}
	// wrong password and unknown shop look the same
	for _, c := range []map[string]any{
		{"shopSlug": "acme", "email": "alice@example.com", "password": "wrong password!"},
		{"shopSlug": "nope", "email": "alice@example.com", "password": "customer password"},
	} {
		if _, code := s.gql("", login, map[string]any{"i": c}); code != "UNAUTHENTICATED" {
			t.Fatalf("login %v: %s", c, code)
		}
	}
	s.must("", login, map[string]any{"i": map[string]any{"shopSlug": "acme", "email": "ALICE@example.com", "password": "customer password"}})
	if _, code := s.gql("", signUp, map[string]any{"i": map[string]any{"shopName": "A", "shopSlug": "acme",
		"email": "x@y.test", "password": "long enough pw"}}); code != "CONFLICT" {
		t.Fatalf("duplicate slug: %s", code)
	}
	if _, code := s.gql("", register, map[string]any{"i": map[string]any{"shopSlug": "acme", "email": "short@pw.test", "password": "short"}}); code != "BAD_USER_INPUT" {
		t.Fatalf("short password: %s", code)
	}
	if _, code := s.gql(admin, `mutation { reconcile(day: "2026-01-01") { matched } }`, nil); code != "" {
		t.Fatalf("admin reconcile: %s", code)
	}
	if _, code := s.gql(alice, `mutation { reconcile(day: "2026-01-01") { matched } }`, nil); code != "FORBIDDEN" {
		t.Fatalf("customer reconcile: %s", code)
	}

	// Saga steps take service tokens only: a customer token can't reserve stock even when it
	// reaches the catalog service directly, bypassing the gateway.
	cc, _ := grpc.NewClient(s.catalog, grpc.WithTransportCredentials(insecure.NewCredentials()))
	defer cc.Close()
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+alice)
	_, err := catalogv1.NewCatalogServiceClient(cc).ReserveStock(ctx, &catalogv1.ReserveStockRequest{OrderId: "x",
		Lines: []*catalogv1.Line{{ProductId: "00000000-0000-4000-8000-000000000000", Qty: 1}}})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("customer reserved stock directly: %v", err)
	}
}

func TestQueryLimits(t *testing.T) {
	s := start(t)
	admin := s.shop("acme")
	deep := `{ orders { lines { unitPrice { amount } } } }` // depth 4: fine
	s.must(admin, deep, nil)
	q := "{ me { id } }" + strings.Repeat(" ", 9000)
	if _, code := s.gql(admin, q, nil); code == "" {
		t.Fatal("oversized query accepted")
	}
}
