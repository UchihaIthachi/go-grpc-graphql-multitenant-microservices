// Package lab connects a service to the Pattern Lab: it reports what the service does as small
// trace events (for the live animation) and reads the lab's runtime switches (chaos settings
// now; pattern variants in later phases).
//
// Everything here is a no-op until Init is called with a lab URL, so tests and production
// builds behave exactly as if the lab did not exist. Emitting never blocks: events go into a
// bounded buffer that is flushed in batches, and are dropped if the lab can't keep up.
package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/tenancy"
)

// Node names used in events; the UI draws one box per node.
const (
	Client  = "client"
	Gateway = "gateway"
	Account = "account"
	Catalog = "catalog"
	Order   = "order"
	Payment = "payment"
	PSP     = "psp"
	Kafka   = "kafka"
)

// DB returns the node name of a service's database.
func DB(service string) string { return service + "-db" }

// Event kinds.
const (
	KindRPC        = "rpc"        // synchronous call From → To
	KindDB         = "db"         // a transaction with a visible effect
	KindState      = "state"      // a saga moved to Label
	KindRetry      = "retry"      // a step failed transiently and was rescheduled
	KindCompensate = "compensate" // the saga started undoing
	KindOutbox     = "outbox"     // an event row written in the same transaction
	KindPublish    = "publish"    // relay sent outbox rows to Kafka
	KindWebhook    = "webhook"    // processor → payment
	KindChaos      = "chaos"      // a fault the lab injected
)

// Event is one thing that happened, small enough to send by the thousand.
type Event struct {
	TS     int64  `json:"ts"` // unix ms
	Kind   string `json:"kind"`
	From   string `json:"from"`
	To     string `json:"to"`
	Label  string `json:"label"`
	OK     bool   `json:"ok"`
	Ms     int64  `json:"ms,omitempty"`
	Order  string `json:"order,omitempty"`
	Tenant string `json:"tenant,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Chaos is what the lab can break on purpose.
type Chaos struct {
	PSPFailPct        int  `json:"psp_fail_pct"`   // 0-100: processor answers 503
	PSPLatencyMs      int  `json:"psp_latency_ms"` // added to every processor call
	DuplicateWebhooks bool `json:"duplicate_webhooks"`
}

// Config is the lab's runtime switches.
type Config struct {
	Chaos    Chaos             `json:"chaos"`
	Paused   map[string]bool   `json:"paused"`   // service → answer UNAVAILABLE
	Patterns map[string]string `json:"patterns"` // switch id → option (phases B and C)
}

// Pattern returns the chosen option of a switch, or def.
func (c Config) Pattern(id, def string) string {
	if v, ok := c.Patterns[id]; ok && v != "" {
		return v
	}
	return def
}

type client struct {
	service string
	url     string
	token   string
	http    *http.Client
	cfg     atomic.Pointer[Config]

	mu  sync.Mutex
	buf []Event
}

var cur atomic.Pointer[client]

// Init connects this process to the lab at url (no-op if url is empty) and starts flushing
// events and polling switches until ctx ends.
func Init(ctx context.Context, service, url, token string) {
	if url == "" {
		return
	}
	c := &client{service: service, url: url, token: token, http: &http.Client{Timeout: 2 * time.Second}}
	c.cfg.Store(&Config{})
	cur.Store(c)
	go c.loop(ctx)
}

// Enabled reports whether the lab is connected.
func Enabled() bool { return cur.Load() != nil }

// Service is this process's node name ("" if the lab is off).
func Service() string {
	if c := cur.Load(); c != nil {
		return c.service
	}
	return ""
}

// Current returns the latest switches (zero Config if the lab is off).
func Current() Config {
	if c := cur.Load(); c != nil {
		return *c.cfg.Load()
	}
	return Config{}
}

// Paused reports whether the lab has paused this service.
func Paused() bool {
	c := cur.Load()
	return c != nil && c.cfg.Load().Paused[c.service]
}

// Emit records an event. From defaults to this service; TS to now; Order and Tenant to ctx's.
func Emit(ctx context.Context, e Event) {
	c := cur.Load()
	if c == nil {
		return
	}
	if e.TS == 0 {
		e.TS = time.Now().UnixMilli()
	}
	if e.From == "" {
		e.From = c.service
	}
	if e.Order == "" {
		e.Order = OrderFrom(ctx)
	}
	if e.Tenant == "" {
		if t, err := tenancy.From(ctx); err == nil {
			e.Tenant = string(t)
		}
	}
	c.mu.Lock()
	if len(c.buf) < 5000 {
		c.buf = append(c.buf, e)
	}
	c.mu.Unlock()
}

func (c *client) loop(ctx context.Context) {
	flush := time.NewTicker(100 * time.Millisecond)
	poll := time.NewTicker(time.Second)
	defer flush.Stop()
	defer poll.Stop()
	c.poll(ctx)
	for {
		select {
		case <-ctx.Done():
			c.flush(context.Background())
			return
		case <-flush.C:
			c.flush(ctx)
		case <-poll.C:
			c.poll(ctx)
		}
	}
}

func (c *client) flush(ctx context.Context) {
	c.mu.Lock()
	batch := c.buf
	c.buf = nil
	c.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	body, _ := json.Marshal(batch)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Lab-Token", c.token)
	if res, err := c.http.Do(req); err == nil {
		res.Body.Close()
	} // dropped on error: the lab is a viewer, never a dependency
}

func (c *client) poll(ctx context.Context) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/config", nil)
	res, err := c.http.Do(req)
	if err != nil {
		return
	}
	defer res.Body.Close()
	var cfg Config
	if res.StatusCode == http.StatusOK && json.NewDecoder(res.Body).Decode(&cfg) == nil {
		c.cfg.Store(&cfg)
	}
}

type orderKey struct{}

// WithOrder tags ctx with an order id, so every event emitted under it is linked to the order.
func WithOrder(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, orderKey{}, id)
}

// OrderFrom returns the order id in ctx, or "".
func OrderFrom(ctx context.Context) string {
	s, _ := ctx.Value(orderKey{}).(string)
	return s
}

// Since is a helper for Event.Ms.
func Since(t time.Time) int64 { return time.Since(t).Milliseconds() }

// Transport wraps an HTTP client transport so every outgoing call to node `to` (the payment
// processor) becomes an rpc event.
func Transport(to string, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return roundTripper{to: to, base: base}
}

type roundTripper struct {
	to   string
	base http.RoundTripper
}

func (rt roundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	start := time.Now()
	res, err := rt.base.RoundTrip(r)
	e := Event{Kind: KindRPC, To: rt.to, Label: pspOp(r.Method, r.URL.Path), Ms: Since(start)}
	switch {
	case err != nil:
		e.Detail = err.Error()
	case res.StatusCode >= 400:
		e.Detail = res.Status
	default:
		e.OK = true
	}
	Emit(r.Context(), e)
	return res, err
}

// pspOp names a processor call by what it does, not by its URL (which holds ids).
func pspOp(method, path string) string {
	switch {
	case strings.HasSuffix(path, "/capture"):
		return "capture"
	case strings.HasSuffix(path, "/void"):
		return "void"
	case strings.HasPrefix(path, "/v1/refunds"):
		return "refund"
	case strings.HasPrefix(path, "/v1/settlements"):
		return "settlement"
	case path == "/v1/charges" && method == http.MethodPost:
		return "authorize"
	case path == "/v1/charges":
		return "lookup charge"
	}
	return method + " " + path
}

// Incoming wraps a handler that `from` calls (processor → payment webhooks) so each request
// becomes an event of kind on this service.
func Incoming(from, kind, label string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		h.ServeHTTP(sw, r)
		Emit(r.Context(), Event{Kind: kind, From: from, To: Service(), Label: label, OK: sw.code < 300, Ms: Since(start),
			Detail: http.StatusText(sw.code)})
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}
