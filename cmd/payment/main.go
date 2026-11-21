// Command payment serves PaymentService over gRPC and the processor's webhooks over HTTP,
// and relays payment events to Kafka.
package main

import (
	"net/http"
	"time"

	"google.golang.org/grpc"

	paymentv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/payment/v1"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/payment"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/payment/ledger"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/payment/psp"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/grpcx"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/lab"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/svc"
)

func main() {
	log := svc.Logger("payment")
	ctx, stop := svc.Context()
	defer stop()
	svc.Lab(ctx, "payment")
	pool, err := svc.Database(ctx, log,
		svc.Migration{FS: ledger.Migrations, Dir: "migrations", Schema: ledger.Schema},
		svc.Migration{FS: payment.Migrations, Dir: "migrations", Schema: payment.Schema})
	if err != nil {
		svc.Fatal(log, "database", err)
	}
	defer pool.Close()
	v, err := svc.Verifier()
	if err != nil {
		svc.Fatal(log, "verifier", err)
	}
	s := payment.Service{DB: pool, PSP: psp.HTTPClient{BaseURL: svc.Must("PSP_URL"), APIKey: svc.Must("PSP_API_KEY"),
		HTTP: &http.Client{Timeout: 5 * time.Second, Transport: lab.Transport(lab.PSP, nil)}}}
	svc.Relay(ctx, log, pool, "payment.outbox", "payment")

	mux := http.NewServeMux()
	mux.Handle("POST /webhooks/psp", lab.Incoming(lab.PSP, lab.KindWebhook, "webhook",
		payment.WebhookHandler(s, svc.Must("PSP_WEBHOOK_SECRET"), log)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	go func() {
		if err := svc.ServeHTTP(ctx, log, svc.Env("HTTP_ADDR", ":8080"), mux); err != nil {
			svc.Fatal(log, "http", err)
		}
	}()

	srv := grpc.NewServer(grpcx.ServerOptions(v, log)...)
	paymentv1.RegisterPaymentServiceServer(srv, payment.Server{Svc: s})
	if err := svc.ServeGRPC(ctx, log, svc.Env("GRPC_ADDR", ":9000"), srv); err != nil {
		svc.Fatal(log, "serve", err)
	}
}
