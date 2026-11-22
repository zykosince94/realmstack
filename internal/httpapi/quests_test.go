package httpapi

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"realmstack-backend/internal/platform"
)

// questsConfig extends the achievement config with a three-quest chain, a daily, an
// event quest and a server-claimed auto-accept quest.
func questsConfig(eventFrom, eventUntil time.Time) map[string]any {
	cfg := achievementsConfig()
	cfg["stats"] = append(cfg["stats"].([]map[string]any),
		map[string]any{"key": "entered_mine", "name": "Entered the mine", "kind": "flag"},
		map[string]any{"key": "ore_delivered", "name": "Ore delivered", "kind": "flag"})
	trolls := func(n int) []map[string]any {
		return []map[string]any{{"type": "stat", "stat_key": "trolls_defeated", "target": n}}
	}
	flag := func(k string) []map[string]any { return []map[string]any{{"type": "flag", "flag_key": k}} }
	credits := func(n int) map[string]any {
		return map[string]any{"currencies": []map[string]any{{"currency_key": "credits", "amount": n}}}
	}
	cfg["quests"] = []map[string]any{
		{"key": "chain_one", "name": "Chain One", "chain": "Main", "stages": []map[string]any{{"key": "talk", "name": "Talk", "conditions": flag("talked_to_elder")}}, "rewards": credits(10)},
		{"key": "chain_two", "name": "Chain Two", "chain": "Main", "requires_quests": []string{"chain_one"}, "stages": []map[string]any{
			{"key": "enter", "name": "Enter the mine", "conditions": flag("entered_mine"), "rewards": credits(5)},
			{"key": "fight", "name": "Defeat 3 trolls", "conditions": trolls(3), "rewards": map[string]any{"items": []map[string]any{{"item_key": "health_potion", "quantity": 1}}}},
			{"key": "deliver", "name": "Deliver the ore", "conditions": flag("ore_delivered")},
		}, "rewards": map[string]any{"currencies": []map[string]any{{"currency_key": "gems", "amount": 5}}, "xp": 20}},
		{"key": "chain_three", "name": "Chain Three", "chain": "Main", "requires_quests": []string{"chain_two"}, "stages": []map[string]any{{"key": "s", "name": "One more troll", "conditions": trolls(1)}}, "rewards": credits(1)},
		{"key": "daily", "name": "Daily", "repeat": "daily", "stages": []map[string]any{{"key": "hunt", "name": "Hunt", "conditions": trolls(2)}}, "rewards": credits(7)},
		{"key": "event", "name": "Event", "available_from": eventFrom, "available_until": eventUntil, "stages": []map[string]any{{"key": "s", "name": "Event trolls", "conditions": trolls(2)}}, "rewards": credits(3)},
		{"key": "server_only", "name": "Server Only", "claimable_by": "server", "auto_accept": true, "stages": []map[string]any{{"key": "s", "name": "A troll", "conditions": trolls(1)}}, "rewards": credits(2)},
	}
	return cfg
}

// testClock points the fixture at a server whose clock the test controls. Tokens are
// checked against that clock, so set re-issues the fixture's tokens at each jump.
type testClock struct {
	f            *fixture
	srv          *Server
	user, player platform.Principal
}

func (f *fixture) withClock() *testClock {
	srv := newServer(f.t, f.db)
	u, ok1 := srv.Env.ReadToken("Bearer "+f.userToken, platform.KindUser)
	p, ok2 := srv.Env.ReadToken("Bearer "+f.player, platform.KindPlayer)
	if !ok1 || !ok2 {
		f.t.Fatal("fixture tokens are not readable")
	}
	f.h = srv.Handler()
	return &testClock{f: f, srv: srv, user: u, player: p}
}

func (c *testClock) set(at time.Time) {
	c.srv.Env.Now = func() time.Time { return at }
	c.f.userToken = c.srv.Env.Token(c.user, time.Minute)
	c.f.player = c.srv.Env.Token(c.player, time.Minute)
}

func (f *fixture) quests() map[string]map[string]any {
	f.t.Helper()
	var list []map[string]any
	expect(f.t, call(f.t, f.h, "GET", "/v1/players/me/quests", nil, bearer(f.player)), 200).into(f.t, &list)
	out := map[string]map[string]any{}
	for _, q := range list {
		out[q["key"].(string)] = q
	}
	return out
}

func (f *fixture) quest(key, action, idem string) apiResult {
	return f.playerPost(f.player, "/v1/players/me/quests/"+key+"/"+action, idem, nil)
}

func (f *fixture) trolls(n int) {
	f.t.Helper()
	expect(f.t, f.server(f.serverPath(f.playerID, "/stats/trolls_defeated/increment"), "trolls-"+platform.RandomID(), map[string]any{"amount": n}), 200)
}

func (f *fixture) setFlag(key string) {
	f.t.Helper()
	expect(f.t, f.server(f.serverPath(f.playerID, "/flags/"+key+"/set"), "flag-"+platform.RandomID(), nil), 200)
}

func (f *fixture) balance(currency string) int {
	f.t.Helper()
	return f.count(`SELECT COALESCE((SELECT amount::int FROM balances b JOIN currency_definitions d ON d.id=b.currency_id WHERE b.player_id=$1 AND d.currency_key=$2),0)`, f.playerID, currency)
}

func wantStatus(t *testing.T, q map[string]any, status string) {
	t.Helper()
	if q["status"] != status {
		t.Fatalf("%s: status=%v want %s (%v)", q["key"], q["status"], status, q)
	}
}

func wantCode(t *testing.T, r apiResult, status int, code string) {
	t.Helper()
	if expect(t, r, status); r.Error.Code != code {
		t.Fatalf("code=%s want %s: %s", r.Error.Code, code, r.raw)
	}
}

var base = time.Date(2027, 3, 1, 9, 0, 0, 0, time.UTC)

// TestIntegrationQuestChain covers a three-quest chain that unlocks in order, staged
// progress, combined claims, replay and the rule that quest rewards never count toward
// goals.
func TestIntegrationQuestChain(t *testing.T) {
	f := newFixture(t)
	clock := f.withClock()
	clock.set(base)
	expect(t, f.publish(questsConfig(base.AddDate(0, 1, 0), base.AddDate(0, 2, 0))), 201)

	q := f.quests()
	wantStatus(t, q["chain_one"], "available")
	wantStatus(t, q["chain_two"], "locked")
	wantStatus(t, q["chain_three"], "locked")
	if lb := q["chain_two"]["locked_by"].(map[string]any)["quests"].([]any); len(lb) != 1 || lb[0] != "chain_one" {
		t.Fatalf("locked_by=%v", lb)
	}
	wantCode(t, f.quest("chain_two", "accept", "a2-early"), 422, "QUEST_LOCKED")

	accepted := expect(t, f.quest("chain_one", "accept", "a1"), 200).obj(t)
	wantStatus(t, accepted, "active")
	if replay := expect(t, f.quest("chain_one", "accept", "a1"), 200).obj(t); !reflect.DeepEqual(accepted, replay) {
		t.Fatalf("replay differs: %v vs %v", accepted, replay)
	}
	wantCode(t, f.quest("chain_one", "accept", "a1-again"), 409, "QUEST_ALREADY_ACTIVE")
	wantCode(t, f.quest("chain_one", "claim", "c1-early"), 422, "NOT_COMPLETED")

	f.setFlag("talked_to_elder")
	q = f.quests()
	wantStatus(t, q["chain_one"], "completed")
	// A complete non-repeating quest unlocks the next one even before it is claimed.
	wantStatus(t, q["chain_two"], "available")
	claim := expect(t, f.quest("chain_one", "claim", "c1"), 200).obj(t)
	if replay := expect(t, f.quest("chain_one", "claim", "c1"), 200).obj(t); !reflect.DeepEqual(claim, replay) {
		t.Fatalf("claim replay differs")
	}
	wantCode(t, f.quest("chain_one", "claim", "c1-again"), 409, "ALREADY_CLAIMED")
	wantCode(t, f.quest("chain_one", "accept", "a1-after"), 409, "ALREADY_CLAIMED")
	if got := f.balance("credits"); got != 10 {
		t.Fatalf("credits=%d want 10", got)
	}

	// Stages advance only in order: later stages' conditions are met first.
	clock.set(base.Add(time.Hour))
	expect(t, f.quest("chain_two", "accept", "a2"), 200)
	f.trolls(3)
	f.setFlag("ore_delivered")
	two := f.quests()["chain_two"]
	stages := two["stages"].([]any)
	if two["current_stage"].(float64) != 0 || stages[1].(map[string]any)["complete"] != false || stages[2].(map[string]any)["complete"] != false {
		t.Fatalf("later stages completed before the first: %v", two)
	}
	if met := stages[1].(map[string]any)["progress"].([]any)[0].(map[string]any)["met"]; met != true {
		t.Fatalf("stage 2 conditions should be met (just not complete): %v", stages[1])
	}
	wantCode(t, f.quest("chain_two", "claim", "c2-early"), 422, "NOT_COMPLETED")
	wantStatus(t, f.quests()["chain_three"], "locked")

	f.setFlag("entered_mine")
	two = f.quests()["chain_two"]
	wantStatus(t, two, "completed")
	claimable := two["claimable"].(map[string]any)
	if fmt.Sprint(claimable["stages"]) != "[0 1]" || claimable["completion"] != true {
		t.Fatalf("claimable=%v (stage 2 has no reward)", claimable)
	}
	paid := expect(t, f.quest("chain_two", "claim", "c2"), 200).obj(t)["paid"].([]any)
	if len(paid) != 3 || paid[2].(map[string]any)["stage_index"].(float64) != -1 {
		t.Fatalf("paid=%v", paid)
	}
	if f.balance("credits") != 15 || f.balance("gems") != 5 {
		t.Fatalf("credits=%d gems=%d", f.balance("credits"), f.balance("gems"))
	}
	if n := f.count(`SELECT count(*) FROM economy_transactions WHERE player_id=$1 AND source='quest'`, f.playerID); n != 4 {
		t.Fatalf("quest-tagged economy rows=%d want 4", n)
	}
	if n := f.count(`SELECT count(*) FROM xp_transactions WHERE player_id=$1 AND source='quest'`, f.playerID); n != 1 {
		t.Fatalf("quest-tagged xp rows=%d want 1", n)
	}
	// The potion paid by stage 2 does not count toward potion_hoarder.
	if a := f.achievements(f.player); a["potion_hoarder"]["status"] != "in_progress" {
		t.Fatalf("quest reward completed an achievement: %v", a["potion_hoarder"])
	}

	// Third quest: activity before acceptance does not count.
	wantStatus(t, f.quests()["chain_three"], "available")
	clock.set(base.Add(2 * time.Hour))
	expect(t, f.quest("chain_three", "accept", "a3"), 200)
	three := f.quests()["chain_three"]
	if p := three["stages"].([]any)[0].(map[string]any)["progress"].([]any)[0].(map[string]any); p["current"].(float64) != 0 {
		t.Fatalf("earlier trolls counted: %v", p)
	}
	f.trolls(1)
	wantStatus(t, f.quests()["chain_three"], "completed")
}

// TestIntegrationQuestAbandonAndReaccept: abandon/re-accept restarts progress without
// paying stage rewards twice.
func TestIntegrationQuestAbandonAndReaccept(t *testing.T) {
	f := newFixture(t)
	clock := f.withClock()
	clock.set(base)
	expect(t, f.publish(questsConfig(base.AddDate(0, 1, 0), base.AddDate(0, 2, 0))), 201)
	f.setFlag("talked_to_elder")
	expect(t, f.quest("chain_one", "accept", "a1"), 200)
	expect(t, f.quest("chain_one", "claim", "c1"), 200)

	expect(t, f.quest("chain_two", "accept", "a2"), 200)
	f.setFlag("entered_mine")
	f.trolls(2)
	paid := expect(t, f.quest("chain_two", "claim", "c2-stage1"), 200).obj(t)["paid"].([]any)
	if len(paid) != 1 || paid[0].(map[string]any)["stage_key"] != "enter" {
		t.Fatalf("paid=%v", paid)
	}

	clock.set(base.Add(time.Minute))
	abandoned := expect(t, f.quest("chain_two", "abandon", "ab1"), 200).obj(t)
	wantStatus(t, abandoned, "available")
	wantCode(t, f.quest("chain_two", "abandon", "ab2"), 422, "QUEST_NOT_ACTIVE")
	wantCode(t, f.quest("chain_two", "claim", "c2-abandoned"), 422, "QUEST_NOT_ACTIVE")
	f.trolls(5) // between abandoning and accepting again: does not count

	clock.set(base.Add(2 * time.Minute))
	expect(t, f.quest("chain_two", "accept", "a2-again"), 200)
	two := f.quests()["chain_two"]
	stages := two["stages"].([]any)
	enter, fight := stages[0].(map[string]any), stages[1].(map[string]any)
	// The flag is state, so stage 1 is complete again, but its reward stays claimed.
	if enter["complete"] != true || enter["claimed"] != true || fight["progress"].([]any)[0].(map[string]any)["current"].(float64) != 0 {
		t.Fatalf("after re-accept: %v", stages)
	}
	if fmt.Sprint(two["claimable"].(map[string]any)["stages"]) != "[]" {
		t.Fatalf("a claimed stage became claimable again: %v", two["claimable"])
	}
	f.trolls(3)
	paid = expect(t, f.quest("chain_two", "claim", "c2-stage2"), 200).obj(t)["paid"].([]any)
	if len(paid) != 1 || paid[0].(map[string]any)["stage_key"] != "fight" {
		t.Fatalf("paid=%v", paid)
	}
	if got := f.balance("credits"); got != 15 {
		t.Fatalf("credits=%d want 10 + 5 (stage 1 paid once)", got)
	}
}

// TestIntegrationDailyQuestResetsAtUTCMidnight: "a daily quest can be completed and
// claimed again the next UTC day but not twice in one day."
func TestIntegrationDailyQuestResetsAtUTCMidnight(t *testing.T) {
	f := newFixture(t)
	clock := f.withClock()
	day1 := time.Date(2027, 3, 1, 23, 0, 0, 0, time.UTC)
	day2 := day1.Add(time.Hour) // 00:00 UTC
	clock.set(day1)
	expect(t, f.publish(questsConfig(day1.AddDate(0, 1, 0), day1.AddDate(0, 2, 0))), 201)

	expect(t, f.quest("daily", "accept", "d1"), 200)
	f.trolls(2)
	daily := f.quests()["daily"]
	wantStatus(t, daily, "completed")
	if daily["period_key"] != "2027-03-01" || daily["resets_at"] != "2027-03-02T00:00:00Z" {
		t.Fatalf("period=%v resets_at=%v", daily["period_key"], daily["resets_at"])
	}
	expect(t, f.quest("daily", "claim", "c1"), 200)
	wantCode(t, f.quest("daily", "claim", "c1-again"), 409, "ALREADY_CLAIMED")
	wantCode(t, f.quest("daily", "accept", "d1-again"), 409, "ALREADY_CLAIMED")

	clock.set(day2.Add(-time.Microsecond))
	wantStatus(t, f.quests()["daily"], "claimed")

	clock.set(day2)
	daily = f.quests()["daily"]
	wantStatus(t, daily, "available")
	if daily["period_key"] != "2027-03-02" {
		t.Fatalf("period=%v", daily["period_key"])
	}
	expect(t, f.quest("daily", "accept", "d2"), 200)
	if p := f.quests()["daily"]["stages"].([]any)[0].(map[string]any)["progress"].([]any)[0].(map[string]any); p["current"].(float64) != 0 {
		t.Fatalf("yesterday's trolls counted today: %v", p)
	}
	f.trolls(2)
	expect(t, f.quest("daily", "claim", "c2"), 200)
	if got := f.balance("credits"); got != 14 {
		t.Fatalf("credits=%d want 7 x 2", got)
	}
	if n := f.count(`SELECT count(*) FROM quest_claims WHERE player_id=$1 AND stage_index=-1`, f.playerID); n != 2 {
		t.Fatalf("completion claims=%d want one per day", n)
	}
}

// TestIntegrationEventQuestWindow: "an event quest counts only activity inside its window."
func TestIntegrationEventQuestWindow(t *testing.T) {
	f := newFixture(t)
	clock := f.withClock()
	from, until := base.Add(24*time.Hour), base.Add(48*time.Hour)
	clock.set(base)
	expect(t, f.publish(questsConfig(from, until)), 201)
	wantStatus(t, f.quests()["event"], "upcoming")
	wantCode(t, f.quest("event", "accept", "e0"), 422, "QUEST_NOT_AVAILABLE")

	clock.set(from)
	f.trolls(1) // inside the window, but before acceptance
	clock.set(from.Add(time.Hour))
	expect(t, f.quest("event", "accept", "e1"), 200)
	f.trolls(1)
	clock.set(until) // the window end is exclusive
	f.trolls(5)
	event := f.quests()["event"]
	wantStatus(t, event, "expired")
	if p := event["stages"].([]any)[0].(map[string]any)["progress"].([]any)[0].(map[string]any); p["current"].(float64) != 1 {
		t.Fatalf("progress=%v, want only the in-window kill after acceptance", p)
	}
	if event["expires_at"] != until.Format(time.RFC3339) {
		t.Fatalf("expires_at=%v", event["expires_at"])
	}
	wantCode(t, f.quest("event", "accept", "e2"), 422, "QUEST_EXPIRED")
	wantCode(t, f.quest("event", "claim", "e-claim"), 422, "NOT_COMPLETED")

	// One more kill at the last instant inside the window completes it; it stays
	// claimable after the window closes.
	clock.set(until.Add(-time.Microsecond))
	f.trolls(1)
	clock.set(until.Add(time.Hour))
	wantStatus(t, f.quests()["event"], "completed")
	expect(t, f.quest("event", "claim", "e-claim-2"), 200)
}

func TestIntegrationQuestPolicyAutoAcceptAndServerRoutes(t *testing.T) {
	f := newFixture(t)
	clock := f.withClock()
	clock.set(base)
	expect(t, f.publish(questsConfig(base.AddDate(0, 1, 0), base.AddDate(0, 2, 0))), 201)

	// Auto-accept counts from publication; claim policy server blocks the player.
	auto := f.quests()["server_only"]
	if auto["status"] != "active" || auto["auto_accepted"] != true {
		t.Fatalf("auto quest: %v", auto)
	}
	f.trolls(1)
	wantCode(t, f.quest("server_only", "claim", "p1"), 403, "CLAIM_NOT_ALLOWED")
	serverClaim := f.server(f.serverPath(f.playerID, "/quests/server_only/claim"), "s1", nil)
	if expect(t, serverClaim, 200).obj(t)["claimed_by"] != "server" {
		t.Fatalf("claim: %s", serverClaim.raw)
	}

	// The server can read the log and accept on the player's behalf.
	expect(t, f.server(f.serverPath(f.playerID, "/quests/chain_one/accept"), "s-accept", nil), 200)
	var log []map[string]any
	expect(t, call(t, f.h, "GET", f.serverPath(f.playerID, "/quests"), nil, map[string]string{"X-Server-Credential": f.credential}), 200).into(t, &log)
	if len(log) != 6 {
		t.Fatalf("server log has %d quests", len(log))
	}
	wantStatus(t, f.quests()["chain_one"], "active")

	// Another tenant's credential cannot reach this player's quests.
	rival := &fixture{t: t, db: f.db, h: f.h}
	clock.set(base) // the rival's tokens are issued against the test clock too
	rival.setupTenant("rival")
	expect(t, call(t, f.h, "GET", f.serverPath(f.playerID, "/quests"), nil, map[string]string{"X-Server-Credential": rival.credential}), 404)
	expect(t, call(t, f.h, "POST", f.serverPath(f.playerID, "/quests/chain_one/claim"), nil, map[string]string{"X-Server-Credential": rival.credential, "Idempotency-Key": "x"}), 404)
	expect(t, call(t, f.h, "GET", f.envPath("/quests/stats"), nil, bearer(rival.userToken)), 404)

	stats := map[string]map[string]any{}
	var rows []map[string]any
	expect(t, call(t, f.h, "GET", f.envPath("/quests/stats"), nil, bearer(f.userToken)), 200).into(t, &rows)
	for _, r := range rows {
		stats[r["key"].(string)] = r
	}
	if stats["chain_one"]["accepted"].(float64) != 1 || stats["server_only"]["completions_claimed"].(float64) != 1 {
		t.Fatalf("stats=%v", rows)
	}
	if strings.Contains(fmt.Sprint(rows), f.playerID) {
		t.Fatal("stats leak player identifiers")
	}

	wantCode(t, f.quest("nope", "accept", "n1"), 404, "NOT_FOUND")
}

func TestIntegrationConcurrentQuestClaimsPayOnce(t *testing.T) {
	f := newFixture(t)
	clock := f.withClock()
	clock.set(base)
	expect(t, f.publish(questsConfig(base.AddDate(0, 1, 0), base.AddDate(0, 2, 0))), 201)
	expect(t, f.quest("chain_one", "accept", "a1"), 200)
	f.setFlag("talked_to_elder")
	var wg sync.WaitGroup
	statuses := make([]int, 10)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = f.quest("chain_one", "claim", fmt.Sprintf("c-%d", i)).Status
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, s := range statuses {
		switch s {
		case 200:
			ok++
		case 409:
		default:
			t.Fatalf("unexpected status %d", s)
		}
	}
	if ok != 1 || f.balance("credits") != 10 {
		t.Fatalf("%d claims succeeded, credits=%d; want exactly one payment", ok, f.balance("credits"))
	}
}

func TestIntegrationQuestValidationAndRepublish(t *testing.T) {
	f := newFixture(t)
	clock := f.withClock()
	clock.set(base)
	cfg := func() map[string]any { return questsConfig(base.AddDate(0, 1, 0), base.AddDate(0, 2, 0)) }

	bad := cfg()
	quests := bad["quests"].([]map[string]any)
	quests[0]["requires_quests"] = []string{"chain_three"}
	quests[3]["stages"] = []map[string]any{}
	r := expect(t, f.publish(bad), 422)
	for _, field := range []string{"quests[1].requires_quests[0]", "quests[3].stages"} {
		if !strings.Contains(r.raw, `"`+field+`"`) {
			t.Errorf("missing field error %s in %s", field, r.raw)
		}
	}
	if !strings.Contains(r.raw, "prerequisite cycle") {
		t.Errorf("cycle not explained: %s", r.raw)
	}

	// The draft keeps editor_layout; publishing ignores it.
	good := cfg()
	good["editor_layout"] = map[string]any{"quests": map[string]any{"chain_one": map[string]any{"x": 10, "y": 20}}}
	expect(t, f.publish(good), 201)
	draft := expect(t, call(t, f.h, "GET", f.envPath("/configurations/draft"), nil, bearer(f.userToken)), 200).obj(t)
	if _, ok := draft["editor_layout"]; !ok {
		t.Fatal("editor_layout was dropped from the draft")
	}

	// Single quest edits are shape-checked; references wait for validate/publish.
	path := f.envPath("/quests")
	selfRef := map[string]any{"key": "loop", "name": "Loop", "requires_quests": []string{"loop"}, "stages": []map[string]any{{"key": "s", "name": "S", "conditions": []map[string]any{{"type": "flag", "flag_key": "x"}}}}}
	if r := expect(t, call(t, f.h, "POST", path, selfRef, bearer(f.userToken)), 422); !strings.Contains(r.raw, "quest.requires_quests[0]") {
		t.Fatalf("self prerequisite: %s", r.raw)
	}
	later := map[string]any{"key": "later", "name": "Later", "requires_quests": []string{"not_yet"}, "stages": []map[string]any{{"key": "s", "name": "S", "conditions": []map[string]any{{"type": "flag", "flag_key": "x"}}}}}
	expect(t, call(t, f.h, "POST", path, later, bearer(f.userToken)), 201)
	v := expect(t, call(t, f.h, "POST", f.envPath("/configurations/validate"), nil, bearer(f.userToken)), 200).obj(t)
	if v["valid"] != false || !strings.Contains(fmt.Sprint(v["errors"]), "unknown quest") {
		t.Fatalf("validate: %v", v)
	}
	expect(t, call(t, f.h, "DELETE", path+"/later", nil, bearer(f.userToken)), 200)

	// Once a player has accepted a quest, its stages and repeat mode are fixed.
	expect(t, f.quest("chain_one", "accept", "a1"), 200)
	reordered := cfg()
	reordered["quests"].([]map[string]any)[0]["stages"] = []map[string]any{{"key": "renamed", "name": "Renamed", "conditions": []map[string]any{{"type": "flag", "flag_key": "talked_to_elder"}}}}
	reordered["quests"].([]map[string]any)[0]["repeat"] = "daily"
	r = expect(t, f.publish(reordered), 422)
	for _, field := range []string{"quests[0].stages", "quests[0].repeat"} {
		if !strings.Contains(r.raw, `"`+field+`"`) {
			t.Errorf("missing field error %s in %s", field, r.raw)
		}
	}
	appended := cfg()
	q0 := appended["quests"].([]map[string]any)[0]
	q0["stages"] = append(q0["stages"].([]map[string]any), map[string]any{"key": "extra", "name": "Extra", "conditions": []map[string]any{{"type": "flag", "flag_key": "entered_mine"}}})
	expect(t, f.publish(appended), 201)

	// Removing a quest deactivates it; its records stay.
	f.setFlag("talked_to_elder")
	f.setFlag("entered_mine")
	expect(t, f.quest("chain_one", "claim", "c1"), 200)
	trimmed := cfg()
	trimmed["quests"] = []map[string]any{}
	expect(t, f.publish(trimmed), 201)
	if q := f.quests(); len(q) != 0 {
		t.Fatalf("removed quests still listed: %v", q)
	}
	if n := f.count(`SELECT count(*) FROM quest_claims WHERE player_id=$1`, f.playerID); n != 1 {
		t.Fatalf("claims=%d, removing a quest must not revoke them", n)
	}
	if n := f.count(`SELECT count(*) FROM quest_definitions WHERE key='chain_one' AND NOT active`); n != 1 {
		t.Fatal("removed quest should be deactivated, not deleted")
	}
}
