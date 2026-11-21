// Command fakepsp runs the payment processor simulator as a standalone service.
//
//	PSP_API_KEY, PSP_WEBHOOK_URL, PSP_WEBHOOK_SECRET   required
//	PSP_FAIL_EVERY=N         answer every Nth API call with 503
//	PSP_LATENCY=200ms        add latency to every call
//	PSP_DUPLICATE_WEBHOOKS=1 send every webhook twice
package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/fakepsp"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/lab"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/svc"
)

func main() {
	log := svc.Logger("fakepsp")
	ctx, stop := svc.Context()
	defer stop()
	svc.Lab(ctx, "fakepsp")
	failEvery, _ := strconv.Atoi(svc.Env("PSP_FAIL_EVERY", "0"))
	latency, _ := time.ParseDuration(svc.Env("PSP_LATENCY", "0s"))
	refundDelay, _ := time.ParseDuration(svc.Env("PSP_REFUND_DELAY", "2s"))
	s := fakepsp.New(fakepsp.Config{
		APIKey: svc.Must("PSP_API_KEY"), WebhookURL: svc.Must("PSP_WEBHOOK_URL"), WebhookSecret: svc.Must("PSP_WEBHOOK_SECRET"),
		FailEvery: failEvery, Latency: latency, RefundDelay: refundDelay,
		DuplicateWebhooks: svc.Env("PSP_DUPLICATE_WEBHOOKS", "") != "", Log: log,
		Chaos: func() (int, time.Duration, bool) { // runtime faults from the Pattern Lab, if connected
			c := lab.Current().Chaos
			return c.PSPFailPct, time.Duration(c.PSPLatencyMs) * time.Millisecond, c.DuplicateWebhooks
		},
	})
	mux := http.NewServeMux()
	mux.Handle("/", s)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	err := svc.ServeHTTP(ctx, log, svc.Env("HTTP_ADDR", ":8080"), mux)
	s.Wait()
	if err != nil {
		svc.Fatal(log, "serve", err)
	}
}
