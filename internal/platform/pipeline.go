package platform

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Write-behind pipelining for mutations.
//
// Why: the load test found PgBouncer, which is single-threaded, pinned at one core
// while PostgreSQL and the API had CPU to spare. Its cost is per network round trip,
// and a credit was 12 of them. Many statements in a mutation are writes whose result
// nobody reads: ledger rows and goal-total upserts. Those are queued with Defer and
// sent in the same round trip as the transaction's next statement (usually the
// idempotency record insert), so three statements cost one round trip.
//
// Semantics stay exact: any later statement on the transaction flushes the queue
// first, in order, so a read inside the transaction always sees the deferred rows. A
// failing deferred statement surfaces as the error of the statement it was sent with,
// and the mutation rolls back as before. Only defer statements whose errors you do not
// need to tell apart from the next statement's.

// Deferrer is implemented by transactions that can queue a write.
type Deferrer interface {
	Defer(sql string, args ...any)
}

// DeferExec queues sql on tx when tx supports it, else runs it now.
func DeferExec(ctx context.Context, tx pgx.Tx, sql string, args ...any) error {
	if d, ok := tx.(Deferrer); ok {
		d.Defer(sql, args...)
		return nil
	}
	_, err := tx.Exec(ctx, sql, args...)
	return err
}

type queued struct {
	sql  string
	args []any
}

// pipeTx wraps a transaction (or savepoint) and holds deferred writes until the next
// statement. Methods it does not override (Rollback, Conn, ...) pass through; Rollback
// discards the queue along with everything else.
type pipeTx struct {
	pgx.Tx
	pending []queued
}

func newPipeTx(tx pgx.Tx) *pipeTx { return &pipeTx{Tx: tx} }

// Pipelined wraps tx so that DeferExec queues on it. Batch writes use it to send each
// entry's SAVEPOINT and the previous entry's RELEASE with the entry's first statement.
func Pipelined(tx pgx.Tx) pgx.Tx { return newPipeTx(tx) }

func (t *pipeTx) Defer(sql string, args ...any) {
	t.pending = append(t.pending, queued{sql, args})
}

// sendWithPending queues the pending writes plus one statement in a single batch and
// executes the pending ones, returning the open batch for the caller's statement.
func (t *pipeTx) sendWithPending(ctx context.Context, sql string, args []any) (pgx.BatchResults, error) {
	b := &pgx.Batch{}
	for _, q := range t.pending {
		b.Queue(q.sql, q.args...)
	}
	b.Queue(sql, args...)
	n := len(t.pending)
	t.pending = nil
	br := t.Tx.SendBatch(ctx, b)
	for i := 0; i < n; i++ {
		if _, err := br.Exec(); err != nil {
			br.Close()
			return nil, err
		}
	}
	return br, nil
}

// flush sends pending writes on their own (before statements that cannot share a
// batch: Query, savepoints, COPY, commit).
func (t *pipeTx) flush(ctx context.Context) error {
	if len(t.pending) == 0 {
		return nil
	}
	last := t.pending[len(t.pending)-1]
	t.pending = t.pending[:len(t.pending)-1]
	br, err := t.sendWithPending(ctx, last.sql, last.args)
	if err != nil {
		return err
	}
	_, err = br.Exec()
	if cerr := br.Close(); err == nil {
		err = cerr
	}
	return err
}

func (t *pipeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if len(t.pending) == 0 {
		return t.Tx.Exec(ctx, sql, args...)
	}
	br, err := t.sendWithPending(ctx, sql, args)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := br.Exec()
	if cerr := br.Close(); err == nil {
		err = cerr
	}
	return tag, err
}

func (t *pipeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if len(t.pending) == 0 {
		return t.Tx.QueryRow(ctx, sql, args...)
	}
	br, err := t.sendWithPending(ctx, sql, args)
	if err != nil {
		return errRow{err}
	}
	return &batchRow{row: br.QueryRow(), br: br}
}

func (t *pipeTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := t.flush(ctx); err != nil {
		return nil, err
	}
	return t.Tx.Query(ctx, sql, args...)
}

// SendBatch puts the pending writes in front of b's statements, in the same round
// trip, and consumes their results before handing the batch back.
func (t *pipeTx) SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults {
	if len(t.pending) == 0 {
		return t.Tx.SendBatch(ctx, b)
	}
	nb := &pgx.Batch{}
	for _, q := range t.pending {
		nb.Queue(q.sql, q.args...)
	}
	nb.QueuedQueries = append(nb.QueuedQueries, b.QueuedQueries...)
	n := len(t.pending)
	t.pending = nil
	br := t.Tx.SendBatch(ctx, nb)
	for i := 0; i < n; i++ {
		if _, err := br.Exec(); err != nil {
			br.Close()
			return errBatch{err}
		}
	}
	return br
}

// Commit sends anything still queued first, so a deferred write can never be
// dropped by committing straight after it.
func (t *pipeTx) Commit(ctx context.Context) error {
	if err := t.flush(ctx); err != nil {
		return err
	}
	return t.Tx.Commit(ctx)
}

func (t *pipeTx) Begin(ctx context.Context) (pgx.Tx, error) {
	if err := t.flush(ctx); err != nil {
		return nil, err
	}
	return t.Tx.Begin(ctx)
}

func (t *pipeTx) CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, src pgx.CopyFromSource) (int64, error) {
	if err := t.flush(ctx); err != nil {
		return 0, err
	}
	return t.Tx.CopyFrom(ctx, table, columns, src)
}

// batchRow closes the batch once the row is scanned, so the connection is free again.
type batchRow struct {
	row pgx.Row
	br  pgx.BatchResults
}

func (r *batchRow) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	if cerr := r.br.Close(); err == nil {
		err = cerr
	}
	return err
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

type errBatch struct{ err error }

func (b errBatch) Exec() (pgconn.CommandTag, error) { return pgconn.CommandTag{}, b.err }
func (b errBatch) Query() (pgx.Rows, error)         { return nil, b.err }
func (b errBatch) QueryRow() pgx.Row                { return errRow{b.err} }
func (b errBatch) Close() error                     { return b.err }
