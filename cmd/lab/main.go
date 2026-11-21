// Command lab runs the Pattern Lab UI and API (see internal/labserver). Development only.
//
//	LAB_TOKEN            shared with the services (they send it with their events)
//	LAB_GATEWAY_URL      GraphQL endpoint for the load generator
//	LAB_CATALOG_DB, LAB_PAYMENT_DB, LAB_ORDERS_DB   owner DSNs for invariant checks (optional)
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/labserver"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/svc"
)

func main() {
	log := svc.Logger("lab")
	ctx, stop := svc.Context()
	defer stop()
	checks := &labserver.Checker{
		Catalog: pool(ctx, log, "LAB_CATALOG_DB"),
		Payment: pool(ctx, log, "LAB_PAYMENT_DB"),
		Orders:  pool(ctx, log, "LAB_ORDERS_DB"),
	}
	s := labserver.New(svc.Must("LAB_TOKEN"), log, labserver.NewLoadGen(svc.Env("LAB_GATEWAY_URL", "http://gateway:8080/graphql"), log), checks)
	go s.Run(ctx)
	if err := svc.ServeHTTP(ctx, log, svc.Env("HTTP_ADDR", ":8090"), s.Handler()); err != nil {
		svc.Fatal(log, "serve", err)
	}
}

func pool(ctx context.Context, log *slog.Logger, env string) *pgxpool.Pool {
	dsn := os.Getenv(env)
	if dsn == "" {
		return nil
	}
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Warn("invariant checks disabled", "db", env, "err", err)
		return nil
	}
	return p
}
