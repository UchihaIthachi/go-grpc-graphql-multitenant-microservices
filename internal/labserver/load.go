package labserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// LoadConfig is what the UI controls.
type LoadConfig struct {
	Running bool           `json:"running"`
	RPS     float64        `json:"rps"` // orders per second
	Mix     map[string]int `json:"mix"` // payment method → weight
}

// LoadStats is what the UI shows.
type LoadStats struct {
	LoadConfig
	Shop      string         `json:"shop"`
	Placed    int64          `json:"placed"`
	Confirmed int64          `json:"confirmed"`
	Rejected  int64          `json:"rejected"`
	InFlight  int64          `json:"in_flight"` // returned before the saga finished
	Errors    int64          `json:"errors"`
	Reasons   map[string]int `json:"reasons"`
	P50Ms     int64          `json:"p50_ms"`
	P95Ms     int64          `json:"p95_ms"`
	LastError string         `json:"last_error,omitempty"`
}

// LoadGen places orders through the public GraphQL API, like real customers would.
type LoadGen struct {
	GQL  string // gateway /graphql URL
	Log  *slog.Logger
	HTTP *http.Client

	mu      sync.Mutex
	cfg     LoadConfig
	stats   LoadStats
	lat     []int64 // recent latencies, ms
	shop    *shop
	cancel  context.CancelFunc
	running bool
}

type shop struct {
	slug      string
	admin     string
	customers []string
	products  []string
	created   time.Time
}

// NewLoadGen returns a stopped generator.
func NewLoadGen(gql string, log *slog.Logger) *LoadGen {
	return &LoadGen{GQL: gql, Log: log, HTTP: &http.Client{Timeout: 15 * time.Second},
		cfg: LoadConfig{RPS: 2, Mix: map[string]int{"pm_ok": 80, "pm_decline": 10, "pm_expire_auth": 5, "pm_insufficient": 5}}}
}

// Set applies a new configuration, starting or stopping the generator.
func (g *LoadGen) Set(c LoadConfig) {
	c.RPS = max(0.2, min(c.RPS, 50))
	if len(c.Mix) == 0 {
		c.Mix = map[string]int{"pm_ok": 1}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cfg = c
	if c.Running && !g.running {
		ctx, cancel := context.WithCancel(context.Background())
		g.cancel, g.running = cancel, true
		go g.run(ctx)
	} else if !c.Running && g.running {
		g.cancel()
		g.running = false
	}
}

// Stats is a snapshot for the UI.
func (g *LoadGen) Stats() LoadStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.stats
	s.LoadConfig = g.cfg
	s.Running = g.running
	if g.shop != nil {
		s.Shop = g.shop.slug
	}
	s.Reasons = map[string]int{}
	for k, v := range g.stats.Reasons {
		s.Reasons[k] = v
	}
	if n := len(g.lat); n > 0 {
		l := slices.Clone(g.lat)
		slices.Sort(l)
		s.P50Ms, s.P95Ms = l[n/2], l[min(n-1, n*95/100)]
	}
	return s
}

func (g *LoadGen) run(ctx context.Context) {
	if err := g.ensureShop(ctx); err != nil {
		g.fail(err)
		g.mu.Lock()
		if g.running {
			g.cancel()
			g.running, g.cfg.Running = false, false
		}
		g.mu.Unlock()
		return
	}
	sem := make(chan struct{}, 64)
	next := time.Now()
	for ctx.Err() == nil {
		g.mu.Lock()
		every := time.Duration(float64(time.Second) / g.cfg.RPS)
		g.mu.Unlock()
		next = next.Add(every)
		if d := time.Until(next); d > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
		} else {
			next = time.Now() // fell behind: don't burst to catch up
		}
		select {
		case sem <- struct{}{}:
			go func() {
				defer func() { <-sem }()
				g.placeOne(ctx)
			}()
		default: // 64 orders in flight: the system is saturated; skip this tick
		}
	}
}

func (g *LoadGen) placeOne(ctx context.Context) {
	g.mu.Lock()
	sh, mix := g.shop, g.cfg.Mix
	g.mu.Unlock()
	if time.Since(sh.created) > 50*time.Minute { // tokens last an hour
		g.mu.Lock()
		g.shop = nil
		g.mu.Unlock()
		if err := g.ensureShop(ctx); err != nil {
			g.fail(err)
			return
		}
		g.mu.Lock()
		sh = g.shop
		g.mu.Unlock()
	}
	lines := []map[string]any{}
	for _, i := range mrand.Perm(len(sh.products))[:1+mrand.IntN(2)] {
		lines = append(lines, map[string]any{"productId": sh.products[i], "qty": 1 + mrand.IntN(2)})
	}
	start := time.Now()
	var res struct {
		PlaceOrder struct{ ID, Status, Reason string }
	}
	err := g.gql(ctx, sh.customers[mrand.IntN(len(sh.customers))],
		`mutation($i: PlaceOrderInput!) { placeOrder(input: $i) { id status reason } }`,
		map[string]any{"i": map[string]any{"requestId": "lab-" + randHex(8), "paymentMethod": pick(mix), "lines": lines}}, &res)
	ms := time.Since(start).Milliseconds()

	g.mu.Lock()
	defer g.mu.Unlock()
	g.stats.Placed++
	g.lat = append(g.lat, ms)
	if len(g.lat) > 500 {
		g.lat = g.lat[len(g.lat)-500:]
	}
	if g.stats.Reasons == nil {
		g.stats.Reasons = map[string]int{}
	}
	switch {
	case err != nil:
		g.stats.Errors++
		g.stats.LastError = err.Error()
	case res.PlaceOrder.Status == "CONFIRMED":
		g.stats.Confirmed++
	case res.PlaceOrder.Status == "REJECTED":
		g.stats.Rejected++
		g.stats.Reasons[reasonKey(res.PlaceOrder.Reason)]++
	default:
		g.stats.InFlight++
	}
}

func reasonKey(r string) string {
	if i := strings.IndexAny(r, ": "); i > 0 {
		return r[:i]
	}
	return r
}

func (g *LoadGen) fail(err error) {
	g.Log.Warn("load generator", "err", err)
	g.mu.Lock()
	g.stats.Errors++
	g.stats.LastError = err.Error()
	g.mu.Unlock()
}

// ensureShop signs up a fresh shop with products and customers (once per hour).
func (g *LoadGen) ensureShop(ctx context.Context) error {
	g.mu.Lock()
	have := g.shop != nil
	g.mu.Unlock()
	if have {
		return nil
	}
	sh := &shop{slug: "lab-" + randHex(4), created: time.Now()}
	var su struct{ SignUp struct{ Token string } }
	if err := g.gql(ctx, "", `mutation($i: SignUpInput!) { signUp(input: $i) { token } }`, map[string]any{"i": map[string]any{
		"shopName": "Pattern Lab shop", "shopSlug": sh.slug, "email": "owner@" + sh.slug + ".test", "password": "lab password " + randHex(4)}}, &su); err != nil {
		return fmt.Errorf("sign up: %w", err)
	}
	sh.admin = su.SignUp.Token
	for i, p := range []struct{ sku, name, price string }{
		{"MUG", "Enamel mug", "12.50"}, {"TEE", "Logo tee", "20.00"}, {"CAP", "Cap", "15.00"}, {"BAG", "Tote bag", "9.99"}} {
		var cp struct{ CreateProduct struct{ ID string } }
		if err := g.gql(ctx, sh.admin, `mutation($i: CreateProductInput!) { createProduct(input: $i) { id } }`, map[string]any{"i": map[string]any{
			"sku": p.sku, "name": p.name, "price": map[string]any{"amount": p.price, "currency": "USD"}, "initialStock": 100 + 50*i}}, &cp); err != nil {
			return fmt.Errorf("create product: %w", err)
		}
		sh.products = append(sh.products, cp.CreateProduct.ID)
	}
	for i := 0; i < 5; i++ {
		var rg struct{ Register struct{ Token string } }
		if err := g.gql(ctx, "", `mutation($i: CredentialsInput!) { register(input: $i) { token } }`, map[string]any{"i": map[string]any{
			"shopSlug": sh.slug, "email": fmt.Sprintf("customer%d@lab.test", i), "password": "customer password"}}, &rg); err != nil {
			return fmt.Errorf("register: %w", err)
		}
		sh.customers = append(sh.customers, rg.Register.Token)
	}
	g.mu.Lock()
	g.shop = sh
	g.mu.Unlock()
	g.Log.Info("load generator shop ready", "shop", sh.slug)
	return nil
}

// Restock adds n units to every product of the lab shop.
func (g *LoadGen) Restock(ctx context.Context, n int) error {
	if err := g.ensureShop(ctx); err != nil {
		return err
	}
	g.mu.Lock()
	sh := g.shop
	g.mu.Unlock()
	for _, id := range sh.products {
		if err := g.gql(ctx, sh.admin, `mutation($id: ID!, $d: Int!) { adjustStock(productId: $id, delta: $d) { id } }`,
			map[string]any{"id": id, "d": n}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (g *LoadGen) gql(ctx context.Context, token, query string, vars map[string]any, out any) error {
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, g.GQL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := g.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var r struct {
		Data   json.RawMessage
		Errors []struct {
			Message    string
			Extensions struct{ Code string }
		}
	}
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		return fmt.Errorf("HTTP %d: %w", res.StatusCode, err)
	}
	if len(r.Errors) > 0 {
		return errors.New(r.Errors[0].Extensions.Code + ": " + r.Errors[0].Message)
	}
	if out != nil {
		return json.Unmarshal(r.Data, out)
	}
	return nil
}

func pick(mix map[string]int) string {
	keys := make([]string, 0, len(mix))
	total := 0
	for k, w := range mix {
		if w > 0 {
			keys = append(keys, k)
			total += w
		}
	}
	if total == 0 {
		return "pm_ok"
	}
	slices.Sort(keys)
	n := mrand.IntN(total)
	for _, k := range keys {
		if n -= mix[k]; n < 0 {
			return k
		}
	}
	return keys[len(keys)-1]
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
