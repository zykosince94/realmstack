package platform

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Deferred writes must behave exactly like immediate ones as far as the transaction
// can tell: visible to every later statement, in order, with failures surfacing.
// Needs TEST_DATABASE_URL.
func TestPipeTxDeferredWrites(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	begin := func() (*pipeTx, pgx.Tx) {
		t.Helper()
		raw, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = raw.Exec(ctx, `CREATE TEMP TABLE pipe_log(n int PRIMARY KEY) ON COMMIT DROP`); err != nil {
			t.Fatal(err)
		}
		return newPipeTx(raw), raw
	}
	count := func(tx pgx.Tx) int {
		t.Helper()
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pipe_log`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	tx, raw := begin()
	if err = DeferExec(ctx, tx, `INSERT INTO pipe_log VALUES($1)`, 1); err != nil {
		t.Fatal(err)
	}
	if err = DeferExec(ctx, tx, `INSERT INTO pipe_log VALUES($1)`, 2); err != nil {
		t.Fatal(err)
	}
	if len(tx.pending) != 2 {
		t.Fatalf("pending = %d, want 2 queued", len(tx.pending))
	}
	// QueryRow sends the queue first, in the same round trip.
	if n := count(tx); n != 2 {
		t.Fatalf("QueryRow saw %d rows, want 2", n)
	}
	// Exec, Query and savepoints flush too.
	_ = DeferExec(ctx, tx, `INSERT INTO pipe_log VALUES($1)`, 3)
	if tag, err := tx.Exec(ctx, `INSERT INTO pipe_log VALUES(4)`); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("exec with pending: %v %v", tag, err)
	}
	_ = DeferExec(ctx, tx, `INSERT INTO pipe_log VALUES($1)`, 5)
	rows, err := tx.Query(ctx, `SELECT n FROM pipe_log ORDER BY n`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	if err != nil || len(got) != 5 || got[4] != 5 {
		t.Fatalf("query after defer = %v, %v", got, err)
	}
	_ = DeferExec(ctx, tx, `INSERT INTO pipe_log VALUES($1)`, 6)
	sp, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := count(sp); n != 6 {
		t.Fatalf("savepoint saw %d rows, want 6", n)
	}
	_ = sp.Rollback(ctx)
	// A failing deferred write surfaces on the statement it was sent with.
	_ = DeferExec(ctx, tx, `INSERT INTO pipe_log VALUES($1)`, 1) // duplicate key
	var n int
	if err = tx.QueryRow(ctx, `SELECT 1`).Scan(&n); err == nil {
		t.Fatal("duplicate deferred insert did not surface an error")
	}
	_ = raw.Rollback(ctx)

	// A raw transaction (no pipeline) just executes immediately.
	tx, raw = begin()
	if err = DeferExec(ctx, raw, `INSERT INTO pipe_log VALUES($1)`, 1); err != nil {
		t.Fatal(err)
	}
	if n := count(raw); n != 1 {
		t.Fatalf("raw DeferExec: %d rows", n)
	}
	_ = tx.Rollback(ctx)

	// SendBatch carries the queue in front of the batch's own statements, in one
	// round trip; batch writes rely on it to send SAVEPOINT with an entry's first one.
	tx, raw = begin()
	_ = DeferExec(ctx, tx, `INSERT INTO pipe_log VALUES($1)`, 1)
	_ = DeferExec(ctx, tx, `INSERT INTO pipe_log VALUES($1)`, 2)
	b := &pgx.Batch{}
	b.Queue(`SELECT count(*) FROM pipe_log`)
	b.Queue(`SELECT max(n) FROM pipe_log`)
	br := tx.SendBatch(ctx, b)
	var total, top int
	if err = br.QueryRow().Scan(&total); err != nil || total != 2 {
		t.Fatalf("batch after defer saw %d rows (%v), want 2", total, err)
	}
	if err = br.QueryRow().Scan(&top); err != nil || top != 2 {
		t.Fatalf("batch second statement = %d (%v), want 2", top, err)
	}
	if err = br.Close(); err != nil || len(tx.pending) != 0 {
		t.Fatalf("close: %v, pending %d", err, len(tx.pending))
	}
	// A failing deferred write fails the batch instead of being skipped.
	_ = DeferExec(ctx, tx, `INSERT INTO pipe_log VALUES($1)`, 1) // duplicate key
	br = tx.SendBatch(ctx, b)
	if err = br.QueryRow().Scan(&total); err == nil {
		t.Fatal("duplicate deferred insert before a batch did not surface an error")
	}
	_ = br.Close()
	_ = raw.Rollback(ctx)

	// Commit sends what is still queued, so its failure is reported, never dropped.
	tx, raw = begin()
	_ = DeferExec(ctx, tx, `INSERT INTO pipe_log VALUES($1)`, 1)
	_ = DeferExec(ctx, tx, `INSERT INTO pipe_log VALUES($1)`, 1) // duplicate key
	if err = tx.Commit(ctx); err == nil {
		t.Fatal("commit with a failing deferred insert succeeded")
	}
	_ = raw.Rollback(ctx)
}
