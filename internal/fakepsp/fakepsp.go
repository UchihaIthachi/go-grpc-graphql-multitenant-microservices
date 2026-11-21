// Package fakepsp is a small card processor simulator, so the payment flow can be exercised end
// to end without a real processor or any money.
//
// Behaviour is chosen by the payment method token, like processor test cards:
//
//	pm_ok            approved
//	pm_decline       declined: "card_declined"
//	pm_insufficient  declined: "insufficient_funds"
//	pm_expire_auth   approved, but capture fails: "authorization_expired"
//	pm_refund_fails  approved; refunds end with a refund.failed webhook
//
// It honours Idempotency-Key on every POST (same key → same stored response), confirms refunds
// asynchronously with HMAC-signed webhooks, and publishes a daily settlement statement with fees
// (2.9% + 30 minor units). Chaos switches: FailEvery (every Nth request answers 503), Latency,
// DuplicateWebhooks (every webhook sent twice).
package fakepsp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	mrand "math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/payment/psp"
)

// Config tunes the simulator.
type Config struct {
	APIKey            string
	WebhookURL        string // where signed events are POSTed; empty disables webhooks
	WebhookSecret     string
	RefundDelay       time.Duration // time until refund.succeeded / refund.failed
	FailEvery         int           // answer every Nth API request with 503 (0 = never)
	Latency           time.Duration
	DuplicateWebhooks bool
	Now               func() time.Time
	Log               *slog.Logger
	// Chaos, if set, is read on every request and overrides the static fault settings, so a
	// controller (the Pattern Lab) can change them while the simulator runs.
	Chaos func() (failPct int, latency time.Duration, duplicateWebhooks bool)
}

type stored struct {
	status int
	body   []byte
}

type settled struct {
	day  string
	line psp.SettlementLine
}

// Server is the simulator; it is an http.Handler.
type Server struct {
	cfg  Config
	mu   sync.Mutex
	n    int
	ch   map[string]*psp.Charge
	pm   map[string]string // charge id → payment method
	keys map[string]stored // idempotency key (+ path) → response
	auth map[string]string // authorize idempotency key → charge id
	stmt []settled
	mux  *http.ServeMux
	wg   sync.WaitGroup
}

// New returns a simulator.
func New(cfg Config) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	s := &Server{cfg: cfg, ch: map[string]*psp.Charge{}, pm: map[string]string{}, keys: map[string]stored{}, auth: map[string]string{}}
	m := http.NewServeMux()
	m.HandleFunc("POST /v1/charges", s.idem(s.authorize))
	m.HandleFunc("GET /v1/charges", s.chargeByKey)
	m.HandleFunc("POST /v1/charges/{id}/capture", s.idem(s.capture))
	m.HandleFunc("POST /v1/charges/{id}/void", s.idem(s.void))
	m.HandleFunc("POST /v1/refunds", s.idem(s.refund))
	m.HandleFunc("GET /v1/settlements", s.settlement)
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	s.mux = m
	return s
}

// Charges returns how many distinct charges exist (tests: proves idempotency).
func (s *Server) Charges() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ch)
}

// Charge returns a copy of one charge (tests).
func (s *Server) Charge(id string) (psp.Charge, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.ch[id]
	if !ok {
		return psp.Charge{}, false
	}
	return *c, true
}

// Wait blocks until all scheduled webhooks were sent (tests).
func (s *Server) Wait() { s.wg.Wait() }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/healthz" {
		if r.Header.Get("Authorization") != "Bearer "+s.cfg.APIKey {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "bad API key")
			return
		}
		latency := s.cfg.Latency
		s.mu.Lock()
		s.n++
		fail := s.cfg.FailEvery > 0 && s.n%s.cfg.FailEvery == 0
		if s.cfg.Chaos != nil {
			pct, lat, _ := s.cfg.Chaos()
			fail = fail || (pct > 0 && mrand.IntN(100) < pct)
			latency += lat
		}
		s.mu.Unlock()
		if latency > 0 {
			time.Sleep(latency)
		}
		if fail {
			writeErr(w, http.StatusServiceUnavailable, "unavailable", "chaos: injected failure")
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

// idem replays the stored response for a repeated Idempotency-Key.
func (s *Server) idem(h func(*http.Request) (int, any)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			writeErr(w, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header required")
			return
		}
		k := r.URL.Path + "|" + key
		s.mu.Lock()
		defer s.mu.Unlock()
		if st, ok := s.keys[k]; ok {
			w.Header().Set("Idempotent-Replayed", "true")
			writeRaw(w, st.status, st.body)
			return
		}
		status, v := h(r)
		b, _ := json.Marshal(v)
		if status < 500 {
			s.keys[k] = stored{status, b}
		}
		if status == http.StatusCreated && strings.HasSuffix(r.URL.Path, "/charges") {
			s.auth[key] = v.(psp.Charge).ID
		}
		writeRaw(w, status, b)
	}
}

func (s *Server) authorize(r *http.Request) (int, any) {
	var in psp.AuthorizeRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Amount <= 0 || len(in.Currency) != 3 {
		return http.StatusBadRequest, apiErr("invalid_request", "amount > 0 and currency required")
	}
	c := &psp.Charge{ID: "ch_" + rid(), Amount: in.Amount, Currency: in.Currency, Status: psp.Authorized, Metadata: in.Metadata}
	switch in.PaymentMethod {
	case "pm_decline":
		c.Status, c.DeclineReason = psp.Declined, "card_declined"
	case "pm_insufficient":
		c.Status, c.DeclineReason = psp.Declined, "insufficient_funds"
	}
	s.ch[c.ID] = c
	s.pm[c.ID] = in.PaymentMethod
	return http.StatusCreated, *c
}

func (s *Server) chargeByKey(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.auth[r.URL.Query().Get("idempotency_key")]
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no charge for that key")
		return
	}
	b, _ := json.Marshal(s.ch[id])
	writeRaw(w, http.StatusOK, b)
}

func (s *Server) capture(r *http.Request) (int, any) {
	c, ok := s.ch[r.PathValue("id")]
	if !ok {
		return http.StatusNotFound, apiErr("not_found", "no such charge")
	}
	var in struct{ Amount int64 }
	_ = json.NewDecoder(r.Body).Decode(&in)
	switch {
	case c.Status == psp.Captured:
		return http.StatusOK, *c
	case c.Status != psp.Authorized:
		return http.StatusConflict, apiErr("invalid_state", "charge is "+c.Status)
	case s.pm[c.ID] == "pm_expire_auth":
		return http.StatusPaymentRequired, apiErr("authorization_expired", "the authorization expired")
	case in.Amount <= 0 || in.Amount > c.Amount:
		return http.StatusBadRequest, apiErr("invalid_amount", "capture 1..authorized amount")
	}
	c.Status, c.Captured, c.Fee = psp.Captured, in.Amount, fee(in.Amount)
	s.stmt = append(s.stmt, settled{s.cfg.Now().UTC().Format("2006-01-02"), psp.SettlementLine{
		Type: "capture", ChargeID: c.ID, Amount: c.Captured, Fee: c.Fee, Currency: c.Currency, Metadata: c.Metadata}})
	return http.StatusOK, *c
}

func (s *Server) void(r *http.Request) (int, any) {
	c, ok := s.ch[r.PathValue("id")]
	if !ok {
		return http.StatusNotFound, apiErr("not_found", "no such charge")
	}
	switch c.Status {
	case psp.Authorized:
		c.Status = psp.Voided
	case psp.Voided, psp.Declined:
	default:
		return http.StatusConflict, apiErr("invalid_state", "charge is "+c.Status+"; refund instead")
	}
	return http.StatusOK, *c
}

func (s *Server) refund(r *http.Request) (int, any) {
	var in struct {
		Charge string `json:"charge"`
		Amount int64  `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return http.StatusBadRequest, apiErr("invalid_request", err.Error())
	}
	c, ok := s.ch[in.Charge]
	if !ok {
		return http.StatusNotFound, apiErr("not_found", "no such charge")
	}
	if c.Status != psp.Captured || in.Amount <= 0 || in.Amount > c.Captured {
		return http.StatusConflict, apiErr("invalid_state", "only captured amounts can be refunded")
	}
	rf := psp.Refund{ID: "re_" + rid(), ChargeID: c.ID, Amount: in.Amount, Status: "pending"}
	ok2 := s.pm[c.ID] != "pm_refund_fails"
	if ok2 {
		s.stmt = append(s.stmt, settled{s.cfg.Now().UTC().Format("2006-01-02"), psp.SettlementLine{
			Type: "refund", ChargeID: c.ID, RefundID: rf.ID, Amount: -in.Amount, Currency: c.Currency, Metadata: c.Metadata}})
	}
	typ := "refund.succeeded"
	if !ok2 {
		typ = "refund.failed"
	}
	s.webhook(psp.Event{ID: "evt_" + rid(), Type: typ, Data: psp.EventData{
		ChargeID: c.ID, RefundID: rf.ID, Amount: in.Amount, Currency: c.Currency, Metadata: c.Metadata}})
	return http.StatusCreated, rf
}

func (s *Server) settlement(w http.ResponseWriter, r *http.Request) {
	day := r.URL.Query().Get("date")
	s.mu.Lock()
	var lines []psp.SettlementLine
	for _, st := range s.stmt {
		if st.day == day {
			lines = append(lines, st.line)
		}
	}
	s.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"date": day, "lines": lines})
	writeRaw(w, http.StatusOK, b)
}

// InjectSettlementLine adds a line the payment service never saw (reconciliation tests).
func (s *Server) InjectSettlementLine(day time.Time, l psp.SettlementLine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stmt = append(s.stmt, settled{day.UTC().Format("2006-01-02"), l})
}

// webhook sends ev after RefundDelay, signed; must be called with s.mu held.
func (s *Server) webhook(ev psp.Event) {
	if s.cfg.WebhookURL == "" {
		return
	}
	ev.Created = s.cfg.Now().Unix()
	body, _ := json.Marshal(ev)
	times := 1
	dup := s.cfg.DuplicateWebhooks
	if s.cfg.Chaos != nil {
		_, _, d := s.cfg.Chaos()
		dup = dup || d
	}
	if dup {
		times = 2
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		time.Sleep(s.cfg.RefundDelay)
		for i := 0; i < times; i++ {
			s.send(body)
		}
	}()
}

func (s *Server) send(body []byte) {
	// retry with backoff like a real processor: 5 attempts
	for attempt, wait := 0, 100*time.Millisecond; attempt < 5; attempt, wait = attempt+1, wait*2 {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, s.cfg.WebhookURL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(psp.SignatureHeader, psp.Sign(s.cfg.WebhookSecret, body, s.cfg.Now()))
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				return
			}
		}
		s.cfg.Log.Warn("fakepsp webhook delivery failed", "attempt", attempt+1, "err", err)
		time.Sleep(wait)
	}
}

func fee(amount int64) int64 { return (amount*29+500)/1000 + 30 } // 2.9% rounded + 0.30

func rid() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func apiErr(code, msg string) map[string]string {
	return map[string]string{"code": code, "message": msg}
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	b, _ := json.Marshal(apiErr(code, msg))
	writeRaw(w, status, b)
}

func writeRaw(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}
