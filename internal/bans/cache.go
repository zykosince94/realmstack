package bans

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Cache is this API instance's copy of every ban in force, so the player-token check on
// every request is a map lookup instead of a database query.
//
// Why not query per request, or cache per player? Player tokens are the hottest path
// (a large game has about 100k concurrent players). A per-request query adds a round trip
// to every call, and a per-player TTL cache still costs a query per player per TTL. Bans
// in force are few, so holding all of them is cheap.
//
// How it stays current: every SyncInterval the first request that notices the copy is
// stale reads the rows changed since the previous sync (player_bans.updated_at, an
// index scan). Every FullInterval it reloads the whole set instead, to repair anything
// a delta could miss. A ban or lift on this instance updates the copy at once (Refresh);
// other instances apply it within SyncInterval, the documented bound on how long a newly
// banned player's access token keeps working elsewhere.
type Cache struct {
	db  *pgxpool.Pool
	log *slog.Logger
	// now is the API clock (Env.Now): ban expiry is compared with it, like every other
	// stored timestamp. wall paces the syncs and is never replaced by tests.
	now, wall    func() time.Time
	syncInterval time.Duration
	fullInterval time.Duration

	mu   sync.RWMutex
	bans map[string]Ban // by player ID

	// loaded and lastSync are read by every request without waiting for a running sync.
	loaded   atomic.Bool
	lastSync atomic.Int64 // wall clock, Unix nanoseconds

	syncMu   sync.Mutex // one sync at a time; guards since and lastFull
	since    time.Time  // database clock: rows changed after this are read next time
	lastFull time.Time  // wall clock
}

const (
	// DefaultSyncInterval bounds how long another instance takes to apply a ban.
	DefaultSyncInterval = 5 * time.Second
	fullInterval        = 10 * time.Minute
	// overlap re-reads recent changes on every delta sync: a row's updated_at is its
	// transaction's start time, so a transaction that commits after a sync may carry an
	// earlier timestamp than that sync's watermark. Re-reading is idempotent.
	overlap = time.Minute
)

// NewCache returns an empty cache; it loads itself on first use. A nil db (unit tests
// that never send player requests) makes every check answer "not banned".
func NewCache(db *pgxpool.Pool, log *slog.Logger, now func() time.Time, syncInterval time.Duration) *Cache {
	if syncInterval <= 0 {
		syncInterval = DefaultSyncInterval
	}
	return &Cache{db: db, log: log, now: now, wall: time.Now, syncInterval: syncInterval, fullInterval: fullInterval, bans: map[string]Ban{}}
}

// Check returns the player's ban in force, or nil. It fails only if the very first load
// fails; after that a failed sync keeps the last good copy and is retried next interval.
func (c *Cache) Check(ctx context.Context, playerID string) (*Ban, error) {
	if c == nil || c.db == nil {
		return nil, nil
	}
	if err := c.maybeSync(ctx); err != nil {
		return nil, err
	}
	c.mu.RLock()
	b, ok := c.bans[playerID]
	c.mu.RUnlock()
	if !ok || !b.inForce(c.now()) {
		return nil, nil
	}
	return &b, nil
}

// Refresh re-reads one player's ban from the database (after a ban or lift here).
func (c *Cache) Refresh(ctx context.Context, playerID string) error {
	if c == nil || c.db == nil {
		return nil
	}
	b, err := Active(ctx, c.db, playerID, c.now())
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if b == nil {
		delete(c.bans, playerID)
	} else {
		c.bans[playerID] = *b
	}
	return nil
}

func (c *Cache) fresh() bool {
	return c.loaded.Load() && c.wall().Sub(time.Unix(0, c.lastSync.Load())) < c.syncInterval
}

func (c *Cache) maybeSync(ctx context.Context) error {
	if c.fresh() {
		return nil
	}
	if c.loaded.Load() {
		// Someone else may be syncing; requests keep using the current copy meanwhile.
		if !c.syncMu.TryLock() {
			return nil
		}
	} else {
		c.syncMu.Lock() // nothing to fall back on yet: wait for the first load
	}
	defer c.syncMu.Unlock()
	if c.fresh() {
		return nil // another request synced while this one waited
	}
	var err error
	if !c.loaded.Load() || c.wall().Sub(c.lastFull) >= c.fullInterval {
		err = c.full(ctx)
	} else {
		err = c.delta(ctx)
	}
	c.lastSync.Store(c.wall().UnixNano()) // also after a failure, so an outage is not hammered
	if err != nil && c.loaded.Load() {
		c.log.Error("ban cache sync failed; using the previous copy", "error", err)
		return nil
	}
	return err
}

// full replaces the copy with every open ban that has not expired. Called with syncMu held.
func (c *Cache) full(ctx context.Context) error {
	var dbNow time.Time
	if err := c.db.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		return err
	}
	rows, err := c.db.Query(ctx, `SELECT player_id::text,id::text,reason,expires_at,created_at FROM player_bans
		WHERE ended_at IS NULL AND (expires_at IS NULL OR expires_at > $1)`, c.now())
	if err != nil {
		return err
	}
	defer rows.Close()
	next := map[string]Ban{}
	for rows.Next() {
		var player string
		var b Ban
		if err = rows.Scan(&player, &b.ID, &b.Reason, &b.ExpiresAt, &b.CreatedAt); err != nil {
			return err
		}
		next[player] = b
	}
	if err = rows.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.bans = next
	c.mu.Unlock()
	c.since, c.lastFull = dbNow.Add(-overlap), c.wall()
	c.loaded.Store(true)
	return nil
}

// delta applies the rows changed since the last sync. Called with syncMu held.
func (c *Cache) delta(ctx context.Context) error {
	var dbNow time.Time
	if err := c.db.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		return err
	}
	rows, err := c.db.Query(ctx, `SELECT player_id::text,id::text,reason,expires_at,created_at,ended_at IS NOT NULL
		FROM player_bans WHERE updated_at > $1 ORDER BY updated_at`, c.since)
	if err != nil {
		return err
	}
	defer rows.Close()
	type change struct {
		player string
		ban    Ban
		ended  bool
	}
	var changes []change
	for rows.Next() {
		var ch change
		if err = rows.Scan(&ch.player, &ch.ban.ID, &ch.ban.Reason, &ch.ban.ExpiresAt, &ch.ban.CreatedAt, &ch.ended); err != nil {
			return err
		}
		changes = append(changes, ch)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	for _, ch := range changes {
		current, has := c.bans[ch.player]
		switch {
		case !ch.ended:
			c.bans[ch.player] = ch.ban
		// Remove only the ban this row describes: replacing a ban ends the old row and
		// opens a new one in the same transaction, so both share one updated_at and may
		// arrive in either order.
		case has && current.ID == ch.ban.ID:
			delete(c.bans, ch.player)
		}
	}
	c.mu.Unlock()
	c.since = dbNow.Add(-overlap)
	return nil
}
