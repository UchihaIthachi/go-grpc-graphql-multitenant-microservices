// Command gateway serves the public GraphQL API on /graphql.
package main

import (
	"net/http"

	accountv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/account/v1"
	catalogv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/catalog/v1"
	orderv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/order/v1"
	paymentv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/payment/v1"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/gateway"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/grpcx"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/lab"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/svc"
)

func main() {
	log := svc.Logger("gateway")
	ctx, stop := svc.Context()
	defer stop()
	svc.Lab(ctx, "gateway")
	v, err := svc.Verifier()
	if err != nil {
		svc.Fatal(log, "verifier", err)
	}
	fwd := grpcx.ForwardUser()
	dial := func(env, node string) *svc.Conn {
		cc, err := svc.Dial(svc.Must(env), fwd, grpcx.Traced(node))
		if err != nil {
			svc.Fatal(log, "dial "+env, err)
		}
		return cc
	}
	c := gateway.Clients{
		Account: accountv1.NewAccountServiceClient(dial("ACCOUNT_ADDR", lab.Account)),
		Catalog: catalogv1.NewCatalogServiceClient(dial("CATALOG_ADDR", lab.Catalog)),
		Order:   orderv1.NewOrderServiceClient(dial("ORDER_ADDR", lab.Order)),
		Payment: paymentv1.NewPaymentServiceClient(dial("PAYMENT_ADDR", lab.Payment)),
	}
	mux := http.NewServeMux()
	mux.Handle("/graphql", gateway.Handler(c, v, log))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	if err := svc.ServeHTTP(ctx, log, svc.Env("HTTP_ADDR", ":8080"), mux); err != nil {
		svc.Fatal(log, "serve", err)
	}
}
