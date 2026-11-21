// Package labserver is the Pattern Lab: it collects trace events from every service, streams
// them to the browser for the live animation, holds the runtime switches the services poll,
// drives load through the real GraphQL API, and checks the system's invariants against the
// real databases.
//
// It is a development tool. It reads every tenant's data (as the owner role) to check
// invariants, so it must never be deployed next to production data.
package labserver

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/lab"
)

//go:embed ui
var uiFS embed.FS

const ringSize = 20000

// Server is the lab's HTTP API and state.
type Server struct {
	Token  string // services send it with /ingest
	Log    *slog.Logger
	Load   *LoadGen
	Checks *Checker

	mu     sync.Mutex
	cfg    lab.Config
	ring   []lab.Event // newest last
	subs   map[chan []byte]struct{}
	counts map[string]int64 // kind → total, for the stats panel
}

// New returns a server with default switches.
func New(token string, log *slog.Logger, load *LoadGen, checks *Checker) *Server {
	return &Server{Token: token, Log: log, Load: load, Checks: checks,
		cfg:  lab.Config{Paused: map[string]bool{}, Patterns: map[string]string{}},
		subs: map[chan []byte]struct{}{}, counts: map[string]int64{}}
}

// Handler serves the UI and the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	ui, _ := fs.Sub(uiFS, "ui")
	mux.Handle("GET /", http.FileServerFS(ui))
	mux.HandleFunc("POST /ingest", s.ingest)
	mux.HandleFunc("GET /config", s.getConfig)
	mux.HandleFunc("PUT /config", s.putConfig)
	mux.HandleFunc("GET /stream", s.stream)
	mux.HandleFunc("GET /orders/{id}", s.orderEvents)
	mux.HandleFunc("GET /load", s.getLoad)
	mux.HandleFunc("PUT /load", s.putLoad)
	mux.HandleFunc("POST /load/restock", s.restock)
	mux.HandleFunc("GET /invariants", s.getInvariants)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	return mux
}

// Run pushes invariants and load stats to subscribers until ctx ends.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if s.Checks != nil {
			s.broadcast("inv", s.Checks.Run(ctx))
		}
		if s.Load != nil {
			s.broadcast("load", s.Load.Stats())
		}
	}
}

func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Lab-Token")), []byte(s.Token)) != 1 {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	var evs []lab.Event
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&evs); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	s.Add(evs)
	w.WriteHeader(http.StatusNoContent)
}

// Add records events and streams them.
func (s *Server) Add(evs []lab.Event) {
	s.mu.Lock()
	s.ring = append(s.ring, evs...)
	if over := len(s.ring) - ringSize; over > 0 {
		s.ring = append([]lab.Event(nil), s.ring[over:]...)
	}
	for _, e := range evs {
		s.counts[e.Kind]++
	}
	s.mu.Unlock()
	s.broadcast("ev", evs)
}

func (s *Server) broadcast(kind string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	msg := append([]byte("event: "+kind+"\ndata: "), b...)
	msg = append(msg, '\n', '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- msg:
		default: // slow browser: drop rather than block the lab
		}
	}
}

// stream is Server-Sent Events: ev (event batches), cfg, inv, load.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := make(chan []byte, 256)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	cfg := s.cfg
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()
	b, _ := json.Marshal(cfg)
	w.Write([]byte("event: cfg\ndata: " + string(b) + "\n\n"))
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case m := <-ch:
			w.Write(m)
			fl.Flush()
		case <-ping.C:
			w.Write([]byte(": ping\n\n"))
			fl.Flush()
		}
	}
}

func (s *Server) getConfig(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	writeJSON(w, cfg)
}

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	var cfg lab.Config
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&cfg); err != nil {
		http.Error(w, "bad config", http.StatusBadRequest)
		return
	}
	cfg.Chaos.PSPFailPct = clamp(cfg.Chaos.PSPFailPct, 0, 100)
	cfg.Chaos.PSPLatencyMs = clamp(cfg.Chaos.PSPLatencyMs, 0, 10000)
	if cfg.Paused == nil {
		cfg.Paused = map[string]bool{}
	}
	if cfg.Patterns == nil {
		cfg.Patterns = map[string]string{}
	}
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	s.Log.Info("switches changed", "config", cfg)
	s.Add([]lab.Event{{TS: time.Now().UnixMilli(), Kind: lab.KindChaos, From: "lab", To: "lab", Label: "switches changed", OK: true}})
	s.broadcast("cfg", cfg)
	writeJSON(w, cfg)
}

// orderEvents returns everything recorded for one order, oldest first (the replay view).
func (s *Server) orderEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	var out []lab.Event
	for _, e := range s.ring {
		if e.Order == id {
			out = append(out, e)
		}
	}
	s.mu.Unlock()
	// services flush on their own schedules, so arrival order isn't event order
	sort.SliceStable(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	writeJSON(w, out)
}

func (s *Server) getLoad(w http.ResponseWriter, _ *http.Request) {
	if s.Load == nil {
		http.Error(w, "no load generator", http.StatusNotFound)
		return
	}
	writeJSON(w, s.Load.Stats())
}

func (s *Server) putLoad(w http.ResponseWriter, r *http.Request) {
	if s.Load == nil {
		http.Error(w, "no load generator", http.StatusNotFound)
		return
	}
	var c LoadConfig
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&c); err != nil {
		http.Error(w, "bad load config", http.StatusBadRequest)
		return
	}
	s.Load.Set(c)
	writeJSON(w, s.Load.Stats())
}

func (s *Server) restock(w http.ResponseWriter, r *http.Request) {
	if s.Load == nil {
		http.Error(w, "no load generator", http.StatusNotFound)
		return
	}
	if err := s.Load.Restock(r.Context(), 50); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getInvariants(w http.ResponseWriter, r *http.Request) {
	if s.Checks == nil {
		writeJSON(w, []Check{})
		return
	}
	writeJSON(w, s.Checks.Run(r.Context()))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func clamp(v, lo, hi int) int { return max(lo, min(v, hi)) }
