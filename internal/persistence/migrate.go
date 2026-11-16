// Package persistence applies the SQL migrations in migrations/.
package persistence

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLockID is the PostgreSQL advisory lock key held while migrating.
const migrationLockID = 72_656_176_000_001

// Migrate applies migrations from ./migrations (relative to the working directory).
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return MigrateDir(ctx, pool, "migrations")
}

// MigrateDir applies every *.sql file in dir, in lexical order, exactly once. A
// session advisory lock, held on one dedicated connection, serializes API instances
// that start at the same time.
func MigrateDir(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	db, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer db.Release()
	if _, err = db.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer func() { _, _ = db.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockID) }()
	if _, err = db.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	for _, file := range files {
		version := filepath.Base(file)
		var applied bool
		if err = db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, version).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if applied {
			continue
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", version, err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", version, err)
		}
		if _, err = tx.Exec(ctx, string(b)); err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES($1)`, version)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
		if err = tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", version, err)
		}
	}
	return nil
}
