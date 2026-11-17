package httpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"realmstack-backend/internal/leaderboards"
	"realmstack-backend/internal/maintenance"
	"realmstack-backend/internal/platform"
	"realmstack-backend/internal/totals"
)

// newInstance is a second API process sharing the fixture's database, for checks that
// limits and caches behave across instances.
func (f *fixture) newInstance(change func(*Config)) *Server {
	cfg := Config{JWTSecret: strings.Repeat("k", 40), AccessTTL: time.Minute, RefreshTTL: time.Hour, BodyLimit: 1 << 20, AuthRateLimit: 10000}
	if change != nil {
		change(&cfg)
	}
	return New(f.db, cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

// snapshot renders a query's rows as strings, for comparing table contents.
func (f *fixture) snapshot(query string) []string {
	f.t.Helper()
	rows, err := f.db.Query(context.Background(), query)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			f.t.Fatal(err)
		}
		out = append(out, strings.TrimSpace(fmt.Sprintln(vals...)))
	}
	return out
}

func TestIntegrationBestScoresRankAndRebuild(t *testing.T) {
	f := newFixture(t)
	srv := newServer(t, f.db)
	f.h = srv.Handler()
	// Recent times: tokens are checked against the same (moved) clock.
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	submit := func(token, board string, score int, when time.Time) {
		t.Helper()
		srv.Env.Now = func() time.Time { return when }
		expect(t, call(t, f.h, "POST", "/v1/leaderboards/"+board+"/scores", map[string]any{"score": score}, withKey(bearer(token), "s-"+platform.RandomID())), 201)
	}
	tokens := map[string]string{"a": f.player}
	ids := map[string]string{"a": f.playerID}
	for _, name := range []string{"b", "c", "d"} {
		ids[name], tokens[name], _ = f.registerPlayer(name+"@rank.test", "Player "+strings.ToUpper(name))
	}
	// global is higher-is-better; speedrun is lower-is-better.
	submit(tokens["a"], "global", 50, at)
	submit(tokens["a"], "global", 90, at.Add(time.Minute)) // a's best: 90
	submit(tokens["b"], "global", 90, at.Add(2*time.Minute))
	submit(tokens["b"], "global", 90, at.Add(time.Second)) // same score earlier (clock moved back): b's best is now the earlier one
	submit(tokens["c"], "global", 120, at)
	submit(tokens["d"], "global", 10, at)
	submit(tokens["a"], "speedrun", 300, at)
	submit(tokens["b"], "speedrun", 250, at)
	submit(tokens["b"], "speedrun", 400, at.Add(time.Minute)) // worse: ignored

	srv.Env.Now = time.Now
	board := expect(t, call(t, f.h, "GET", "/v1/leaderboards/global?environment_id="+f.publicID+"&limit=10", nil, bearer(tokens["a"])), 200).obj(t)
	var order []string
	for _, e := range board["entries"].([]any) {
		order = append(order, fmt.Sprintf("%s:%v", e.(map[string]any)["display_name"], e.(map[string]any)["score"]))
	}
	// c 120; b 90 at +1s beats a 90 at +1m (earliest best wins the tie); d 10.
	if got := strings.Join(order, ","); got != "Player C:120,Player B:90,Hero owner:90,Player D:10" {
		t.Fatalf("ranking: %s", got)
	}
	if board["player_rank"].(float64) != 3 || board["player_score"].(float64) != 90 {
		t.Fatalf("player rank: %v", board)
	}
	speed := expect(t, call(t, f.h, "GET", "/v1/leaderboards/speedrun?environment_id="+f.publicID, nil, bearer(tokens["a"])), 200).obj(t)
	if speed["player_rank"].(float64) != 2 || speed["entries"].([]any)[0].(map[string]any)["score"].(float64) != 250 {
		t.Fatalf("lower-is-better: %v", speed)
	}

	// The maintained table equals a rebuild from the submissions, row for row.
	if n, err := leaderboards.VerifyBestScores(context.Background(), f.db); err != nil || n != 0 {
		t.Fatalf("verify: %d %v", n, err)
	}
	const q = `SELECT leaderboard_id,player_id,best_score,rank_score,achieved_at,submission_id FROM leaderboard_best_scores ORDER BY leaderboard_id,player_id`
	before := f.snapshot(q)
	if _, err := leaderboards.RebuildBestScores(context.Background(), f.db); err != nil {
		t.Fatal(err)
	}
	if after := f.snapshot(q); !reflect.DeepEqual(before, after) || len(after) != 6 {
		t.Fatalf("rebuild changed the table:\n%v\n%v", before, after)
	}
}

func TestIntegrationGoalTotalsMatchLedgersAndRebuild(t *testing.T) {
	f := newFixture(t)
	expect(t, f.publish(phase6Config(base.AddDate(0, 1, 0), base.AddDate(0, 2, 0))), 201)
	expect(t, f.grant(f.playerID, "health_potion", 3), 200)
	expect(t, f.playerPost(f.player, "/v1/players/me/inventory/consume", "c1", map[string]any{"item_key": "health_potion", "quantity": 1}), 200)
	expect(t, f.credit(f.playerID, "credits", 100), 200)
	expect(t, f.playerPost(f.player, "/v1/players/me/currency/spend", "sp1", map[string]any{"currency_key": "credits", "amount": 60}), 200)
	expect(t, f.server(f.serverPath(f.playerID, "/xp/award"), "xp1", map[string]any{"amount": 120}), 200)
	f.trolls(3)
	f.setFlag("talked_to_elder")
	// A claim pays gems, XP and potions tagged 'achievement': they must not move totals.
	expect(t, f.playerPost(f.player, "/v1/players/me/achievements/troll_slayer/claim", "claim1", nil), 200)

	got := map[string]int{}
	for _, row := range f.snapshot(`SELECT metric,total FROM player_goal_totals WHERE player_id='` + f.playerID + `' ORDER BY metric`) {
		var metric string
		var total int
		fmt.Sscan(row, &metric, &total)
		got[metric] = total
	}
	want := map[string]int{"grant:": 3, "grant:health_potion": 3, "consume:": 1, "consume:health_potion": 1, "credit:credits": 100, "spend:credits": 60, "xp": 120, "stat:trolls_defeated": 3}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("totals:\n got %v\nwant %v", got, want)
	}
	// Achievement progress read from the totals.
	a := f.achievements(f.player)
	if a["first_steps"]["status"] != "completed" || a["gem_collector"]["status"] != "in_progress" || a["troll_slayer"]["status"] != "claimed" {
		t.Fatalf("achievements from totals: %v", a)
	}
	if n, err := totals.Verify(context.Background(), f.db); err != nil || n != 0 {
		t.Fatalf("verify: %d %v", n, err)
	}
	const q = `SELECT player_id,metric,total FROM player_goal_totals ORDER BY player_id,metric`
	before := f.snapshot(q)
	if _, err := totals.Rebuild(context.Background(), f.db); err != nil {
		t.Fatal(err)
	}
	if after := f.snapshot(q); !reflect.DeepEqual(before, after) {
		t.Fatalf("rebuild changed totals:\n%v\n%v", before, after)
	}
}

func TestIntegrationBatchWrites(t *testing.T) {
	f := newFixture(t)
	expect(t, f.publish(phase5Config()), 201)
	other, _, _ := f.registerPlayer("other@batch.test", "Other")
	entries := []map[string]any{
		{"type": "grant", "idempotency_key": "b1", "player_id": f.playerID, "item_key": "health_potion", "quantity": 2},
		{"type": "grant", "idempotency_key": "b2", "player_id": f.playerID, "item_key": "health_potion", "quantity": 1}, // same player, after b1
		{"type": "grant", "idempotency_key": "b3", "player_id": f.playerID, "item_key": "no_such_item", "quantity": 1},
		{"type": "credit", "idempotency_key": "b4", "player_id": other, "currency_key": "gems", "amount": 5},
		{"type": "xp", "idempotency_key": "b5", "player_id": f.playerID, "amount": 150},
		{"type": "stat", "idempotency_key": "b6", "player_id": f.playerID, "stat_key": "trolls_defeated", "amount": 2},
		{"type": "flag", "idempotency_key": "b7", "player_id": f.playerID, "flag_key": "talked_to_elder"},
		{"type": "grant", "idempotency_key": "", "player_id": f.playerID, "item_key": "health_potion", "quantity": 1},
		{"type": "teleport", "idempotency_key": "b9", "player_id": f.playerID},
	}
	batch := func(key string, entries []map[string]any) apiResult {
		return f.server("/v1/server/batch", key, map[string]any{"entries": entries})
	}
	res := expect(t, batch("x", entries), 200).obj(t)
	if res["applied"].(float64) != 6 || res["failed"].(float64) != 3 {
		t.Fatalf("batch: %v", res)
	}
	results := res["results"].([]any)
	status := func(i int) float64 { return results[i].(map[string]any)["status"].(float64) }
	code := func(i int) string { return results[i].(map[string]any)["error"].(map[string]any)["code"].(string) }
	if results[0].(map[string]any)["data"].(map[string]any)["quantity"].(float64) != 2 || results[1].(map[string]any)["data"].(map[string]any)["quantity"].(float64) != 3 {
		t.Fatalf("entries for one player apply in order: %v %v", results[0], results[1])
	}
	if status(2) != 404 || code(7) != "IDEMPOTENCY_KEY_REQUIRED" || code(8) != "INVALID_REQUEST" {
		t.Fatalf("failures: %v %v %v", results[2], results[7], results[8])
	}
	if f.count(`SELECT quantity::int FROM inventory WHERE player_id=$1`, f.playerID) != 3 {
		t.Fatal("a failing entry rolled back its neighbours")
	}

	// Retrying the batch replays the applied entries instead of applying them again.
	again := expect(t, batch("y", entries), 200).obj(t)
	if again["applied"].(float64) != 6 || f.count(`SELECT quantity::int FROM inventory WHERE player_id=$1`, f.playerID) != 3 ||
		f.count(`SELECT total_xp::int FROM player_progression WHERE player_id=$1`, f.playerID) != 150 {
		t.Fatalf("retry applied twice: %v", again)
	}
	// Same key, different request: that entry conflicts, the rest still apply.
	conflict := expect(t, batch("z", []map[string]any{
		{"type": "grant", "idempotency_key": "b1", "player_id": f.playerID, "item_key": "health_potion", "quantity": 9},
		{"type": "grant", "idempotency_key": "b10", "player_id": f.playerID, "item_key": "health_potion", "quantity": 1},
	}), 200).obj(t)
	if r := conflict["results"].([]any); r[0].(map[string]any)["error"].(map[string]any)["code"] != "IDEMPOTENCY_CONFLICT" || r[1].(map[string]any)["status"].(float64) != 200 {
		t.Fatalf("conflict: %v", conflict)
	}
	// Another tenant's player is not found; the size limit comes from project_quotas.
	rival := &fixture{t: t, db: f.db, h: f.h}
	rival.setupTenant("rival")
	cross := expect(t, batch("w", []map[string]any{{"type": "grant", "idempotency_key": "x1", "player_id": rival.playerID, "item_key": "health_potion", "quantity": 1}}), 200).obj(t)
	if cross["results"].([]any)[0].(map[string]any)["status"].(float64) != 404 {
		t.Fatalf("cross-tenant: %v", cross)
	}
	if _, err := f.db.Exec(context.Background(), `INSERT INTO project_quotas(project_id,requests_per_minute,writes_per_minute,max_batch_entries) VALUES($1,1000,1000,2)`, f.projectID); err != nil {
		t.Fatal(err)
	}
	f.h = f.newInstance(nil).Handler() // a fresh instance reads the new quota
	if r := expect(t, batch("v", entries[:3]), 422); r.Error.Code != "BATCH_SIZE" {
		t.Fatalf("code=%s", r.Error.Code)
	}
	if n, err := totals.Verify(context.Background(), f.db); err != nil || n != 0 {
		t.Fatalf("batch writes kept goal totals exact: %d %v", n, err)
	}
}

// Quotas and the authentication limiter are shared through PostgreSQL, so two API
// instances together stay within one limit.
func TestIntegrationLimitsHoldAcrossInstances(t *testing.T) {
	f := newFixture(t)
	if _, err := f.db.Exec(context.Background(), `INSERT INTO project_quotas(project_id,requests_per_minute,writes_per_minute,max_batch_entries) VALUES($1,100,5,500)`, f.projectID); err != nil {
		t.Fatal(err)
	}
	a, b := f.newInstance(nil), f.newInstance(nil)
	grant := func(s *Server) apiResult {
		return call(t, s.Handler(), "POST", "/v1/server/players/"+f.playerID+"/items/grant", map[string]any{"item_key": "health_potion", "quantity": 1},
			map[string]string{"X-Server-Credential": f.credential, "Idempotency-Key": "q-" + platform.RandomID()})
	}
	ctx := context.Background()
	for range 3 {
		expect(t, grant(a), 200)
	}
	if err := a.Quotas.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	expect(t, grant(b), 200)
	expect(t, grant(b), 200)
	r := expect(t, grant(b), 429)
	if r.Error.Code != "QUOTA_EXCEEDED" || r.Header.Get("Retry-After") == "" {
		t.Fatalf("quota: %s %v", r.raw, r.Header)
	}
	if err := b.Quotas.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	// a learns b's usage at its next sync (until then the overshoot is bounded by what
	// a admits within one 250 ms sync interval).
	if err := a.Quotas.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	expect(t, grant(a), 429)
	if n := f.count(`SELECT count(*) FROM economy_transactions WHERE player_id=$1`, f.playerID); n != 5 {
		t.Fatalf("writes=%d, want exactly the quota of 5", n)
	}
	// Reads have their own (larger) budget.
	expect(t, call(t, a.Handler(), "GET", "/v1/server/players/"+f.playerID+"/stats", nil, map[string]string{"X-Server-Credential": f.credential}), 200)

	// Login attempts: 3 per IP and route per minute, counted across both instances.
	la, lb := f.newInstance(func(c *Config) { c.AuthRateLimit = 3 }), f.newInstance(func(c *Config) { c.AuthRateLimit = 3 })
	login := map[string]any{"public_environment_id": f.publicID, "email": "owner-player@example.test", "password": "wrong"}
	expect(t, call(t, la.Handler(), "POST", "/v1/players/login", login, nil), 401)
	expect(t, call(t, lb.Handler(), "POST", "/v1/players/login", login, nil), 401)
	expect(t, call(t, la.Handler(), "POST", "/v1/players/login", login, nil), 401)
	expect(t, call(t, lb.Handler(), "POST", "/v1/players/login", login, nil), 429)
}

func TestIntegrationCredentialCacheAndRevocation(t *testing.T) {
	f := newFixture(t)
	a, b := f.newInstance(nil), f.newInstance(nil)
	stats := func(s *Server) apiResult {
		return call(t, s.Handler(), "GET", "/v1/server/players/"+f.playerID+"/stats", nil, map[string]string{"X-Server-Credential": f.credential})
	}
	expect(t, stats(a), 200)
	expect(t, stats(b), 200) // both instances now hold the lookup
	// A wrong secret is rejected even with the prefix cached.
	prefix := strings.SplitN(f.credential, ".", 2)[0]
	expect(t, call(t, a.Handler(), "GET", "/v1/server/players/"+f.playerID+"/stats", nil, map[string]string{"X-Server-Credential": prefix + ".wrong"}), 401)

	var creds []map[string]any
	expect(t, call(t, a.Handler(), "GET", f.envPath("/credentials"), nil, bearer(f.userToken)), 200).into(t, &creds)
	expect(t, call(t, a.Handler(), "POST", "/v1/credentials/"+creds[0]["id"].(string)+"/revoke", nil, bearer(f.userToken)), 200)
	expect(t, stats(a), 401) // the revoking instance drops it at once
	expect(t, stats(b), 200) // another instance may serve it until its cache entry expires…
	b.Env.Now = func() time.Time { return time.Now().Add(31 * time.Second) }
	expect(t, stats(b), 401) // …which is at most the TTL (30 s here, never more than 60 s)
}

func TestIntegrationPartitionsAndRetention(t *testing.T) {
	f := newFixture(t)
	expect(t, f.grant(f.playerID, "health_potion", 1), 200)
	var partition string
	if err := f.db.QueryRow(context.Background(), `SELECT tableoid::regclass::text FROM economy_transactions WHERE player_id=$1`, f.playerID).Scan(&partition); err != nil {
		t.Fatal(err)
	}
	if want := "economy_transactions_" + time.Now().UTC().Format("2006_01"); !strings.HasSuffix(partition, want) {
		t.Fatalf("row landed in %s, want %s", partition, want)
	}
	// Append-only holds on the partition itself, not only the parent.
	if _, err := f.db.Exec(context.Background(), `UPDATE `+partition+` SET quantity=99`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("partition was editable: %v", err)
	}
	if n, err := maintenance.EnsurePartitions(context.Background(), f.db); err != nil || n != 0 {
		t.Fatalf("ensure partitions is idempotent: %d %v", n, err)
	}

	// An expired idempotency record is purged; the key is then treated as new.
	key := "old-key"
	grant := func() apiResult {
		return f.server("/v1/server/players/"+f.playerID+"/items/grant", key, map[string]any{"item_key": "health_potion", "quantity": 1})
	}
	expect(t, grant(), 200)
	expect(t, grant(), 200) // replay
	if f.count(`SELECT quantity::int FROM inventory WHERE player_id=$1`, f.playerID) != 2 {
		t.Fatal("replay applied twice")
	}
	if _, err := f.db.Exec(context.Background(), `UPDATE idempotency_records SET expires_at=now()-interval '1 second' WHERE key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if n, err := maintenance.PurgeIdempotency(context.Background(), f.db, 1); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	expect(t, grant(), 200)
	if f.count(`SELECT quantity::int FROM inventory WHERE player_id=$1`, f.playerID) != 3 {
		t.Fatal("after expiry the key should be treated as new")
	}
}

// Scores are bounded to +-MaxScore: the rank projection stores them as float64, which
// holds integers exactly only up to 2^53. The bound is enforced at the API and by a CHECK
// constraint, so no stored score can be ordered wrongly by Redis.
func TestIntegrationScoreRange(t *testing.T) {
	f := newFixture(t)
	post := func(score int64) apiResult {
		return f.playerPost(f.player, "/v1/leaderboards/global/scores", "s-"+platform.RandomID(), map[string]any{"score": score})
	}
	for _, score := range []int64{leaderboards.MaxScore + 1, -leaderboards.MaxScore - 1, math.MinInt64, math.MaxInt64} {
		if r := expect(t, post(score), 422); r.Error.Code != "VALIDATION_ERROR" {
			t.Fatalf("score %d: code %s", score, r.Error.Code)
		}
	}
	expect(t, post(leaderboards.MaxScore), 201)
	expect(t, post(-leaderboards.MaxScore), 201)
	// The constraint is the last line of defence for code that bypasses the handler.
	if _, err := f.db.Exec(context.Background(), `INSERT INTO score_submissions(leaderboard_id,player_id,project_id,environment_id,score,idempotency_key,request_hash,submitted_at)
		SELECT leaderboard_id,player_id,project_id,environment_id,$1,'direct','x',submitted_at FROM score_submissions LIMIT 1`, int64(leaderboards.MaxScore)+1); err == nil {
		t.Fatal("an out-of-range score was stored")
	}
}

// A batch locks players in sorted order to avoid deadlocks, and its idempotency keys
// are per player, so one player written in two spellings (UUIDs are case-insensitive)
// must be one player: the same entry replays instead of applying twice.
func TestIntegrationBatchPlayerIDCase(t *testing.T) {
	f := newFixture(t)
	expect(t, f.publish(phase5Config()), 201)
	entry := func(id string) map[string]any {
		return map[string]any{"type": "grant", "idempotency_key": "case-1", "player_id": id, "item_key": "health_potion", "quantity": 1}
	}
	for _, id := range []string{f.playerID, strings.ToUpper(f.playerID)} {
		res := expect(t, f.server("/v1/server/batch", "k-"+id, map[string]any{"entries": []map[string]any{entry(id)}}), 200).obj(t)
		if res["applied"].(float64) != 1 {
			t.Fatalf("batch for %s: %v", id, res)
		}
	}
	if n := f.count(`SELECT quantity::int FROM inventory WHERE player_id=$1`, f.playerID); n != 1 {
		t.Fatalf("quantity = %d, want 1: the upper-case spelling applied the entry again", n)
	}
}
