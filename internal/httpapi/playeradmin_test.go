package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The dashboard's player list: newest first, searchable, cursor-paginated, and limited
// to the dashboard user's own environment.
func TestIntegrationPlayerAdminList(t *testing.T) {
	f := newFixture(t)
	path := f.envPath("/players")
	ana, _, _ := f.registerPlayer("ana@example.test", "Ana 100% Real")
	bo, _, _ := f.registerPlayer("bo@example.test", "Bo")
	cy, _, _ := f.registerPlayer("cy_underscore@example.test", "Cy")
	expect(t, call(t, f.h, "POST", "/v1/server/players/"+ana+"/xp/award", map[string]any{"amount": 150}, withKey(map[string]string{"X-Server-Credential": f.credential}, "xp-ana")), 200)

	list := func(query string) ([]map[string]any, string) {
		t.Helper()
		r := expect(t, call(t, f.h, "GET", path+query, nil, bearer(f.userToken)), 200)
		var rows []map[string]any
		r.into(t, &rows)
		var envelope struct {
			Meta struct {
				NextCursor string `json:"next_cursor"`
			} `json:"meta"`
		}
		_ = json.Unmarshal([]byte(r.raw), &envelope)
		return rows, envelope.Meta.NextCursor
	}
	ids := func(rows []map[string]any) []string {
		out := []string{}
		for _, r := range rows {
			out = append(out, r["id"].(string))
		}
		return out
	}

	all, next := list("")
	if got := ids(all); len(got) != 4 || got[0] != cy || got[1] != bo || got[2] != ana || got[3] != f.playerID || next != "" {
		t.Fatalf("list = %v (next %q), want newest first: cy, bo, ana, fixture player", got, next)
	}
	if all[2]["level"].(float64) < 2 || all[2]["total_xp"].(float64) != 150 || all[2]["email"] != "ana@example.test" {
		t.Fatalf("ana's row: %v", all[2])
	}
	for _, row := range all {
		if strings.Contains(strings.ToLower(row["display_name"].(string)+row["id"].(string)), "password") || row["password_hash"] != nil {
			t.Fatalf("a row exposes credentials: %v", row)
		}
	}

	// Search: part of a name or email, case-insensitive, or a whole ID. LIKE wildcards in
	// the search are literal: "%" matches only names with a percent sign.
	for query, want := range map[string]string{"?search=ANA": ana, "?search=bo%40example": bo, "?search=" + strings.ToUpper(cy): cy, "?search=%25": ana, "?search=_under": cy} {
		if got := ids(must(list(query))); len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v, want only %s", query, got, want)
		}
	}
	if got := ids(must(list("?search=_"))); len(got) != 1 {
		t.Errorf("an underscore must not match every name: %v", got)
	}

	// Pages do not overlap and end with no cursor.
	page1, cursor := list("?limit=3")
	if len(page1) != 3 || cursor == "" {
		t.Fatalf("page 1: %d rows, cursor %q", len(page1), cursor)
	}
	page2, end := list("?limit=3&cursor=" + cursor)
	if got := ids(page2); len(got) != 1 || got[0] != f.playerID || end != "" {
		t.Fatalf("page 2 = %v (next %q)", got, end)
	}
	expect(t, call(t, f.h, "GET", path+"?cursor=garbage", nil, bearer(f.userToken)), 400)
	expect(t, call(t, f.h, "GET", path, nil, nil), 401)

	// Another tenant cannot list these players, and sees only their own.
	rival := &fixture{t: t, db: f.db, h: f.h}
	rival.setupTenant("rival")
	expect(t, call(t, f.h, "GET", path, nil, bearer(rival.userToken)), 404)
	if got := ids(must(list2(t, rival, ""))); len(got) != 1 || got[0] != rival.playerID {
		t.Fatalf("rival's list = %v", got)
	}
}

func must(rows []map[string]any, _ string) []map[string]any { return rows }

func list2(t *testing.T, f *fixture, query string) ([]map[string]any, string) {
	t.Helper()
	var rows []map[string]any
	expect(t, call(t, f.h, "GET", f.envPath("/players")+query, nil, bearer(f.userToken)), 200).into(t, &rows)
	return rows, ""
}

// A player's detail routes return exactly what the game sees, and nothing outside the
// owned environment.
func TestIntegrationPlayerAdminDetail(t *testing.T) {
	f := newFixture(t)
	expect(t, f.grant(f.playerID, "health_potion", 3), 200)
	expect(t, f.credit(f.playerID, "gems", 40), 200)
	expect(t, call(t, f.h, "POST", "/v1/server/players/"+f.playerID+"/xp/award", map[string]any{"amount": 120}, withKey(map[string]string{"X-Server-Credential": f.credential}, "xp-1")), 200)
	admin := func(suffix string) apiResult {
		t.Helper()
		return expect(t, call(t, f.h, "GET", f.envPath("/players/"+f.playerID+suffix), nil, bearer(f.userToken)), 200)
	}

	profile := admin("").obj(t)
	if profile["id"] != f.playerID || profile["email"] == nil || profile["active_sessions"].(float64) < 1 || profile["last_session_at"] == nil {
		t.Fatalf("profile: %v", profile)
	}
	if raw := admin("").raw; strings.Contains(raw, "password") || strings.Contains(raw, "token") {
		t.Fatalf("profile exposes credentials: %s", raw)
	}

	// Same handlers, same data: compare with the player's own view and the server's view.
	server := map[string]string{"X-Server-Credential": f.credential}
	for suffix, game := range map[string]apiResult{
		"/inventory":    call(t, f.h, "GET", "/v1/players/me/inventory", nil, bearer(f.player)),
		"/currency":     call(t, f.h, "GET", "/v1/players/me/currency", nil, bearer(f.player)),
		"/transactions": call(t, f.h, "GET", "/v1/players/me/transactions", nil, bearer(f.player)),
		"/progression":  call(t, f.h, "GET", "/v1/players/me/progression", nil, bearer(f.player)),
		"/stats":        call(t, f.h, "GET", "/v1/server/players/"+f.playerID+"/stats", nil, server),
		"/achievements": call(t, f.h, "GET", "/v1/server/players/"+f.playerID+"/achievements", nil, server),
		"/quests":       call(t, f.h, "GET", "/v1/server/players/"+f.playerID+"/quests", nil, server),
	} {
		expect(t, game, 200)
		if got := admin(suffix); string(got.Data) != string(game.Data) {
			t.Errorf("%s differs from the game's view:\nadmin %s\ngame  %s", suffix, got.Data, game.Data)
		}
	}
	if !strings.Contains(string(admin("/inventory").Data), `"health_potion"`) || !strings.Contains(string(admin("/currency").Data), `40`) {
		t.Fatal("the seeded data is missing from the admin view")
	}

	// Isolation: another tenant's player, a player of another environment of this
	// project, an unknown player and a malformed ID are all 404; no token is 401.
	rival := &fixture{t: t, db: f.db, h: f.h}
	rival.setupTenant("rival")
	expect(t, call(t, f.h, "GET", f.envPath("/players/"+rival.playerID), nil, bearer(f.userToken)), 404)
	expect(t, call(t, f.h, "GET", rival.envPath("/players/"+f.playerID+"/inventory"), nil, bearer(rival.userToken)), 404)
	expect(t, call(t, f.h, "GET", f.envPath("/players/"+f.playerID), nil, bearer(rival.userToken)), 404)
	prod := expect(t, call(t, f.h, "POST", "/v1/projects/"+f.projectID+"/environments", map[string]any{"name": "production"}, bearer(f.userToken)), 201).obj(t)
	expect(t, call(t, f.h, "GET", "/v1/projects/"+f.projectID+"/environments/"+prod["id"].(string)+"/players/"+f.playerID, nil, bearer(f.userToken)), 404)
	expect(t, call(t, f.h, "GET", f.envPath("/players/00000000-0000-0000-0000-000000000000/quests"), nil, bearer(f.userToken)), 404)
	expect(t, call(t, f.h, "GET", f.envPath("/players/not-a-uuid"), nil, bearer(f.userToken)), 404)
	expect(t, call(t, f.h, "GET", f.envPath("/players/"+f.playerID), nil, nil), 401)
	// A player token is not a dashboard token.
	expect(t, call(t, f.h, "GET", f.envPath("/players/"+f.playerID), nil, bearer(f.player)), 401)
}

// Management: an admin performs the game's actions on a player with a dashboard token.
// They go through the same handlers as the game, so the rules hold (a balance cannot go
// below zero, a goal pays once) and the data lands where the game reads it.
func TestIntegrationPlayerAdminActions(t *testing.T) {
	f := newFixture(t)
	clock := f.withClock()
	clock.set(base)
	expect(t, f.publish(questsConfig(base.AddDate(0, 1, 0), base.AddDate(0, 2, 0))), 201)
	pid := f.playerID
	post := func(suffix, key string, body any) apiResult {
		return call(t, f.h, "POST", f.envPath("/players/"+pid+suffix), body, withKey(bearer(f.userToken), key))
	}

	// Economy: grant, credit, spend; the player's own view shows the result.
	expect(t, post("/items/grant", "g1", map[string]any{"item_key": "iron_sword", "quantity": 1, "reason": "support"}), 200)
	expect(t, post("/currency/credit", "c1", map[string]any{"currency_key": "gems", "amount": 40, "reason": "compensation"}), 200)
	spend := expect(t, post("/currency/spend", "s1", map[string]any{"currency_key": "gems", "amount": 15}), 200).obj(t)
	if spend["balance"].(float64) != 25 || f.balance("gems") != 25 {
		t.Fatalf("balance after spend: %v / %d", spend, f.balance("gems"))
	}
	if r := post("/currency/spend", "s2", map[string]any{"currency_key": "gems", "amount": 26}); r.Status != 422 {
		t.Fatalf("overspend status=%d: %s", r.Status, r.raw)
	}
	expect(t, post("/inventory/equip", "e1", map[string]any{"item_key": "iron_sword", "slot": "weapon"}), 200)
	if !strings.Contains(string(expect(t, call(t, f.h, "GET", "/v1/players/me/inventory", nil, bearer(f.player)), 200).Data), `"iron_sword"`) {
		t.Fatal("the player does not see the admin's grant")
	}

	// Idempotency: a replay returns the original result without crediting twice, a
	// reused key with another body conflicts, and no key is rejected.
	expect(t, post("/currency/credit", "c1", map[string]any{"currency_key": "gems", "amount": 40, "reason": "compensation"}), 200)
	if f.balance("gems") != 25 {
		t.Fatalf("replay credited again: %d", f.balance("gems"))
	}
	if r := post("/currency/credit", "c1", map[string]any{"currency_key": "gems", "amount": 1}); r.Status != 409 {
		t.Fatalf("conflict status=%d", r.Status)
	}
	expect(t, call(t, f.h, "POST", f.envPath("/players/"+pid+"/currency/credit"), map[string]any{"currency_key": "gems", "amount": 1}, bearer(f.userToken)), 400)
	// The same key from the game server is a different namespace, so it is not a replay.
	expect(t, f.server(f.serverPath(pid, "/currency/credit"), "c1", map[string]any{"currency_key": "gems", "amount": 1}), 200)
	if f.balance("gems") != 26 {
		t.Fatalf("server key shared the admin's namespace: %d", f.balance("gems"))
	}

	// Quests: accept, abandon and accept again before any progress; a locked quest refuses.
	expect(t, post("/quests/chain_one/accept", "q1", nil), 200)
	expect(t, post("/quests/chain_one/abandon", "q2", nil), 200)
	expect(t, post("/quests/chain_one/accept", "q3", nil), 200)
	if r := post("/quests/chain_two/accept", "q4", nil); r.Status != 422 {
		t.Fatalf("locked quest status=%d: %s", r.Status, r.raw)
	}

	// XP, score, stats and flags.
	expect(t, post("/xp/award", "x1", map[string]any{"amount": 60}), 200)
	expect(t, post("/leaderboards/global/scores", "lb1", map[string]any{"score": 900}), 201)
	for i := 0; i < 3; i++ {
		expect(t, post("/stats/trolls_defeated/increment", fmt.Sprintf("t%d", i), map[string]any{"amount": 1}), 200)
	}
	expect(t, post("/flags/talked_to_elder/set", "f1", nil), 200)
	if f.count(`SELECT count(*) FROM xp_transactions WHERE player_id=$1`, pid) != 1 || f.count(`SELECT count(*) FROM leaderboard_best_scores WHERE player_id=$1`, pid) != 1 {
		t.Fatal("xp or score rows are missing")
	}

	// Achievement claim: the goal completes from the admin's stat and flag writes, and
	// pays once.
	expect(t, post("/achievements/troll_slayer/claim", "a1", nil), 200)
	if r := post("/achievements/troll_slayer/claim", "a2", nil); r.Status != 409 {
		t.Fatalf("second claim status=%d: %s", r.Status, r.raw)
	}
	if f.balance("gems") != 36 {
		t.Fatalf("gems after claim: %d", f.balance("gems"))
	}

	// Quests: claim what the earlier writes completed.
	expect(t, post("/quests/chain_one/claim", "q5", nil), 200)
	// The server-claimed quest can be claimed by an admin too.
	f.trolls(1)
	expect(t, post("/quests/server_only/claim", "q6", nil), 200)

	// Isolation and credentials: another tenant's admin, another environment, no token,
	// and a player token all fail before anything is written.
	rival := &fixture{t: t, db: f.db, h: f.h}
	rival.setupTenant("rival")
	before := f.balance("gems")
	for _, tok := range []string{rival.userToken} {
		r := call(t, f.h, "POST", f.envPath("/players/"+pid+"/currency/credit"), map[string]any{"currency_key": "gems", "amount": 5}, withKey(bearer(tok), "x"))
		if r.Status != 404 {
			t.Fatalf("rival admin status=%d: %s", r.Status, r.raw)
		}
	}
	expect(t, call(t, f.h, "POST", rival.envPath("/players/"+pid+"/currency/credit"), map[string]any{"currency_key": "gems", "amount": 5}, withKey(bearer(rival.userToken), "x")), 404)
	expect(t, call(t, f.h, "POST", f.envPath("/players/"+pid+"/currency/credit"), map[string]any{"currency_key": "gems", "amount": 5}, withKey(nil, "x")), 401)
	expect(t, call(t, f.h, "POST", f.envPath("/players/"+pid+"/currency/credit"), map[string]any{"currency_key": "gems", "amount": 5}, withKey(bearer(f.player), "x")), 401)
	if f.balance("gems") != before {
		t.Fatalf("a refused request changed the balance: %d -> %d", before, f.balance("gems"))
	}
}
