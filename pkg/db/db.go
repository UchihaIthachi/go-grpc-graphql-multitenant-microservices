// Package db opens Postgres pools and runs every tenant-scoped unit of work inside a
// transaction that sets app.tenant_id, which the row-level security policies check.
//
// Services connect as an application role that does NOT own the tables and does not have
// BYPASSRLS, and every table has FORCE ROW LEVEL SECURITY. So even a query that forgets
// "WHERE tenant_id = ..." only ever sees the current tenant's rows.
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/tenancy"
)

// Open connects a pool and checks it with a ping.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: parse dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// Beginner is satisfied by *pgxpool.Pool and pgx.Tx.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// InTenantTx runs fn in a transaction scoped to the tenant in ctx. A context without a tenant
// is refused before touching the database. Serialization failures and deadlocks are retried
// (up to 3 attempts), so fn must be safe to run again: do side effects only through tx.
func InTenantTx(ctx context.Context, b Beginner, fn func(pgx.Tx) error) error {
	tid, err := tenancy.From(ctx)
	if err != nil {
		return err
	}
	return inTx(ctx, b, func(tx pgx.Tx) error {
		// set_config(..., is_local => true) is SET LOCAL: it ends with the transaction, so a
		// pooled connection can never leak one tenant's setting into the next request.
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, string(tid)); err != nil {
			return fmt.Errorf("db: set tenant: %w", err)
		}
		return fn(tx)
	})
}

// InTx runs fn in a plain transaction (no tenant), for cross-tenant system work such as the
// outbox relay. Use sparingly: RLS hides every tenant-scoped row from it.
func InTx(ctx context.Context, b Beginner, fn func(pgx.Tx) error) error {
	return inTx(ctx, b, fn)
}

func inTx(ctx context.Context, b Beginner, fn func(pgx.Tx) error) error {
	const attempts = 3
	var err error
	for i := 0; i < attempts; i++ {
		if err = once(ctx, b, fn); err == nil || !retryable(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(i+1) * 15 * time.Millisecond):
		}
	}
	return err
}

func once(ctx context.Context, b Beginner, fn func(pgx.Tx) error) (err error) {
	tx, err := b.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SQLSTATE codes used across services.
const (
	UniqueViolation      = "23505"
	ForeignKeyViolation  = "23503"
	CheckViolation       = "23514"
	SerializationFailure = "40001"
	DeadlockDetected     = "40P01"
	RaiseException       = "P0001"
)

// Code returns the SQLSTATE of a Postgres error, or "".
func Code(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func retryable(err error) bool {
	c := Code(err)
	return c == SerializationFailure || c == DeadlockDetected
}
