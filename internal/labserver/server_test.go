package labserver

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/lab"
)

func TestServer(t *testing.T) {
	s := New("tok", slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	post := func(path, token string, v any) int {
		b, _ := json.Marshal(v)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(b))
		req.Header.Set("X-Lab-Token", token)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	evs := []lab.Event{{TS: 30, Kind: "state", Order: "o1", Label: "CONFIRMED"}, {TS: 10, Kind: "state", Order: "o1", Label: "PENDING"},
		{TS: 20, Kind: "rpc", Order: "o2"}}
	if c := post("/ingest", "wrong", evs); c != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", c)
	}
	if c := post("/ingest", "tok", evs); c != http.StatusNoContent {
		t.Fatalf("ingest: %d", c)
	}
	var got []lab.Event
	res, _ := http.Get(srv.URL + "/orders/o1")
	_ = json.NewDecoder(res.Body).Decode(&got)
	res.Body.Close()
	if len(got) != 2 || got[0].Label != "PENDING" {
		t.Fatalf("order events not filtered and sorted: %+v", got)
	}

	// switches are clamped and served to the services
	b, _ := json.Marshal(lab.Config{Chaos: lab.Chaos{PSPFailPct: 400, PSPLatencyMs: -5}})
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/config", bytes.NewReader(b))
	res, _ = http.DefaultClient.Do(req)
	res.Body.Close()
	var cfg lab.Config
	res, _ = http.Get(srv.URL + "/config")
	_ = json.NewDecoder(res.Body).Decode(&cfg)
	res.Body.Close()
	if cfg.Chaos.PSPFailPct != 100 || cfg.Chaos.PSPLatencyMs != 0 || cfg.Paused == nil {
		t.Fatalf("config: %+v", cfg)
	}

	// the UI is served
	res, _ = http.Get(srv.URL + "/")
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !bytes.Contains(body, []byte("Pattern Lab")) {
		t.Fatal("UI not served")
	}
}

func TestPickRespectsWeights(t *testing.T) {
	n := map[string]int{}
	for i := 0; i < 2000; i++ {
		n[pick(map[string]int{"a": 90, "b": 10, "c": 0})]++
	}
	if n["c"] != 0 || n["a"] < 1600 || n["b"] < 100 {
		t.Fatalf("weights ignored: %v", n)
	}
}
