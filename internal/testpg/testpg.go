// Package testpg gives integration tests a fresh Postgres database with the same two-role
// setup as production: migrations run as the owner, the code under test connects as an
// application role that has no BYPASSRLS and owns nothing.
//
// Tests need TEST_PG_DSN pointing at a superuser connection, e.g.
//
//	TEST_PG_DSN=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable go test ./...
//
// (make test-db starts one in Docker). Without it the integration tests are skipped.
package testpg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io/fs"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/UchihaIthachi/go-grpc-graphql-multitenant-microservices/pkg/db"
)

const appRole = "app_test"
const appPassword = "app_test"

// DB is a migrated, throw-away database.
type DB struct {
	App   *pgxpool.Pool // connected as the RLS-bound application role
	Owner *pgx.Conn     // the owner, for tests that must reach behind the app's back
}

// New creates a database, runs the migrations in dir of fsys into schema, and returns pools.
func New(t *testing.T, fsys fs.FS, dir, schema string) *DB {
	t.Helper()
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TEST_PG_DSN not set: skipping Postgres integration test")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(func() { admin.Close(ctx) })

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "t_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create db: %v", err)
	}
	_, _ = admin.Exec(ctx, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '`+appRole+`') THEN
			CREATE ROLE `+appRole+` LOGIN PASSWORD '`+appPassword+`' NOBYPASSRLS NOSUPERUSER;
		END IF; END $$`)

	ownerURL := withDB(t, dsn, name, "", "")
	owner, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		t.Fatalf("connect owner: %v", err)
	}
	if _, err := db.Migrate(ctx, owner, fsys, dir, schema, appRole); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	app, err := db.Open(ctx, withDB(t, dsn, name, appRole, appPassword))
	if err != nil {
		t.Fatalf("connect app role: %v", err)
	}
	t.Cleanup(func() {
		app.Close()
		owner.Close(ctx)
		_, _ = admin.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})
	return &DB{App: app, Owner: owner}
}

func withDB(t *testing.T, dsn, dbname, user, pass string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("TEST_PG_DSN must be a URL: %v", err)
	}
	u.Path = "/" + dbname
	if user != "" {
		u.User = url.UserPassword(user, pass)
	}
	return u.String()
}
