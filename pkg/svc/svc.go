// Package svc is the boilerplate every binary shares: configuration from the environment,
// logging, database setup with migrations, gRPC serving with health checks, and shutdown.
package svc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/auth"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/bus"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/db"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/kafka"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/lab"
	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/outbox"
)

// Env returns the variable or def.
func Env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Must returns the variable or exits.
func Must(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "missing required environment variable %s\n", key)
		os.Exit(2)
	}
	return v
}

// Logger is a JSON logger tagged with the service name. LOG_LEVEL=debug|info|warn|error.
func Logger(service string) *slog.Logger {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(Env("LOG_LEVEL", "info")))
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})).With("service", service)
}

// Context is cancelled on SIGINT/SIGTERM.
func Context() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// Fatal logs and exits.
func Fatal(log *slog.Logger, msg string, err error) {
	log.Error(msg, "err", err)
	os.Exit(1)
}

// Migration is one schema's migrations.
type Migration struct {
	FS     fs.FS
	Dir    string
	Schema string
}

// Database migrates (as DATABASE_OWNER_URL, if set) and then connects as DATABASE_URL, the
// application role. APP_DB_ROLE names that role for the grants (default "app").
func Database(ctx context.Context, log *slog.Logger, migrations ...Migration) (*pgxpool.Pool, error) {
	if owner := os.Getenv("DATABASE_OWNER_URL"); owner != "" {
		var conn *pgx.Conn
		var err error
		for i := 0; i < 30; i++ { // Postgres may still be starting
			if conn, err = pgx.Connect(ctx, owner); err == nil {
				break
			}
			time.Sleep(time.Second)
		}
		if err != nil {
			return nil, fmt.Errorf("connect as owner: %w", err)
		}
		defer conn.Close(context.WithoutCancel(ctx))
		for _, m := range migrations {
			applied, err := db.Migrate(ctx, conn, m.FS, m.Dir, m.Schema, Env("APP_DB_ROLE", "app"))
			if err != nil {
				return nil, err
			}
			if len(applied) > 0 {
				log.Info("migrated", "schema", m.Schema, "files", applied)
			}
		}
	}
	return db.Open(ctx, Must("DATABASE_URL"))
}

// Verifier reads AUTH_PUBLIC_KEY and SERVICE_TOKEN_SECRET.
func Verifier() (auth.Verifier, error) {
	pub, err := auth.ParsePublicKey(Must("AUTH_PUBLIC_KEY"))
	if err != nil {
		return auth.Verifier{}, err
	}
	return auth.Verifier{Public: pub, ServiceSecret: []byte(os.Getenv("SERVICE_TOKEN_SECRET"))}, nil
}

// ServeGRPC serves srv on addr with the standard health service until ctx ends, then drains.
func ServeGRPC(ctx context.Context, log *slog.Logger, addr string, srv *grpc.Server) error {
	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		hs.Shutdown() // report NOT_SERVING first, so balancers stop sending new calls
		done := make(chan struct{})
		go func() { srv.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			srv.Stop()
		}
	}()
	log.Info("grpc listening", "addr", addr)
	if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}

// ServeHTTP serves h on addr until ctx ends.
func ServeHTTP(ctx context.Context, log *slog.Logger, addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("http listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Conn is a gRPC client connection.
type Conn = grpc.ClientConn

// Dial opens a client connection to a service (plaintext inside the cluster network).
func Dial(target string, ics ...grpc.UnaryClientInterceptor) (*grpc.ClientConn, error) {
	return grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithChainUnaryInterceptor(ics...))
}

// Lab connects to the Pattern Lab if LAB_URL is set (see pkg/lab).
func Lab(ctx context.Context, service string) {
	lab.Init(ctx, service, os.Getenv("LAB_URL"), os.Getenv("LAB_TOKEN"))
}

// Relay starts the outbox relay for table if KAFKA_BROKERS is set; otherwise events stay in the
// outbox (they are still written, so nothing is lost when Kafka is added later).
func Relay(ctx context.Context, log *slog.Logger, pool *pgxpool.Pool, table, clientID string) {
	brokers := os.Getenv("KAFKA_BROKERS")
	if brokers == "" {
		log.Warn("KAFKA_BROKERS not set: outbox relay disabled", "table", table)
		return
	}
	pub, err := kafka.New(strings.Split(brokers, ","), clientID)
	if err != nil {
		Fatal(log, "kafka", err)
	}
	iv, err := time.ParseDuration(Env("OUTBOX_INTERVAL", "200ms"))
	if err != nil {
		iv = 200 * time.Millisecond
	}
	go func() {
		outbox.Relay{DB: pool, Table: table, Pub: pub, Interval: iv, Log: log}.Run(ctx)
		pub.Close()
	}()
}

// Consume runs h for topics as consumer group `group` if KAFKA_BROKERS is set, restarting the
// consumer if it stops with an error. Messages that keep failing go to <topic>.dlq.
func Consume(ctx context.Context, log *slog.Logger, service, group string, topics []string, h bus.Handler) {
	brokers := os.Getenv("KAFKA_BROKERS")
	if brokers == "" {
		log.Warn("KAFKA_BROKERS not set: event consumer disabled (choreographed sagas won't progress)", "group", group)
		return
	}
	list := strings.Split(brokers, ",")
	dlq, err := kafka.New(list, service+"-dlq")
	if err != nil {
		Fatal(log, "kafka", err)
	}
	sub := kafka.Subscriber{Brokers: list, ClientID: service}
	go func() {
		defer dlq.Close()
		for ctx.Err() == nil {
			err := bus.Consume(ctx, sub, group, topics, h, bus.Options{Service: service, DLQ: dlq, Log: log})
			if err != nil && ctx.Err() == nil {
				log.Warn("consumer stopped; restarting", "group", group, "err", err)
				time.Sleep(time.Second)
			}
		}
	}()
}
