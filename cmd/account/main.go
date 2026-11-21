// Command account serves AccountService.
package main

import (
	"google.golang.org/grpc"

	accountv1 "github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/gen/account/v1"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/internal/account"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/auth"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/grpcx"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/svc"
)

func main() {
	log := svc.Logger("account")
	ctx, stop := svc.Context()
	defer stop()
	svc.Lab(ctx, "account")
	pool, err := svc.Database(ctx, log, svc.Migration{FS: account.Migrations, Dir: "migrations", Schema: account.Schema})
	if err != nil {
		svc.Fatal(log, "database", err)
	}
	defer pool.Close()
	priv, err := auth.ParsePrivateKey(svc.Must("AUTH_PRIVATE_KEY"))
	if err != nil {
		svc.Fatal(log, "AUTH_PRIVATE_KEY", err)
	}
	v, err := svc.Verifier()
	if err != nil {
		svc.Fatal(log, "verifier", err)
	}
	srv := grpc.NewServer(grpcx.ServerOptions(v, log, account.PublicMethods...)...)
	accountv1.RegisterAccountServiceServer(srv, account.Server{Svc: account.Service{DB: pool, Issuer: auth.Issuer{Key: priv}}})
	if err := svc.ServeGRPC(ctx, log, svc.Env("GRPC_ADDR", ":9000"), srv); err != nil {
		svc.Fatal(log, "serve", err)
	}
}
