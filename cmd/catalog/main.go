// Command catalog serves CatalogService and sweeps expired stock reservations.
package main

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/grpc"

	catalogv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/catalog/v1"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/catalog"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/grpcx"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/svc"
)

func main() {
	log := svc.Logger("catalog")
	ctx, stop := svc.Context()
	defer stop()
	svc.Lab(ctx, "catalog")
	pool, err := svc.Database(ctx, log, svc.Migration{FS: catalog.Migrations, Dir: "migrations", Schema: catalog.Schema})
	if err != nil {
		svc.Fatal(log, "database", err)
	}
	defer pool.Close()
	v, err := svc.Verifier()
	if err != nil {
		svc.Fatal(log, "verifier", err)
	}
	ttl, _ := time.ParseDuration(svc.Env("RESERVATION_TTL", "15m"))
	store := catalog.Store{DB: pool, TTL: ttl}
	go sweep(ctx, log, store)

	srv := grpc.NewServer(grpcx.ServerOptions(v, log)...)
	catalogv1.RegisterCatalogServiceServer(srv, catalog.Server{Store: store})
	if err := svc.ServeGRPC(ctx, log, svc.Env("GRPC_ADDR", ":9000"), srv); err != nil {
		svc.Fatal(log, "serve", err)
	}
}

// sweep hands expired reservations' units back to stock.
func sweep(ctx context.Context, log *slog.Logger, s catalog.Store) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if n, err := s.ExpireReservations(ctx, 500); err != nil && ctx.Err() == nil {
			log.Warn("expire reservations", "err", err)
		} else if n > 0 {
			log.Info("released expired reservations", "products", n)
		}
	}
}
