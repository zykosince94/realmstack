package httpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"
	"realmstack-backend/internal/leaderboards"
	"realmstack-backend/internal/platform"
)

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set; skipping Redis integration test")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opts)
	t.Cleanup(func() { rdb.Close() })
	if err = rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis: %v", err)
	}
	return rdb
}

// The Redis rank projection gives the same ranks as the PostgreSQL count, never serves
// a board before its replay finished or a player whose best it has not caught up with,
// and is repaired by Heal and checked by Verify.
func TestIntegrationRankProjection(t *testing.T) {
	ctx := context.Background()
	rdb := testRedis(t)
	f := newFixture(t)
	srv := newServer(t, f.db)
	srv.Env.Redis = rdb
	f.h = srv.Handler()
	p := &leaderboards.Projection{DB: f.db, Redis: rdb, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Batch: 3}
	drain := func() {
		t.Helper()
		for i := 0; i < 100; i++ {
			n, err := p.Drain(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				return
			}
		}
		t.Fatal("outbox did not drain")
	}
	var boardID string
	if err := f.db.QueryRow(ctx, `SELECT id::text FROM leaderboard_definitions WHERE environment_id=$1 AND key='global'`, f.envID).Scan(&boardID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ids, _ := f.db.Query(ctx, `SELECT id::text FROM leaderboard_definitions WHERE environment_id=$1`, f.envID)
		for ids.Next() {
			var id string
			_ = ids.Scan(&id)
			tag := "realmstack:lb:{" + id + "}"
			rdb.Del(ctx, tag+":z", tag+":m", tag+":ready")
		}
		ids.Close()
	})

	type player struct{ id, token string }
	players := []player{{f.playerID, f.player}}
	for i := 1; i < 6; i++ {
		id, token, _ := f.registerPlayer(fmt.Sprintf("rank%d@example.test", i), fmt.Sprintf("Ranker %d", i))
		players = append(players, player{id, token})
	}
	submit := func(pl player, score int) {
		t.Helper()
		expect(t, f.playerPost(pl.token, "/v1/leaderboards/global/scores", "s-"+platform.RandomID(), map[string]any{"score": score}), 201)
	}
	rankOf := func(pl player) float64 {
		t.Helper()
		return expect(t, call(t, f.h, "GET", "/v1/leaderboards/global?limit=1", nil, bearer(pl.token)), 200).obj(t)["player_rank"].(float64)
	}
	// countRank is the PostgreSQL answer the projection must reproduce: one plus the
	// number of players whose best is better (smaller rank_score, then earlier, then id).
	countRank := func(pl player) float64 {
		t.Helper()
		return float64(f.count(`SELECT (1+count(*))::int FROM leaderboard_best_scores o, leaderboard_best_scores b
			WHERE b.leaderboard_id=$1 AND b.player_id=$2 AND o.leaderboard_id=b.leaderboard_id
			AND (o.rank_score,o.achieved_at,o.player_id) < (b.rank_score,b.achieved_at,b.player_id)`, boardID, pl.id))
	}
	checkAll := func() {
		t.Helper()
		for i, pl := range players {
			if got, want := rankOf(pl), countRank(pl); got != want {
				t.Fatalf("player %d: rank %v, count says %v", i, got, want)
			}
		}
	}
	verify := func(wantMismatches int64) {
		t.Helper()
		res, err := p.Verify(ctx)
		if err != nil || res.Mismatches != wantMismatches {
			t.Fatalf("verify = %+v (err %v), want %d mismatches", res, err, wantMismatches)
		}
	}

	// Ties on 500 (earlier first), improvements, and a worse score that must not
	// enqueue anything.
	submit(players[0], 200)
	submit(players[0], 500)
	submit(players[1], 500)
	submit(players[1], 100) // worse: no outbox row
	submit(players[2], 450)
	submit(players[3], 900)
	submit(players[4], 500)
	submit(players[5], 10)
	if n := f.count(`SELECT count(*) FROM leaderboard_outbox WHERE player_id IS NOT NULL`); n != 7 {
		t.Fatalf("outbox rows = %d, want 7 (improvements only)", n)
	}
	// Before any replay the board is not ready: ranks come from PostgreSQL.
	checkAll()
	verify(0)

	// Heal replays both active boards; applying the outbox makes them ready.
	healed, err := p.Heal(ctx)
	if err != nil || healed != 2 {
		t.Fatalf("heal = %d, %v; want 2 boards", healed, err)
	}
	if again, _ := p.Heal(ctx); again != 0 {
		t.Fatalf("a pending replay was enqueued again (%d)", again)
	}
	drain()
	_, _, ready := projectionKeysForTest(boardID)
	if n, _ := rdb.Exists(ctx, ready).Result(); n != 1 {
		t.Fatal("board is not ready after its replay")
	}
	if n := f.count(`SELECT count(*) FROM leaderboard_outbox`); n != 0 {
		t.Fatalf("outbox not empty: %d", n)
	}
	checkAll()
	verify(0)

	// Ranks really come from Redis now: an extra member ahead of everyone shifts them.
	sorted, _, _ := projectionKeysForTest(boardID)
	rdb.ZAdd(ctx, sorted, redis.Z{Score: -1e6, Member: "0000000000000000000:ghost"})
	if got := rankOf(players[3]); got != 2 {
		t.Fatalf("rank with a ghost entry = %v; ranks are not served from Redis", got)
	}
	verify(int64(len(players)) + 1) // every position shifted, plus one extra entry
	rdb.ZRem(ctx, sorted, "0000000000000000000:ghost")
	verify(0)

	// An improvement Redis has not applied yet: the player's own rank falls back to
	// the count instead of reporting their old position.
	submit(players[5], 1000)
	if got := rankOf(players[5]); got != 1 {
		t.Fatalf("rank after an unprojected improvement = %v, want 1", got)
	}
	drain()
	checkAll()
	verify(0)

	// Redis lost its data: nothing is ready, ranks use PostgreSQL, Heal replays.
	for _, k := range []string{"ready", "z", "m"} {
		rdb.Del(ctx, "realmstack:lb:{"+boardID+"}:"+k)
	}
	checkAll()
	if healed, err = p.Heal(ctx); err != nil || healed != 1 {
		t.Fatalf("heal after data loss = %d, %v; want 1 board", healed, err)
	}
	drain()
	checkAll()
	verify(0)

	// rebuild-leaderboards can make bests worse, which improve-only writes cannot
	// follow; RebuildAll replays from scratch and Verify agrees afterwards.
	if _, err = f.db.Exec(ctx, `DELETE FROM leaderboard_best_scores WHERE leaderboard_id=$1 AND player_id=$2`, boardID, players[3].id); err != nil {
		t.Fatal(err)
	}
	// Redis still has the deleted player in second place: four positions shift, plus
	// one extra entry.
	verify(5)
	if _, err = p.RebuildAll(ctx); err != nil {
		t.Fatal(err)
	}
	drain()
	verify(0)
}

// projectionKeysForTest mirrors leaderboards.projectionKeys (unexported).
func projectionKeysForTest(board string) (sorted, members, ready string) {
	tag := "realmstack:lb:{" + board + "}"
	return tag + ":z", tag + ":m", tag + ":ready"
}
