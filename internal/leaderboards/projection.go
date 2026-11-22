package leaderboards

// Optional Redis rank projection.
//
// Why it exists: a player's rank in PostgreSQL is a count of the index entries ahead of
// them. Counting costs about 65 ns per entry, so a mid-table lookup on a board with N
// players counts N/2 entries (about 30 ms of CPU at a million players), and hundreds of
// lookups per second keep many cores busy counting. A Redis sorted set answers ZRANK
// in O(log n).
//
// How it stays correct:
//   - PostgreSQL remains the source of truth. A submission that improves a best inserts
//     a leaderboard_outbox row in the same transaction, so no improvement is lost.
//   - Workers on every instance drain the outbox with FOR UPDATE SKIP LOCKED, read the
//     player's current best from PostgreSQL and write that to Redis. A best only ever
//     improves, so the Redis write keeps whichever entry is better: a retry, or two
//     workers racing on one player, cannot leave an older best behind.
//   - A board is served from Redis only after a full replay set its ready key. Until
//     then (new board, Redis restarted or flushed) ranks come from the PostgreSQL count,
//     and Heal re-enqueues boards that are not ready.
//   - A rank read also checks that Redis holds this player's current best (from the
//     primary-key lookup). If the projection is behind for this player it falls back to
//     the count, so a player never sees a rank for a score they no longer have. Other
//     players' changes may be a poll interval (~100 ms) behind.
//
// Operating rules (Redis is trusted once a board is ready, so these are not optional):
//   - Set REDIS_URL on every API instance or on none. An instance without it accepts
//     improving scores without writing outbox rows. After running without Redis, or with
//     it on only some instances, run `realmstack-admin rebuild-projection`.
//   - Redis must not evict keys (maxmemory-policy noeviction, the default without a
//     maxmemory limit). A ready board that lost members to eviction would report ranks
//     that are too good for everyone behind the missing entries. `verify` detects it.
//   - Scores are bounded to +-MaxScore at submission (and by a CHECK constraint), so
//     every rank score is exactly representable as the float64 a sorted set stores.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// projectionKeys returns a board's Redis keys. The {board} hash tag keeps them in one
// Redis Cluster slot, which the Lua scripts need.
func projectionKeys(board string) (sorted, members, ready string) {
	tag := "realmstack:lb:{" + board + "}"
	return tag + ":z", tag + ":m", tag + ":ready"
}

// projectedMember orders entries with equal scores. Redis breaks score ties by member
// bytes; a fixed-width achieved_at then the player UUID text reproduces PostgreSQL's
// (rank_score, achieved_at, player_id) order exactly, because UUIDs sort by their bytes
// in both. The members hash maps player to member, since the member changes with
// achieved_at.
func projectedMember(at time.Time, player string) string {
	return fmt.Sprintf("%019d:%s", at.UnixMicro(), player)
}

// upsertScript replaces a player's entry only with a better one (lower score, or equal
// score and earlier time). KEYS: sorted, members. ARGV: player, score, member, micros.
const upsertScript = `
local old = redis.call('HGET', KEYS[2], ARGV[1])
if old then
  local s = redis.call('ZSCORE', KEYS[1], old)
  if s then
    s = tonumber(s)
    local n = tonumber(ARGV[2])
    if s < n or (s == n and tonumber(string.sub(old, 1, 19)) <= tonumber(ARGV[4])) then
      return 0
    end
  end
  redis.call('ZREM', KEYS[1], old)
end
redis.call('ZADD', KEYS[1], ARGV[2], ARGV[3])
redis.call('HSET', KEYS[2], ARGV[1], ARGV[3])
return 1`

// removeScript drops a player whose best no longer exists. KEYS: sorted, members.
const removeScript = `
local old = redis.call('HGET', KEYS[2], ARGV[1])
if old then
  redis.call('ZREM', KEYS[1], old)
  redis.call('HDEL', KEYS[2], ARGV[1])
end
return 1`

// rankScript returns nil when the board is not ready, {-1} when the player is not
// projected, else {zero-based rank, member, score}. KEYS: sorted, members, ready. It
// runs on every rank read, so it is a redis.Script: sent by hash (EVALSHA), with the
// source only on the first call after Redis loses its script cache.
var rankScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[3]) == 0 then return false end
local m = redis.call('HGET', KEYS[2], ARGV[1])
if not m then return {-1} end
return {redis.call('ZRANK', KEYS[1], m), m, redis.call('ZSCORE', KEYS[1], m)}`)

// projectedRank answers from Redis when it can do so exactly; ok=false means use the
// PostgreSQL count. Any Redis error also falls back: the projection is an
// optimization, never a dependency of correctness.
func projectedRank(ctx context.Context, rdb redis.UniversalClient, board, player string, rankScore int64, at time.Time) (int64, bool) {
	ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	sorted, members, ready := projectionKeys(board)
	res, err := rankScript.Run(ctx, rdb, []string{sorted, members, ready}, player).Slice()
	if err != nil || len(res) != 3 {
		return 0, false
	}
	rank, okRank := res[0].(int64)
	member, okMember := res[1].(string)
	score, okScore := res[2].(string)
	if !okRank || !okMember || !okScore || member != projectedMember(at, player) {
		return 0, false
	}
	if f, err := strconv.ParseFloat(score, 64); err != nil || f != float64(rankScore) {
		return 0, false
	}
	return rank + 1, true
}

// Projection drains the outbox into Redis and repairs boards that are not ready.
type Projection struct {
	DB    *pgxpool.Pool
	Redis redis.UniversalClient
	Log   *slog.Logger
	// Batch is the number of outbox rows per transaction (default 1000).
	Batch int
}

func (p *Projection) batch() int {
	if p.Batch > 0 {
		return p.Batch
	}
	return 1000
}

// Drain applies one batch of outbox rows and returns how many rows it processed.
func (p *Projection) Drain(ctx context.Context) (int, error) {
	// Idle instances poll every few milliseconds; one plain query is much cheaper than
	// a transaction (three round trips through the connection pooler).
	var queued bool
	if err := p.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM leaderboard_outbox)`).Scan(&queued); err != nil || !queued {
		return 0, err
	}
	tx, err := p.DB.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	// SKIP LOCKED lets every instance drain at once without waiting on each other.
	// The player's best is read in the same statement, from the primary.
	rows, err := tx.Query(ctx, `SELECT o.id,o.leaderboard_id::text,o.player_id::text,b.rank_score,b.achieved_at
		FROM (SELECT id,leaderboard_id,player_id FROM leaderboard_outbox ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED) o
		LEFT JOIN leaderboard_best_scores b ON b.leaderboard_id=o.leaderboard_id AND b.player_id=o.player_id`, p.batch())
	if err != nil {
		return 0, err
	}
	type entry struct {
		id        int64
		board     string
		player    *string
		rankScore *int64
		at        *time.Time
	}
	var entries []entry
	for rows.Next() {
		var e entry
		if err = rows.Scan(&e.id, &e.board, &e.player, &e.rankScore, &e.at); err != nil {
			rows.Close()
			return 0, err
		}
		entries = append(entries, e)
	}
	rows.Close()
	if err = rows.Err(); err != nil || len(entries) == 0 {
		return 0, err
	}

	pipe := p.Redis.Pipeline()
	var applied []int64
	var markers []entry
	for _, e := range entries {
		if e.player == nil {
			markers = append(markers, e)
			continue
		}
		applied = append(applied, e.id)
		sorted, members, _ := projectionKeys(e.board)
		if e.rankScore == nil { // the best no longer exists (a rebuild removed it)
			pipe.Eval(ctx, removeScript, []string{sorted, members}, *e.player)
		} else {
			pipe.Eval(ctx, upsertScript, []string{sorted, members}, *e.player,
				strconv.FormatInt(*e.rankScore, 10), projectedMember(*e.at, *e.player), e.at.UnixMicro())
		}
	}
	if len(applied) > 0 {
		// A Redis failure rolls the transaction back, so the rows are retried.
		if _, err = pipe.Exec(ctx); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM leaderboard_outbox WHERE id = ANY($1)`, applied); err != nil {
			return 0, err
		}
	}
	// A replay marker completes only when no earlier row for its board remains. Rows
	// another worker is applying stay visible until it commits, so they block it too.
	for _, m := range markers {
		var pending bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM leaderboard_outbox WHERE leaderboard_id=$1 AND id<$2)`, m.board, m.id).Scan(&pending); err != nil {
			return 0, err
		}
		if pending {
			continue
		}
		_, _, ready := projectionKeys(m.board)
		if err = p.Redis.Set(ctx, ready, "1", 0).Err(); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM leaderboard_outbox WHERE id=$1`, m.id); err != nil {
			return 0, err
		}
	}
	return len(entries), tx.Commit(ctx)
}

// Rebuild replays one board: it clears the board's Redis keys and enqueues every best
// plus a completion marker. Ranks use PostgreSQL until the replay finishes.
func (p *Projection) Rebuild(ctx context.Context, board string) error {
	tx, err := p.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO leaderboard_outbox(leaderboard_id,player_id) SELECT leaderboard_id,player_id FROM leaderboard_best_scores WHERE leaderboard_id=$1`, board); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO leaderboard_outbox(leaderboard_id) VALUES($1)`, board); err != nil {
		return err
	}
	// UNLINK frees a large sorted set in the background instead of blocking Redis.
	sorted, members, ready := projectionKeys(board)
	if err = p.Redis.Unlink(ctx, ready, sorted, members).Err(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RebuildAll replays every board (after rebuild-leaderboards, or on demand).
func (p *Projection) RebuildAll(ctx context.Context) (int, error) {
	ids, err := p.boards(ctx, `SELECT id::text FROM leaderboard_definitions`)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err = p.Rebuild(ctx, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// Heal replays active boards that are not ready and have no replay pending: new
// boards, and every board after Redis lost its data.
func (p *Projection) Heal(ctx context.Context) (int, error) {
	ids, err := p.boards(ctx, `SELECT d.id::text FROM leaderboard_definitions d JOIN projects pr ON pr.id=d.project_id
		WHERE d.active AND pr.status='active' AND NOT EXISTS (SELECT 1 FROM leaderboard_outbox o WHERE o.leaderboard_id=d.id AND o.player_id IS NULL)`)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	pipe := p.Redis.Pipeline()
	checks := make([]*redis.IntCmd, len(ids))
	for i, id := range ids {
		_, _, ready := projectionKeys(id)
		checks[i] = pipe.Exists(ctx, ready)
	}
	if _, err = pipe.Exec(ctx); err != nil {
		return 0, err
	}
	healed := 0
	for i, id := range ids {
		if checks[i].Val() > 0 {
			continue
		}
		if err = p.Rebuild(ctx, id); err != nil {
			return healed, err
		}
		healed++
	}
	return healed, nil
}

func (p *Projection) boards(ctx context.Context, sql string) ([]string, error) {
	rows, err := p.DB.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// VerifyResult reports how a projection compares with leaderboard_best_scores.
type VerifyResult struct {
	Boards, NotReady int
	Mismatches       int64
}

// Verify compares every ready board's sorted set, position by position, with the
// PostgreSQL order. Boards that are not ready are served by PostgreSQL and only counted.
func (p *Projection) Verify(ctx context.Context) (VerifyResult, error) {
	var res VerifyResult
	ids, err := p.boards(ctx, `SELECT id::text FROM leaderboard_definitions`)
	if err != nil {
		return res, err
	}
	for _, id := range ids {
		res.Boards++
		sorted, _, ready := projectionKeys(id)
		if n, err := p.Redis.Exists(ctx, ready).Result(); err != nil {
			return res, err
		} else if n == 0 {
			res.NotReady++
			continue
		}
		n, err := p.verifyBoard(ctx, id, sorted)
		if err != nil {
			return res, err
		}
		res.Mismatches += n
	}
	return res, nil
}

func (p *Projection) verifyBoard(ctx context.Context, board, sorted string) (int64, error) {
	rows, err := p.DB.Query(ctx, `SELECT player_id::text,rank_score,achieved_at FROM leaderboard_best_scores WHERE leaderboard_id=$1 ORDER BY rank_score,achieved_at,player_id`, board)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	const page = 10000
	var mismatches, pos int64
	var chunk []redis.Z
	for rows.Next() {
		var player string
		var rankScore int64
		var at time.Time
		if err = rows.Scan(&player, &rankScore, &at); err != nil {
			return 0, err
		}
		if pos%page == 0 {
			if chunk, err = p.Redis.ZRangeWithScores(ctx, sorted, pos, pos+page-1).Result(); err != nil {
				return 0, err
			}
		}
		i := int(pos % page)
		if i >= len(chunk) || chunk[i].Member != projectedMember(at, player) || chunk[i].Score != float64(rankScore) {
			mismatches++
		}
		pos++
	}
	if err = rows.Err(); err != nil {
		return 0, err
	}
	// Extra Redis entries beyond the PostgreSQL rows are mismatches too.
	card, err := p.Redis.ZCard(ctx, sorted).Result()
	if err != nil {
		return 0, err
	}
	if card > pos {
		mismatches += card - pos
	}
	return mismatches, nil
}

// Run drains continuously and heals every healEvery (one instance at a time, through
// a transaction-scoped advisory lock that works behind PgBouncer).
func (p *Projection) Run(ctx context.Context, idle, healEvery time.Duration) {
	nextHeal := time.Now()
	for ctx.Err() == nil {
		if !time.Now().Before(nextHeal) {
			nextHeal = time.Now().Add(healEvery)
			if err := p.healExclusive(ctx); err != nil && ctx.Err() == nil {
				p.Log.Error("rank projection heal failed", "error", err)
			}
		}
		n, err := p.Drain(ctx)
		wait := time.Duration(0)
		switch {
		case err != nil && ctx.Err() == nil:
			p.Log.Error("rank projection drain failed", "error", err)
			wait = time.Second
		case n < p.batch():
			wait = idle
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
		}
	}
}

func (p *Projection) healExclusive(ctx context.Context) error {
	tx, err := p.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var won bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtext('realmstack-rank-projection-heal'))`).Scan(&won); err != nil || !won {
		return err
	}
	n, err := p.Heal(ctx)
	if n > 0 {
		p.Log.Info("rank projection replay started", "boards", n)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// errNoRedis is returned by admin helpers when REDIS_URL is not set.
var errNoRedis = errors.New("REDIS_URL is not set")

// NewProjection returns a Projection, or an error without a Redis client.
func NewProjection(db *pgxpool.Pool, rdb redis.UniversalClient, log *slog.Logger) (*Projection, error) {
	if rdb == nil {
		return nil, errNoRedis
	}
	return &Projection{DB: db, Redis: rdb, Log: log}, nil
}
