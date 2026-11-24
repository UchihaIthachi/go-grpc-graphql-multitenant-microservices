// Command order serves OrderService, runs saga recovery, and relays order events to Kafka.
package main

import (
	"time"

	"google.golang.org/grpc"

	catalogv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/catalog/v1"
	orderv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/order/v1"
	paymentv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/payment/v1"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/order"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/order/grpcdeps"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/grpcx"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/lab"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/svc"
)

func main() {
	log := svc.Logger("order")
	ctx, stop := svc.Context()
	defer stop()
	svc.Lab(ctx, "order")
	pool, err := svc.Database(ctx, log, svc.Migration{FS: order.Migrations, Dir: "migrations", Schema: order.Schema})
	if err != nil {
		svc.Fatal(log, "database", err)
	}
	defer pool.Close()
	v, err := svc.Verifier()
	if err != nil {
		svc.Fatal(log, "verifier", err)
	}
	asOrder := grpcx.AsService("order", []byte(svc.Must("SERVICE_TOKEN_SECRET")))
	cat, err := svc.Dial(svc.Must("CATALOG_ADDR"), asOrder, grpcx.Traced(lab.Catalog))
	if err != nil {
		svc.Fatal(log, "dial catalog", err)
	}
	pay, err := svc.Dial(svc.Must("PAYMENT_ADDR"), asOrder, grpcx.Traced(lab.Payment))
	if err != nil {
		svc.Fatal(log, "dial payment", err)
	}
	engine := order.Engine{DB: pool, Log: log,
		Catalog:  grpcdeps.Catalog{C: catalogv1.NewCatalogServiceClient(cat)},
		Payments: grpcdeps.Payments{C: paymentv1.NewPaymentServiceClient(pay)},
		// saga style for new orders: the Pattern Lab's switch, else SAGA_MODE, else orchestration
		Mode: func() string { return lab.Current().Pattern("saga", svc.Env("SAGA_MODE", order.Orchestration)) }}
	go engine.RunRecovery(ctx, time.Second)
	svc.Relay(ctx, log, pool, "orders.outbox", "order")
	svc.Consume(ctx, log, "order", order.ConsumerGroup, order.ConsumeTopics, engine.HandleEvent)

	srv := grpc.NewServer(grpcx.ServerOptions(v, log)...)
	orderv1.RegisterOrderServiceServer(srv, order.Server{Engine: engine})
	if err := svc.ServeGRPC(ctx, log, svc.Env("GRPC_ADDR", ":9000"), srv); err != nil {
		svc.Fatal(log, "serve", err)
	}
}
