// Package gateway is the public GraphQL API. It verifies the caller's token, then calls the
// services over gRPC with that same token: it holds no data and makes no authorization
// decisions of its own, so every rule is enforced by the service that owns the data.
package gateway

import (
	"context"
	_ "embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	graphql "github.com/graph-gophers/graphql-go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/account/v1"
	catalogv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/catalog/v1"
	commonv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/common/v1"
	orderv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/order/v1"
	paymentv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/payment/v1"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/auth"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/grpcx"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/lab"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/money"
)

//go:embed schema.graphql
var Schema string

// Clients are the services the gateway calls. Their connections must use grpcx.ForwardUser.
type Clients struct {
	Account accountv1.AccountServiceClient
	Catalog catalogv1.CatalogServiceClient
	Order   orderv1.OrderServiceClient
	Payment paymentv1.PaymentServiceClient
}

// Handler serves POST /graphql.
func Handler(c Clients, v auth.Verifier, log *slog.Logger) http.Handler {
	schema := graphql.MustParseSchema(Schema, &root{c},
		graphql.MaxDepth(8), graphql.MaxQueryLength(8<<10), graphql.MaxParallelism(10), graphql.UseStringDescriptions())
	v.ServiceSecret = nil // the public edge accepts user tokens only
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "POST a GraphQL request", http.StatusMethodNotAllowed)
			return
		}
		ctx := r.Context()
		if h := r.Header.Get("Authorization"); h != "" {
			raw, ok := strings.CutPrefix(h, "Bearer ")
			p, err := v.Verify(raw)
			if !ok || err != nil {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"errors": []map[string]any{{
					"message": "invalid or expired token", "extensions": map[string]string{"code": "UNAUTHENTICATED"}}}})
				return
			}
			ctx = auth.With(ctx, p)
		}
		var req struct {
			Query         string         `json:"query"`
			OperationName string         `json:"operationName"`
			Variables     map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "body must be a JSON GraphQL request", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		start := time.Now()
		res := schema.Exec(ctx, req.Query, req.OperationName, req.Variables)
		if lab.Enabled() {
			e := lab.Event{Kind: lab.KindRPC, From: lab.Client, To: lab.Gateway, Label: operation(req.Query, req.OperationName),
				OK: len(res.Errors) == 0, Ms: lab.Since(start)}
			if len(res.Errors) > 0 {
				e.Detail = res.Errors[0].Message
			}
			if id := placedOrder(res.Data); id != "" {
				e.Order = id
			}
			lab.Emit(ctx, e)
		}
		for _, e := range res.Errors {
			if e.Extensions == nil { // parse/validation errors from the GraphQL engine itself
				e.Extensions = map[string]any{"code": "GRAPHQL_VALIDATION_FAILED"}
			}
			if e.Extensions["code"] == "INTERNAL" {
				log.Error("graphql", "path", e.Path, "err", e.ResolverError)
			}
		}
		writeJSON(w, http.StatusOK, res)
	})
}

// operation names a request for the lab: the operation name, or its first field.
func operation(query, name string) string {
	if name != "" {
		return name
	}
	q := strings.TrimSpace(query)
	for _, p := range []string{"mutation", "query"} {
		q = strings.TrimPrefix(q, p)
	}
	if i := strings.Index(q, "{"); i >= 0 {
		q = q[i+1:]
	}
	f := strings.FieldsFunc(q, func(r rune) bool { return r == '(' || r == '{' || r == ' ' || r == '\n' || r == '\t' })
	if len(f) > 0 {
		return f[0]
	}
	return "graphql"
}

// placedOrder finds the order id in a placeOrder response, to link the client's call to it.
func placedOrder(data json.RawMessage) string {
	var d struct {
		PlaceOrder struct{ ID string } `json:"placeOrder"`
	}
	_ = json.Unmarshal(data, &d)
	return d.PlaceOrder.ID
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// gqlError carries a stable code for clients in "extensions".
type gqlError struct {
	msg, code string
}

func (e gqlError) Error() string              { return e.msg }
func (e gqlError) Extensions() map[string]any { return map[string]any{"code": e.code} }

// fail turns a gRPC error into a client-facing error. Internal details never leave.
func fail(err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return gqlError{"internal error", "INTERNAL"}
	}
	code := map[codes.Code]string{
		codes.InvalidArgument: "BAD_USER_INPUT", codes.Unauthenticated: "UNAUTHENTICATED", codes.PermissionDenied: "FORBIDDEN",
		codes.NotFound: "NOT_FOUND", codes.AlreadyExists: "CONFLICT", codes.FailedPrecondition: "FAILED_PRECONDITION",
		codes.Unavailable: "UNAVAILABLE", codes.DeadlineExceeded: "TIMEOUT", codes.ResourceExhausted: "RATE_LIMITED",
	}[st.Code()]
	if r := grpcx.Reason(err); r != "" {
		code = r
	}
	if code == "" {
		return gqlError{"internal error", "INTERNAL"}
	}
	return gqlError{st.Message(), code}
}

func signedIn(ctx context.Context) error {
	if _, ok := auth.From(ctx); !ok {
		return gqlError{"sign in first", "UNAUTHENTICATED"}
	}
	return nil
}

type root struct{ c Clients }

// ---- queries

func (r *root) Me(ctx context.Context) (*userR, error) {
	if err := signedIn(ctx); err != nil {
		return nil, err
	}
	u, err := r.c.Account.Me(ctx, &accountv1.MeRequest{})
	if err != nil {
		return nil, fail(err)
	}
	return &userR{u}, nil
}

func (r *root) Products(ctx context.Context, a struct {
	Query string // defaults in the schema make these non-null
	First int32
	After *string
}) (*pageR, error) {
	if err := signedIn(ctx); err != nil {
		return nil, err
	}
	req := &catalogv1.SearchProductsRequest{Query: a.Query, Limit: a.First}
	if a.After != nil {
		req.Cursor = *a.After
	}
	res, err := r.c.Catalog.SearchProducts(ctx, req)
	if err != nil {
		return nil, fail(err)
	}
	return &pageR{res}, nil
}

func (r *root) Product(ctx context.Context, a struct{ ID graphql.ID }) (*productR, error) {
	if err := signedIn(ctx); err != nil {
		return nil, err
	}
	res, err := r.c.Catalog.GetProducts(ctx, &catalogv1.GetProductsRequest{Ids: []string{string(a.ID)}})
	if status.Code(err) == codes.InvalidArgument || (err == nil && len(res.Products) == 0) {
		return nil, nil
	}
	if err != nil {
		return nil, fail(err)
	}
	return &productR{res.Products[0]}, nil
}

func (r *root) Order(ctx context.Context, a struct{ ID graphql.ID }) (*orderR, error) {
	if err := signedIn(ctx); err != nil {
		return nil, err
	}
	o, err := r.c.Order.GetOrder(ctx, &orderv1.GetOrderRequest{Id: string(a.ID)})
	if status.Code(err) == codes.NotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fail(err)
	}
	return &orderR{o, r.c}, nil
}

func (r *root) Orders(ctx context.Context, a struct{ First int32 }) ([]*orderR, error) {
	if err := signedIn(ctx); err != nil {
		return nil, err
	}
	req := &orderv1.ListOrdersRequest{Limit: a.First}
	res, err := r.c.Order.ListOrders(ctx, req)
	if err != nil {
		return nil, fail(err)
	}
	out := make([]*orderR, 0, len(res.Orders))
	for _, o := range res.Orders {
		out = append(out, &orderR{o, r.c})
	}
	return out, nil
}

// ---- mutations

func (r *root) SignUp(ctx context.Context, a struct {
	Input struct{ ShopName, ShopSlug, Email, Password string }
}) (*authR, error) {
	res, err := r.c.Account.SignUp(ctx, &accountv1.SignUpRequest{TenantName: a.Input.ShopName, TenantSlug: a.Input.ShopSlug,
		Email: a.Input.Email, Password: a.Input.Password})
	if err != nil {
		return nil, fail(err)
	}
	return &authR{res}, nil
}

type credentials struct{ ShopSlug, Email, Password string }

func (r *root) Register(ctx context.Context, a struct{ Input credentials }) (*authR, error) {
	res, err := r.c.Account.Register(ctx, &accountv1.RegisterRequest{TenantSlug: a.Input.ShopSlug, Email: a.Input.Email, Password: a.Input.Password})
	if err != nil {
		return nil, fail(err)
	}
	return &authR{res}, nil
}

func (r *root) Login(ctx context.Context, a struct{ Input credentials }) (*authR, error) {
	res, err := r.c.Account.Login(ctx, &accountv1.LoginRequest{TenantSlug: a.Input.ShopSlug, Email: a.Input.Email, Password: a.Input.Password})
	if err != nil {
		return nil, fail(err)
	}
	return &authR{res}, nil
}

func (r *root) CreateProduct(ctx context.Context, a struct {
	Input struct {
		Sku, Name    string
		Description  string
		Price        struct{ Amount, Currency string }
		InitialStock int32
	}
}) (*productR, error) {
	if err := signedIn(ctx); err != nil {
		return nil, err
	}
	price, err := money.Parse(a.Input.Price.Amount, strings.ToUpper(a.Input.Price.Currency))
	if err != nil {
		return nil, gqlError{err.Error(), "BAD_USER_INPUT"}
	}
	req := &catalogv1.CreateProductRequest{Sku: a.Input.Sku, Name: a.Input.Name, Description: a.Input.Description,
		InitialStock: a.Input.InitialStock, Price: &commonv1.Money{AmountMinor: price.Amount, Currency: price.Currency}}
	p, err := r.c.Catalog.CreateProduct(ctx, req)
	if err != nil {
		return nil, fail(err)
	}
	return &productR{p}, nil
}

func (r *root) AdjustStock(ctx context.Context, a struct {
	ProductID graphql.ID
	Delta     int32
}) (*productR, error) {
	if err := signedIn(ctx); err != nil {
		return nil, err
	}
	p, err := r.c.Catalog.AdjustStock(ctx, &catalogv1.AdjustStockRequest{ProductId: string(a.ProductID), Delta: a.Delta})
	if err != nil {
		return nil, fail(err)
	}
	return &productR{p}, nil
}

func (r *root) Reconcile(ctx context.Context, a struct{ Day string }) (*reconR, error) {
	if err := signedIn(ctx); err != nil {
		return nil, err
	}
	res, err := r.c.Payment.Reconcile(ctx, &paymentv1.ReconcileRequest{Day: a.Day})
	if err != nil {
		return nil, fail(err)
	}
	return &reconR{res}, nil
}

func (r *root) PlaceOrder(ctx context.Context, a struct {
	Input struct {
		RequestID     string
		PaymentMethod string
		Lines         []struct {
			ProductID graphql.ID
			Qty       int32
		}
	}
}) (*orderR, error) {
	if err := signedIn(ctx); err != nil {
		return nil, err
	}
	req := &orderv1.PlaceOrderRequest{RequestId: a.Input.RequestID, PaymentMethod: a.Input.PaymentMethod}
	for _, l := range a.Input.Lines {
		req.Lines = append(req.Lines, &orderv1.Line{ProductId: string(l.ProductID), Qty: l.Qty})
	}
	o, err := r.c.Order.PlaceOrder(ctx, req)
	if err != nil {
		return nil, fail(err)
	}
	return &orderR{o, r.c}, nil
}

// ---- object resolvers

type authR struct{ a *accountv1.AuthResponse }

func (r *authR) Token() string     { return r.a.Token }
func (r *authR) ExpiresAt() string { return time.Unix(r.a.ExpiresAtUnix, 0).UTC().Format(time.RFC3339) }
func (r *authR) User() *userR      { return &userR{r.a.User} }

type userR struct{ u *accountv1.User }

func (r *userR) ID() graphql.ID     { return graphql.ID(r.u.Id) }
func (r *userR) Email() string      { return r.u.Email }
func (r *userR) Role() string       { return r.u.Role }
func (r *userR) ShopID() graphql.ID { return graphql.ID(r.u.TenantId) }
func (r *userR) ShopSlug() string   { return r.u.TenantSlug }

type moneyR struct{ m money.Money }

func moneyOf(m *commonv1.Money) *moneyR {
	return &moneyR{money.Money{Amount: m.GetAmountMinor(), Currency: m.GetCurrency()}}
}
func (r *moneyR) Amount() string   { return r.m.Decimal() }
func (r *moneyR) Minor() string    { return strconv.FormatInt(r.m.Amount, 10) }
func (r *moneyR) Currency() string { return r.m.Currency }

type productR struct{ p *catalogv1.Product }

func (r *productR) ID() graphql.ID      { return graphql.ID(r.p.Id) }
func (r *productR) Sku() string         { return r.p.Sku }
func (r *productR) Name() string        { return r.p.Name }
func (r *productR) Description() string { return r.p.Description }
func (r *productR) Price() *moneyR      { return moneyOf(r.p.Price) }
func (r *productR) Available() int32    { return r.p.Available }
func (r *productR) OnHand() int32       { return r.p.OnHand }
func (r *productR) Reserved() int32     { return r.p.Reserved }

type pageR struct {
	r *catalogv1.SearchProductsResponse
}

func (r *pageR) Items() []*productR {
	out := make([]*productR, 0, len(r.r.Products))
	for _, p := range r.r.Products {
		out = append(out, &productR{p})
	}
	return out
}
func (r *pageR) NextCursor() *string {
	if r.r.NextCursor == "" {
		return nil
	}
	return &r.r.NextCursor
}

type orderR struct {
	o *orderv1.Order
	c Clients
}

func (r *orderR) ID() graphql.ID    { return graphql.ID(r.o.Id) }
func (r *orderR) RequestID() string { return r.o.RequestId }
func (r *orderR) Status() string    { return r.o.Status }
func (r *orderR) Reason() string    { return r.o.Reason }
func (r *orderR) Total() *moneyR    { return moneyOf(r.o.Total) }
func (r *orderR) History() []string { return append([]string{}, r.o.History...) }
func (r *orderR) CreatedAt() string {
	return time.Unix(r.o.CreatedAtUnix, 0).UTC().Format(time.RFC3339)
}
func (r *orderR) Lines() []*lineR {
	out := make([]*lineR, 0, len(r.o.Lines))
	for _, l := range r.o.Lines {
		out = append(out, &lineR{l})
	}
	return out
}

// Payment is resolved only when asked for, and only for admins (the payment service decides).
func (r *orderR) Payment(ctx context.Context) (*paymentR, error) {
	if p, _ := auth.From(ctx); p.Role != auth.RoleAdmin {
		return nil, nil
	}
	pm, err := r.c.Payment.GetPayment(ctx, &paymentv1.OrderRef{OrderId: r.o.Id})
	if status.Code(err) == codes.NotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fail(err)
	}
	return &paymentR{pm}, nil
}

type lineR struct{ l *orderv1.OrderLine }

func (r *lineR) ProductID() graphql.ID { return graphql.ID(r.l.ProductId) }
func (r *lineR) Sku() string           { return r.l.Sku }
func (r *lineR) Name() string          { return r.l.Name }
func (r *lineR) Qty() int32            { return r.l.Qty }
func (r *lineR) UnitPrice() *moneyR    { return moneyOf(r.l.UnitPrice) }
func (r *lineR) Subtotal() *moneyR {
	m := moneyOf(r.l.UnitPrice)
	m.m.Amount *= int64(r.l.Qty) // bounded: qty ≤ 1000 and the order total already fit
	return m
}

type paymentR struct{ p *paymentv1.Payment }

func (r *paymentR) ID() graphql.ID        { return graphql.ID(r.p.Id) }
func (r *paymentR) Status() string        { return r.p.Status }
func (r *paymentR) Amount() *moneyR       { return moneyOf(r.p.Amount) }
func (r *paymentR) FailureReason() string { return r.p.FailureReason }
func (r *paymentR) Fee() *moneyR {
	return &moneyR{money.Money{Amount: r.p.FeeMinor, Currency: r.p.GetAmount().GetCurrency()}}
}

type reconR struct{ r *paymentv1.ReconcileResponse }

func (r *reconR) RunID() graphql.ID { return graphql.ID(r.r.RunId) }
func (r *reconR) Matched() int32    { return r.r.Matched }
func (r *reconR) Breaks() []*breakR {
	out := make([]*breakR, 0, len(r.r.Breaks))
	for _, b := range r.r.Breaks {
		out = append(out, &breakR{b})
	}
	return out
}

type breakR struct{ b *paymentv1.ReconBreak }

func (r *breakR) Kind() string        { return r.b.Kind }
func (r *breakR) ChargeID() string    { return r.b.ChargeId }
func (r *breakR) LedgerMinor() string { return strconv.FormatInt(r.b.LedgerMinor, 10) }
func (r *breakR) PspMinor() string    { return strconv.FormatInt(r.b.PspMinor, 10) }
