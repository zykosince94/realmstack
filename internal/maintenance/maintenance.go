// Package maintenance runs the retention jobs: create ledger partitions
// ahead of time, delete expired idempotency records in batches, and drop old
// rate-limit windows. Every API instance runs the loop; a transaction-scoped advisory
// lock picks one instance per task run. Transaction-scoped (not session) locks keep it
// working through PgBouncer in transaction mode, where a session is not stable.
package maintenance

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"realmstack-backend/internal/limits"
)

// MonthsAhead is how far ahead monthly ledger partitions are created.
const MonthsAhead = 3

// EnsurePartitions creates any missing monthly partitions from last month through
// MonthsAhead months ahead, returning how many it created.
func EnsurePartitions(ctx context.Context, db *pgxpool.Pool) (int, error) {
	var created int
	err := db.QueryRow(ctx, `SELECT realmstack_ensure_partitions((date_trunc('month', now()) - interval '1 month')::date, $1)`, MonthsAhead).Scan(&created)
	return created, err
}

// PurgeIdempotency deletes expired idempotency records in batches of batchSize, each
// in its own short transaction, so purging never holds long locks on a hot table.
func PurgeIdempotency(ctx context.Context, db *pgxpool.Pool, batchSize int) (int64, error) {
	var total int64
	for {
		tag, err := db.Exec(ctx, `DELETE FROM idempotency_records WHERE ctid IN (SELECT ctid FROM idempotency_records WHERE expires_at < now() LIMIT $1)`, batchSize)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < int64(batchSize) || ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
}

// PurgeRefreshTokens deletes player and dashboard refresh tokens that expired more than
// a day ago. Revoked but unexpired rows stay: reuse detection needs them until they
// would have expired anyway. Without this the tables grow by one row per refresh.
func PurgeRefreshTokens(ctx context.Context, db *pgxpool.Pool) (int64, error) {
	var total int64
	for _, table := range []string{"refresh_tokens", "dashboard_refresh_tokens"} {
		// Rotation links rows (replaced_by), so unlink the expired ones before deleting.
		if _, err := db.Exec(ctx, `UPDATE `+table+` SET replaced_by=NULL WHERE replaced_by IN (SELECT id FROM `+table+` WHERE expires_at < now() - interval '1 day')`); err != nil {
			return total, err
		}
		tag, err := db.Exec(ctx, `DELETE FROM `+table+` WHERE expires_at < now() - interval '1 day'`)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
	}
	return total, nil
}

// exclusive runs fn only if this instance wins the named lock for this run.
func exclusive(ctx context.Context, db *pgxpool.Pool, name string, fn func() error) (bool, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var won bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtext($1))`, "realmstack-maintenance:"+name).Scan(&won); err != nil || !won {
		return false, err
	}
	// The lock lives as long as tx; fn uses the pool, so its batches stay short.
	if err = fn(); err != nil {
		return true, err
	}
	return true, tx.Commit(ctx)
}

// RunOnce performs every task once (cmd/realmstack-admin and the loop share it).
func RunOnce(ctx context.Context, db *pgxpool.Pool, log *slog.Logger) {
	run := func(name string, fn func() error) {
		if _, err := exclusive(ctx, db, name, fn); err != nil && ctx.Err() == nil {
			log.Error("maintenance task failed", "task", name, "error", err)
		}
	}
	run("partitions", func() error {
		n, err := EnsurePartitions(ctx, db)
		if n > 0 {
			log.Info("created ledger partitions", "count", n)
		}
		return err
	})
	run("idempotency", func() error {
		n, err := PurgeIdempotency(ctx, db, 5000)
		if n > 0 {
			log.Info("purged expired idempotency records", "count", n)
		}
		return err
	})
	run("refresh-tokens", func() error {
		n, err := PurgeRefreshTokens(ctx, db)
		if n > 0 {
			log.Info("purged expired refresh tokens", "count", n)
		}
		return err
	})
	run("rate-limits", func() error {
		_, err := limits.Purge(ctx, db, 10*time.Minute, time.Now())
		return err
	})
}

// Run repeats RunOnce every interval until ctx ends.
func Run(ctx context.Context, db *pgxpool.Pool, log *slog.Logger, interval time.Duration) {
	RunOnce(ctx, db, log)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			RunOnce(ctx, db, log)
		}
	}
}
