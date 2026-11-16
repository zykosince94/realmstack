// Package limits enforces rate limits and per-tenant quotas through PostgreSQL, so
// they hold across every API instance (spec 6.6). Phase 1–7 counted in process
// memory, so N instances quietly allowed N times the limit.
//
// Two strategies, chosen by traffic volume:
//
//   - Exact: one atomic upsert per attempt. Precise, and fine for low-volume buckets
//     such as login and registration attempts.
//   - Quotas: per-tenant request and write budgets at thousands of requests per second.
//     A database write per request would double the write load and serialize every
//     instance on one hot row, so each instance counts locally and folds its count into
//     the shared row on every Sync (by default every 250 ms). Decisions use the last
//     known shared total plus local unsynced usage, so the overshoot across instances
//     is bounded by what the instances admit within one sync interval.
package limits

import (
	"context"
	"errors"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/platform"
)

// Exact counts one attempt in bucket's current fixed window and reports whether it is
// within limit, plus the time until the window resets.
func Exact(ctx context.Context, q platform.Querier, bucket string, limit int, window time.Duration, now time.Time) (bool, time.Duration, error) {
	start := now.Truncate(window)
	var count int64
	err := q.QueryRow(ctx, `INSERT INTO rate_limit_windows(bucket,window_start,count) VALUES($1,$2,1)
		ON CONFLICT (bucket,window_start) DO UPDATE SET count=rate_limit_windows.count+1 RETURNING count`, bucket, start).Scan(&count)
	if err != nil {
		return true, 0, err
	}
	return count <= int64(limit), start.Add(window).Sub(now), nil
}

// RetryAfterHeader renders a wait as whole seconds for the Retry-After header, rounded
// up so a client that waits exactly that long lands in the next window.
func RetryAfterHeader(wait time.Duration) string {
	return strconv.Itoa(max(1, int(math.Ceil(wait.Seconds()))))
}

// Purge deletes windows older than keep (the maintenance job calls it).
func Purge(ctx context.Context, q platform.Querier, keep time.Duration, now time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM rate_limit_windows WHERE window_start < $1`, now.Add(-keep))
	return tag.RowsAffected(), err
}

// Limits is one tenant's quota (project_quotas, or the defaults).
type Limits struct {
	RequestsPerMinute int64
	WritesPerMinute   int64
	MaxBatchEntries   int
}

// Quota kinds.
const (
	Requests = "requests"
	Writes   = "writes"
)

type counter struct {
	window  time.Time
	pending int64 // admitted here, not yet folded into the shared row
	shared  int64 // the shared total as of the last sync (all instances)
}

type cachedLimits struct {
	limits  Limits
	expires time.Time
}

// Quotas enforces per-tenant budgets over one-minute windows.
type Quotas struct {
	db       interface{ platform.Querier }
	defaults Limits
	now      func() time.Time

	mu       sync.Mutex
	counters map[string]*counter
	cache    map[string]cachedLimits
}

// NewQuotas returns a quota enforcer; defaults apply to projects without a row.
func NewQuotas(db platform.Querier, defaults Limits, now func() time.Time) *Quotas {
	return &Quotas{db: db, defaults: defaults, now: now, counters: map[string]*counter{}, cache: map[string]cachedLimits{}}
}

// Limits returns a project's quota. Rows are cached for a minute: quota changes are
// rare, and this is on the hot path of every request.
func (q *Quotas) Limits(ctx context.Context, projectID string) Limits {
	now := q.now()
	q.mu.Lock()
	c, ok := q.cache[projectID]
	q.mu.Unlock()
	if ok && now.Before(c.expires) {
		return c.limits
	}
	l := q.defaults
	err := q.db.QueryRow(ctx, `SELECT requests_per_minute,writes_per_minute,max_batch_entries FROM project_quotas WHERE project_id=$1`, projectID).
		Scan(&l.RequestsPerMinute, &l.WritesPerMinute, &l.MaxBatchEntries)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return q.defaults // fail open to the defaults; a database error is reported elsewhere
	}
	q.mu.Lock()
	q.cache[projectID] = cachedLimits{limits: l, expires: now.Add(time.Minute)}
	q.mu.Unlock()
	return l
}

// Take admits n units of kind for a project, or reports how long until the window
// resets. It never touches the database except for the cached Limits lookup.
func (q *Quotas) Take(ctx context.Context, projectID, kind string, n int64) (bool, time.Duration) {
	l := q.Limits(ctx, projectID)
	limit := l.RequestsPerMinute
	if kind == Writes {
		limit = l.WritesPerMinute
	}
	now := q.now()
	window := now.Truncate(time.Minute)
	key := kind + ":" + projectID
	q.mu.Lock()
	c := q.counters[key]
	fresh := c == nil || !c.window.Equal(window)
	q.mu.Unlock()
	if fresh {
		// First use of this project in this window on this instance: start from what
		// the other instances have already used (one query per project per minute),
		// instead of from zero, which would let every instance admit a whole quota.
		var shared int64
		_ = q.db.QueryRow(ctx, `SELECT count FROM rate_limit_windows WHERE bucket=$1 AND window_start=$2`, "quota:"+key, window).Scan(&shared)
		q.mu.Lock()
		if c = q.counters[key]; c == nil || !c.window.Equal(window) {
			c = &counter{window: window, shared: shared}
			q.counters[key] = c
		}
		q.mu.Unlock()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if c.shared+c.pending+n > limit {
		return false, window.Add(time.Minute).Sub(now)
	}
	c.pending += n
	return true, 0
}

// Sync folds local usage into the shared counters and learns the other instances'
// usage from the result.
func (q *Quotas) Sync(ctx context.Context) error {
	type flush struct {
		key     string
		window  time.Time
		pending int64
	}
	q.mu.Lock()
	var work []flush
	current := q.now().Truncate(time.Minute)
	for key, c := range q.counters {
		// Counters with nothing to add are refreshed too (adding 0), so an instance
		// keeps learning the other instances' usage while it is idle.
		if c.pending > 0 || c.window.Equal(current) {
			work = append(work, flush{key, c.window, c.pending})
		}
	}
	q.mu.Unlock()
	for _, f := range work {
		var total int64
		err := q.db.QueryRow(ctx, `INSERT INTO rate_limit_windows(bucket,window_start,count) VALUES($1,$2,$3)
			ON CONFLICT (bucket,window_start) DO UPDATE SET count=rate_limit_windows.count+EXCLUDED.count RETURNING count`,
			"quota:"+f.key, f.window, f.pending).Scan(&total)
		if err != nil {
			return err
		}
		q.mu.Lock()
		if c := q.counters[f.key]; c != nil && c.window.Equal(f.window) {
			c.pending -= f.pending
			c.shared = total
		}
		q.mu.Unlock()
	}
	// Drop counters from past windows.
	q.mu.Lock()
	for key, c := range q.counters {
		if c.window.Before(current) && c.pending == 0 {
			delete(q.counters, key)
		}
	}
	q.mu.Unlock()
	return nil
}

// Run syncs every interval until ctx ends, then once more so nothing admitted is lost.
func (q *Quotas) Run(ctx context.Context, interval time.Duration, onError func(error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = q.Sync(flushCtx)
			cancel()
			return
		case <-t.C:
			if err := q.Sync(ctx); err != nil && onError != nil && ctx.Err() == nil {
				onError(err)
			}
		}
	}
}
