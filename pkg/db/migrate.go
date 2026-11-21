package db

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Migrate applies the *.sql files in dir of fsys, in name order, each in its own transaction,
// recording them in <schema>.schema_migrations. An advisory lock keyed on the schema makes
// concurrent starts of the same service safe: one migrates, the others wait and then skip.
//
// Migrations run as the schema owner (a separate DSN from the app role), and each file may use
// {{APP_ROLE}}, replaced with appRole, to grant the application role its privileges.
func Migrate(ctx context.Context, conn *pgx.Conn, fsys fs.FS, dir, schema, appRole string) (applied []string, err error) {
	files, err := fs.Glob(fsys, path.Join(dir, "*.sql"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, "migrate:"+schema); err != nil {
		return nil, err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext($1))`, "migrate:"+schema)

	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s;
		CREATE TABLE IF NOT EXISTS %s.schema_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`, ident, ident)); err != nil {
		return nil, fmt.Errorf("migrate: bookkeeping: %w", err)
	}

	for _, f := range files {
		name := path.Base(f)
		var done bool
		if err := conn.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s.schema_migrations WHERE name = $1)`, ident), name).Scan(&done); err != nil {
			return applied, err
		}
		if done {
			continue
		}
		body, err := fs.ReadFile(fsys, f)
		if err != nil {
			return applied, err
		}
		sql := strings.ReplaceAll(string(body), "{{APP_ROLE}}", pgx.Identifier{appRole}.Sanitize())
		tx, err := conn.Begin(ctx)
		if err != nil {
			return applied, err
		}
		if _, err := tx.Exec(ctx, sql); err != nil {
			_ = tx.Rollback(ctx)
			return applied, fmt.Errorf("migrate %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.schema_migrations (name) VALUES ($1)`, ident), name); err != nil {
			_ = tx.Rollback(ctx)
			return applied, err
		}
		if err := tx.Commit(ctx); err != nil {
			return applied, err
		}
		applied = append(applied, name)
	}
	return applied, nil
}
