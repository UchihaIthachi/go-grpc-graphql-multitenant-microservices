package lab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/tenancy"
)

func TestDisabledIsNoOp(t *testing.T) {
	cur.Store(nil)
	Emit(context.Background(), Event{Kind: KindRPC}) // must not panic or block
	if Enabled() || Paused() || Current().Chaos.PSPFailPct != 0 {
		t.Fatal("lab should be off")
	}
}

func TestEmitAndPoll(t *testing.T) {
	var mu sync.Mutex
	var got []Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ingest":
			if r.Header.Get("X-Lab-Token") != "tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var evs []Event
			_ = json.NewDecoder(r.Body).Decode(&evs)
			mu.Lock()
			got = append(got, evs...)
			mu.Unlock()
		case "/config":
			_ = json.NewEncoder(w).Encode(Config{Chaos: Chaos{PSPFailPct: 30}, Paused: map[string]bool{"order": true}})
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer cur.Store(nil)
	Init(ctx, "order", srv.URL, "tok")

	octx := WithOrder(tenancy.With(context.Background(), "11111111-1111-4111-8111-111111111111"), "o-1")
	Emit(octx, Event{Kind: KindState, To: DB(Order), Label: "CONFIRMED", OK: true})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 && Current().Chaos.PSPFailPct == 30 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("events: %v", got)
	}
	e := got[0]
	if e.From != "order" || e.Order != "o-1" || e.Tenant == "" || e.TS == 0 {
		t.Fatalf("defaults not filled: %+v", e)
	}
	if !Paused() || Current().Chaos.PSPFailPct != 30 {
		t.Fatalf("config not applied: %+v", Current())
	}
}

func TestPSPOpNames(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"POST", "/v1/charges"}:              "authorize",
		{"GET", "/v1/charges"}:               "lookup charge",
		{"POST", "/v1/charges/ch_1/capture"}: "capture",
		{"POST", "/v1/charges/ch_1/void"}:    "void",
		{"POST", "/v1/refunds"}:              "refund",
	} {
		if got := pspOp(in[0], in[1]); got != want {
			t.Errorf("%v: %s", in, got)
		}
	}
}
