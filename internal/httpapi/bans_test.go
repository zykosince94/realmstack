package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// banAdmin calls a dashboard route on the fixture's player.
func (f *fixture) banAdmin(method, suffix, key string, body any) apiResult {
	headers := bearer(f.userToken)
	if key != "" {
		headers = withKey(headers, key)
	}
	return call(f.t, f.h, method, f.envPath("/players/"+f.playerID+suffix), body, headers)
}

func (f *fixture) login(password string) apiResult {
	return call(f.t, f.h, "POST", "/v1/players/login", map[string]any{"public_environment_id": f.publicID, "email": "owner-player@example.test", "password": password}, nil)
}

func errorDetails(t *testing.T, r apiResult) map[string]any {
	t.Helper()
	var envelope struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.raw), &envelope); err != nil {
		t.Fatalf("decode %s: %v", r.raw, err)
	}
	return envelope.Error.Details
}

// A ban stops the player everywhere they prove who they are (access token, login,
// refresh), leaves the game server and the dashboard able to act, and keeps a history.
func TestIntegrationPlayerBans(t *testing.T) {
	f := newFixture(t)
	clock := f.withClock()
	clock.set(base)
	pid := f.playerID

	// Validation, and nothing to lift yet.
	wantCode(t, f.banAdmin("POST", "/ban", "v1", map[string]any{"reason": "  "}), 422, "VALIDATION_ERROR")
	wantCode(t, f.banAdmin("POST", "/ban", "v2", map[string]any{"reason": "x", "expires_at": base.Add(-time.Hour)}), 422, "VALIDATION_ERROR")
	wantCode(t, f.banAdmin("POST", "/ban/lift", "l0", nil), 422, "NOT_BANNED")
	expect(t, f.banAdmin("POST", "/ban", "", map[string]any{"reason": "x"}), 400) // no Idempotency-Key
	expect(t, call(t, f.h, "GET", "/v1/players/me", nil, bearer(f.player)), 200)

	// Ban for an hour: every refresh token is revoked in the same transaction.
	until := base.Add(time.Hour)
	banBody := map[string]any{"reason": "Cheating in ranked matches", "expires_at": until}
	res := expect(t, f.banAdmin("POST", "/ban", "b1", banBody), 201).obj(t)
	ban := res["ban"].(map[string]any)
	if ban["status"] != "active" || ban["created_by"] != "owner@example.test" || res["sessions_revoked"].(float64) < 1 {
		t.Fatalf("ban result: %v", res)
	}
	if again := expect(t, f.banAdmin("POST", "/ban", "b1", banBody), 201).obj(t); again["ban"].(map[string]any)["id"] != ban["id"] {
		t.Fatalf("replay created another ban: %v", again)
	}
	if n := f.count(`SELECT count(*) FROM player_bans WHERE player_id=$1`, pid); n != 1 {
		t.Fatalf("bans after replay: %d", n)
	}

	// The still-valid access token is refused at once, with the reason and expiry.
	r := call(t, f.h, "GET", "/v1/players/me", nil, bearer(f.player))
	wantCode(t, r, 403, "PLAYER_BANNED")
	if d := errorDetails(t, r); d["reason"] != "Cheating in ranked matches" || d["expires_at"] == nil {
		t.Fatalf("ban details: %v", d)
	}
	wantCode(t, f.playerPost(f.player, "/v1/leaderboards/global/scores", "score-1", map[string]any{"score": 10}), 403, "PLAYER_BANNED")
	wantCode(t, call(t, f.h, "GET", "/v1/leaderboards/global", nil, bearer(f.player)), 403, "PLAYER_BANNED")
	// A wrong password never reveals the ban; the right one learns about it.
	wantCode(t, f.login("not-the-password"), 401, "INVALID_CREDENTIALS")
	wantCode(t, f.login("PlayerPass123!"), 403, "PLAYER_BANNED")
	// The revoked refresh token says "banned", not "reuse detected".
	wantCode(t, call(t, f.h, "POST", "/v1/players/refresh", map[string]any{"refresh_token": f.refresh}, nil), 403, "PLAYER_BANNED")

	// The game server and the dashboard can still act on the player.
	expect(t, f.credit(pid, "gems", 5), 200)
	expect(t, f.banAdmin("GET", "/currency", "", nil), 200)

	// Profile, directory flag and the banned-only filter.
	profile := expect(t, f.banAdmin("GET", "", "", nil), 200).obj(t)
	if profile["ban"] == nil || profile["ban"].(map[string]any)["reason"] != "Cheating in ranked matches" || profile["active_sessions"].(float64) != 0 {
		t.Fatalf("profile: %v", profile)
	}
	other, _, _ := f.registerPlayer("other@example.test", "Other")
	var rows []map[string]any
	expect(t, call(t, f.h, "GET", f.envPath("/players"), nil, bearer(f.userToken)), 200).into(t, &rows)
	for _, row := range rows {
		if want := row["id"] == pid; row["banned"] != want {
			t.Fatalf("banned flag for %v: %v", row["id"], row["banned"])
		}
	}
	expect(t, call(t, f.h, "GET", f.envPath("/players?banned=true"), nil, bearer(f.userToken)), 200).into(t, &rows)
	if len(rows) != 1 || rows[0]["id"] != pid {
		t.Fatalf("banned filter: %v (other player %s)", rows, other)
	}

	// Banning again replaces the ban (here: made permanent) and keeps the old one.
	perm := expect(t, f.banAdmin("POST", "/ban", "b2", map[string]any{"reason": "Repeated cheating"}), 201).obj(t)["ban"].(map[string]any)
	if perm["expires_at"] != nil {
		t.Fatalf("permanent ban has an expiry: %v", perm)
	}
	var history []map[string]any
	expect(t, f.banAdmin("GET", "/bans", "", nil), 200).into(t, &history)
	if len(history) != 2 || history[0]["status"] != "active" || history[1]["status"] != "replaced" || history[1]["ended_by"] != "owner@example.test" {
		t.Fatalf("history: %v", history)
	}

	// Lifting: the access token works again; the revoked refresh token stays revoked
	// (so the client signs in again), and signing in works.
	lifted := expect(t, f.banAdmin("POST", "/ban/lift", "l1", map[string]any{"note": "Appeal accepted"}), 200).obj(t)["ban"].(map[string]any)
	if lifted["status"] != "lifted" || lifted["end_note"] != "Appeal accepted" {
		t.Fatalf("lifted: %v", lifted)
	}
	expect(t, call(t, f.h, "GET", "/v1/players/me", nil, bearer(f.player)), 200)
	wantCode(t, call(t, f.h, "POST", "/v1/players/refresh", map[string]any{"refresh_token": f.refresh}, nil), 401, "REFRESH_TOKEN_REUSE")
	expect(t, f.login("PlayerPass123!"), 200)
	wantCode(t, f.banAdmin("POST", "/ban/lift", "l2", nil), 422, "NOT_BANNED")
	// Replaying the old ban request returns its stored result without banning again:
	// the cache re-reads the database instead of trusting the replayed response.
	expect(t, f.banAdmin("POST", "/ban", "b1", banBody), 201)
	expect(t, call(t, f.h, "GET", "/v1/players/me", nil, bearer(f.player)), 200)

	// Expiry: a one-hour ban stops applying after the hour, with no job involved.
	expect(t, f.banAdmin("POST", "/ban", "b3", banBody), 201)
	wantCode(t, call(t, f.h, "GET", "/v1/players/me", nil, bearer(f.player)), 403, "PLAYER_BANNED")
	clock.set(until.Add(time.Minute))
	expect(t, call(t, f.h, "GET", "/v1/players/me", nil, bearer(f.player)), 200)
	expect(t, f.login("PlayerPass123!"), 200)
	if p := expect(t, f.banAdmin("GET", "", "", nil), 200).obj(t); p["ban"] != nil {
		t.Fatalf("expired ban still in the profile: %v", p["ban"])
	}
	expect(t, f.banAdmin("GET", "/bans", "", nil), 200).into(t, &history)
	if history[0]["status"] != "expired" {
		t.Fatalf("expired ban status: %v", history[0])
	}
	wantCode(t, f.banAdmin("POST", "/ban/lift", "l3", nil), 422, "NOT_BANNED")

	// Isolation: another tenant's admin, another environment and a player token cannot ban.
	rival := &fixture{t: t, db: f.db, h: f.h}
	rival.setupTenant("rival")
	expect(t, call(t, f.h, "POST", f.envPath("/players/"+pid+"/ban"), map[string]any{"reason": "x"}, withKey(bearer(rival.userToken), "r1")), 404)
	expect(t, call(t, f.h, "POST", rival.envPath("/players/"+pid+"/ban"), map[string]any{"reason": "x"}, withKey(bearer(rival.userToken), "r2")), 404)
	expect(t, call(t, f.h, "POST", f.envPath("/players/"+pid+"/ban"), map[string]any{"reason": "x"}, withKey(bearer(f.player), "r3")), 401)
	expect(t, call(t, f.h, "GET", f.envPath("/players/"+pid+"/bans"), nil, bearer(rival.userToken)), 404)
	if n := f.count(`SELECT count(*) FROM player_bans WHERE player_id=$1 AND ended_at IS NULL AND expires_at IS NULL`, pid); n != 0 {
		t.Fatalf("a refused request banned the player: %d", n)
	}
}

// A ban made on one API instance reaches the others within BanSyncInterval, through
// the cache's delta sync (including a replacement, whose two rows share a timestamp).
func TestIntegrationBanAcrossInstances(t *testing.T) {
	f := newFixture(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := Config{JWTSecret: strings.Repeat("k", 40), AccessTTL: time.Minute, RefreshTTL: time.Hour, BodyLimit: 1 << 20, AuthRateLimit: 10000, BanSyncInterval: 20 * time.Millisecond}
	a, b := New(f.db, cfg, logger).Handler(), New(f.db, cfg, logger).Handler()
	get := func(onB bool) int {
		h := a
		if onB {
			h = b
		}
		return call(t, h, "GET", "/v1/players/me", nil, bearer(f.player)).Status
	}
	eventually := func(what string, want int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for get(true) != want {
			if time.Now().After(deadline) {
				t.Fatalf("instance B never answered %d %s", want, what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	banOnA := func(key string, body any) apiResult {
		return call(t, a, "POST", f.envPath("/players/"+f.playerID+"/ban"), body, withKey(bearer(f.userToken), key))
	}

	if get(true) != 200 { // B loads its copy before the ban exists
		t.Fatal("B refused before any ban")
	}
	expect(t, banOnA("x1", map[string]any{"reason": "Botting"}), 201)
	if get(false) != 403 {
		t.Fatal("the instance that banned did not refuse at once")
	}
	eventually("after the ban", 403)
	expect(t, banOnA("x2", map[string]any{"reason": "Botting, second offence"}), 201) // replaces the first ban
	time.Sleep(60 * time.Millisecond)                                                 // several sync intervals
	if got := get(true); got != 403 {
		t.Fatalf("B answered %d after the ban was replaced", got)
	}
	expect(t, call(t, a, "POST", f.envPath("/players/"+f.playerID+"/ban/lift"), nil, withKey(bearer(f.userToken), "x3")), 200)
	eventually("after the lift", 200)
}
