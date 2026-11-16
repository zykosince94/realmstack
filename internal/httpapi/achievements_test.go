package httpapi

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// phase5Config extends the base fixture config with stats, flags and achievements.
func phase5Config() map[string]any {
	cfg := testConfig()
	cfg["stats"] = []map[string]any{
		{"key": "trolls_defeated", "name": "Trolls defeated", "kind": "counter"},
		{"key": "talked_to_elder", "name": "Talked to the elder", "kind": "flag"},
	}
	cfg["achievements"] = []map[string]any{
		{"key": "troll_slayer", "name": "Troll Slayer", "description": "Defeat 3 trolls and meet the elder.",
			"conditions": []map[string]any{{"type": "stat", "stat_key": "trolls_defeated", "target": 3}, {"type": "flag", "flag_key": "talked_to_elder"}},
			"rewards":    map[string]any{"currencies": []map[string]any{{"currency_key": "gems", "amount": 10}}, "xp": 50, "items": []map[string]any{{"item_key": "health_potion", "quantity": 2}}}},
		// Would complete from the troll_slayer rewards if claim rewards counted.
		{"key": "gem_collector", "name": "Gem Collector", "conditions": []map[string]any{{"type": "currency_earned", "currency_key": "gems", "target": 10}}},
		{"key": "first_steps", "name": "First Steps", "conditions": []map[string]any{{"type": "xp_total", "target": 40}}, "rewards": map[string]any{"currencies": []map[string]any{{"currency_key": "credits", "amount": 5}}}},
		{"key": "potion_hoarder", "name": "Potion Hoarder", "conditions": []map[string]any{{"type": "item_granted", "item_key": "health_potion", "target": 1}}},
		{"key": "big_spender", "name": "Big Spender", "conditions": []map[string]any{{"type": "currency_spent", "currency_key": "credits", "target": 50}}},
		{"key": "speed_demon", "name": "Speed Demon", "claimable_by": "server", "conditions": []map[string]any{{"type": "leaderboard_score", "leaderboard_key": "speedrun", "target": 60}}},
		{"key": "secret_room", "name": "Secret Room", "description": "You found it.", "hidden": true, "conditions": []map[string]any{{"type": "level_reached", "target": 2}}},
	}
	return cfg
}

func (f *fixture) publish(cfg map[string]any) apiResult {
	f.t.Helper()
	expect(f.t, call(f.t, f.h, "PUT", f.envPath("/configurations/draft"), cfg, bearer(f.userToken)), 200)
	return call(f.t, f.h, "POST", f.envPath("/configurations/publish"), nil, bearer(f.userToken))
}

func (f *fixture) achievements(token string) map[string]map[string]any {
	f.t.Helper()
	var list []map[string]any
	expect(f.t, call(f.t, f.h, "GET", "/v1/players/me/achievements", nil, bearer(token)), 200).into(f.t, &list)
	out := map[string]map[string]any{}
	for _, a := range list {
		out[a["key"].(string)] = a
	}
	return out
}

func (f *fixture) serverPath(playerID, suffix string) string {
	return "/v1/server/players/" + playerID + suffix
}

// TestIntegrationPhase5ExitCriteria walks the spec's Phase 5 exit criterion: a player
// completes an achievement through existing economy/XP/score APIs and server-reported
// stats/flags, claims it exactly once (replays return the original result), receives
// its rewards atomically, and claim rewards do not count toward other achievements.
func TestIntegrationPhase5ExitCriteria(t *testing.T) {
	f := newFixture(t)
	expect(t, f.publish(phase5Config()), 201)

	// Progress comes from the existing APIs...
	expect(t, f.server(f.serverPath(f.playerID, "/xp/award"), "xp-1", map[string]any{"amount": 120}), 200)
	expect(t, f.credit(f.playerID, "credits", 100), 200)
	expect(t, f.playerPost(f.player, "/v1/players/me/currency/spend", "spend-1", map[string]any{"currency_key": "credits", "amount": 60}), 200)
	// ...and from server-reported game-world facts.
	expect(t, f.server(f.serverPath(f.playerID, "/stats/trolls_defeated/increment"), "trolls-1", map[string]any{"amount": 2}), 200)
	a := f.achievements(f.player)
	if a["troll_slayer"]["status"] != "in_progress" {
		t.Fatalf("troll_slayer completed too early: %v", a["troll_slayer"])
	}
	progress := a["troll_slayer"]["progress"].([]any)[0].(map[string]any)
	if progress["current"].(float64) != 2 || progress["target"].(float64) != 3 {
		t.Fatalf("progress: %v", progress)
	}
	if a["first_steps"]["status"] != "completed" || a["big_spender"]["status"] != "completed" || a["secret_room"]["status"] != "completed" {
		t.Fatalf("xp/spend/level achievements should be complete: %v", a)
	}

	claimPath := "/v1/players/me/achievements/troll_slayer/claim"
	if r := expect(t, f.playerPost(f.player, claimPath, "claim-early", nil), 422); r.Error.Code != "NOT_COMPLETED" {
		t.Fatalf("code=%s", r.Error.Code)
	}
	expect(t, f.server(f.serverPath(f.playerID, "/stats/trolls_defeated/increment"), "trolls-2", map[string]any{"amount": 1}), 200)
	expect(t, f.server(f.serverPath(f.playerID, "/flags/talked_to_elder/set"), "elder-1", nil), 200)

	first := expect(t, f.playerPost(f.player, claimPath, "claim-1", nil), 200).obj(t)
	replay := expect(t, f.playerPost(f.player, claimPath, "claim-1", nil), 200).obj(t)
	if !reflect.DeepEqual(first, replay) {
		t.Fatalf("replay differs: %v vs %v", first, replay)
	}
	if r := expect(t, f.playerPost(f.player, claimPath, "claim-2", nil), 409); r.Error.Code != "ALREADY_CLAIMED" {
		t.Fatalf("code=%s", r.Error.Code)
	}

	// Rewards arrived once, tagged with the achievement source.
	if n := f.count(`SELECT amount::int FROM balances b JOIN currency_definitions d ON d.id=b.currency_id WHERE b.player_id=$1 AND d.currency_key='gems'`, f.playerID); n != 10 {
		t.Fatalf("gems=%d, want 10", n)
	}
	if n := f.count(`SELECT quantity::int FROM inventory WHERE player_id=$1`, f.playerID); n != 2 {
		t.Fatalf("potions=%d, want 2", n)
	}
	if n := f.count(`SELECT total_xp::int FROM player_progression WHERE player_id=$1`, f.playerID); n != 170 {
		t.Fatalf("total xp=%d, want 120+50", n)
	}
	if n := f.count(`SELECT count(*) FROM economy_transactions WHERE player_id=$1 AND source='achievement'`, f.playerID); n != 2 {
		t.Fatalf("achievement-tagged economy rows=%d, want 2", n)
	}
	if n := f.count(`SELECT count(*) FROM achievement_claims WHERE player_id=$1`, f.playerID); n != 1 {
		t.Fatalf("claims=%d, want 1", n)
	}

	// Claim rewards do not count toward other achievements.
	a = f.achievements(f.player)
	if a["troll_slayer"]["status"] != "claimed" {
		t.Fatalf("troll_slayer: %v", a["troll_slayer"])
	}
	for _, key := range []string{"gem_collector", "potion_hoarder"} {
		if a[key]["status"] != "in_progress" {
			t.Fatalf("%s was completed by claim rewards: %v", key, a[key])
		}
	}
	// A real grant through the API does count.
	expect(t, f.grant(f.playerID, "health_potion", 1), 200)
	if a = f.achievements(f.player); a["potion_hoarder"]["status"] != "completed" {
		t.Fatalf("api grant should complete potion_hoarder: %v", a["potion_hoarder"])
	}
}

func TestIntegrationStatsAndFlags(t *testing.T) {
	f := newFixture(t)
	expect(t, f.publish(phase5Config()), 201)
	inc := f.serverPath(f.playerID, "/stats/trolls_defeated/increment")

	// Only a trusted server credential may write; players get 401 (no such route for them).
	expect(t, call(t, f.h, "POST", inc, map[string]any{"amount": 1}, withKey(bearer(f.player), "p1")), 401)
	expect(t, f.server(inc, "bad-amount", map[string]any{"amount": 0}), 422)
	expect(t, f.server(inc, "bad-amount-2", map[string]any{"amount": -3}), 422)
	if r := expect(t, f.server(f.serverPath(f.playerID, "/stats/talked_to_elder/increment"), "wrong-kind", map[string]any{"amount": 1}), 422); r.Error.Code != "WRONG_STAT_KIND" {
		t.Fatalf("code=%s", r.Error.Code)
	}
	expect(t, f.server(f.serverPath(f.playerID, "/flags/trolls_defeated/set"), "wrong-kind-2", nil), 422)
	expect(t, f.server(f.serverPath(f.playerID, "/stats/dragons/increment"), "unknown", map[string]any{"amount": 1}), 404)

	// Repeated keys do not double-count.
	expect(t, f.server(inc, "k1", map[string]any{"amount": 4, "reason": "combat"}), 200)
	expect(t, f.server(inc, "k1", map[string]any{"amount": 4, "reason": "combat"}), 200)
	expect(t, f.server(inc, "k1", map[string]any{"amount": 5}), 409)
	// Flags are set-only; setting twice writes one ledger row.
	set := f.serverPath(f.playerID, "/flags/talked_to_elder/set")
	if r := expect(t, f.server(set, "f1", map[string]any{"reason": "dialogue"}), 200).obj(t); r["newly_set"] != true {
		t.Fatalf("first set: %v", r)
	}
	if r := expect(t, f.server(set, "f2", nil), 200).obj(t); r["newly_set"] != false {
		t.Fatalf("second set: %v", r)
	}
	if n := f.count(`SELECT count(*) FROM stat_transactions WHERE player_id=$1`, f.playerID); n != 2 {
		t.Fatalf("stat ledger rows=%d, want 2 (one increment, one flag)", n)
	}

	// Players read their own; the server reads any player in scope.
	mine := expect(t, call(t, f.h, "GET", "/v1/players/me/stats", nil, bearer(f.player)), 200).obj(t)
	counters, flags := mine["counters"].([]any), mine["flags"].([]any)
	if counters[0].(map[string]any)["value"].(float64) != 4 || flags[0].(map[string]any)["set"] != true {
		t.Fatalf("stats: %v", mine)
	}
	expect(t, call(t, f.h, "GET", f.serverPath(f.playerID, "/stats"), nil, map[string]string{"X-Server-Credential": f.credential}), 200)

	// The stat kind cannot change once values exist.
	cfg := phase5Config()
	cfg["stats"] = []map[string]any{{"key": "trolls_defeated", "name": "Trolls", "kind": "flag"}, {"key": "talked_to_elder", "name": "Elder", "kind": "flag"}}
	cfg["achievements"] = []map[string]any{}
	r := expect(t, f.publish(cfg), 422)
	if !strings.Contains(r.raw, `"stats[0].kind"`) {
		t.Fatalf("kind change should be rejected: %s", r.raw)
	}
}

func TestIntegrationAchievementPoliciesAndMasking(t *testing.T) {
	f := newFixture(t)
	expect(t, f.publish(phase5Config()), 201)
	// Hidden incomplete achievements are masked for players, not for the server.
	a := f.achievements(f.player)
	if a["secret_room"]["name"] != nil || a["secret_room"]["conditions"] != nil {
		t.Fatalf("hidden achievement leaked: %v", a["secret_room"])
	}
	var serverList []map[string]any
	expect(t, call(t, f.h, "GET", f.serverPath(f.playerID, "/achievements"), nil, map[string]string{"X-Server-Credential": f.credential}), 200).into(t, &serverList)
	for _, x := range serverList {
		if x["key"] == "secret_room" && x["name"] != "Secret Room" {
			t.Fatalf("server view masked: %v", x)
		}
	}

	// Claim policy "server": the player cannot claim it even when complete.
	expect(t, f.playerPost(f.player, "/v1/leaderboards/speedrun/scores", "s1", map[string]any{"score": 55}), 201)
	if r := expect(t, f.playerPost(f.player, "/v1/players/me/achievements/speed_demon/claim", "c1", nil), 403); r.Error.Code != "CLAIM_NOT_ALLOWED" {
		t.Fatalf("code=%s", r.Error.Code)
	}
	expect(t, f.server(f.serverPath(f.playerID, "/achievements/speed_demon/claim"), "c1", nil), 200)
	expect(t, f.playerPost(f.player, "/v1/players/me/achievements/unknown/claim", "c2", nil), 404)

	// Claim statistics carry counts only.
	var stats []map[string]any
	expect(t, call(t, f.h, "GET", f.envPath("/achievements/stats"), nil, bearer(f.userToken)), 200).into(t, &stats)
	for _, s := range stats {
		if s["key"] == "speed_demon" && (s["claims"].(float64) != 1 || s["claimed_by_server"].(float64) != 1) {
			t.Fatalf("stats: %v", s)
		}
		if _, leaked := s["player_id"]; leaked {
			t.Fatal("stats leaked a player identifier")
		}
	}
	// Another tenant cannot read them.
	other := &fixture{t: t, db: f.db, h: f.h}
	other.setupTenant("rival5")
	expect(t, call(t, f.h, "GET", f.envPath("/achievements/stats"), nil, bearer(other.userToken)), 404)
	expect(t, call(t, f.h, "POST", f.serverPath(f.playerID, "/achievements/speed_demon/claim"), nil, map[string]string{"X-Server-Credential": other.credential, "Idempotency-Key": "x"}), 404)
}

func TestIntegrationClaimRollsBackOnFailingReward(t *testing.T) {
	f := newFixture(t)
	cfg := phase5Config()
	cfg["achievements"] = []map[string]any{{"key": "sword_bonus", "name": "Sword Bonus",
		"conditions": []map[string]any{{"type": "xp_total", "target": 1}},
		"rewards":    map[string]any{"currencies": []map[string]any{{"currency_key": "gems", "amount": 5}}, "items": []map[string]any{{"item_key": "iron_sword", "quantity": 1}}}}}
	expect(t, f.publish(cfg), 201)
	expect(t, f.server(f.serverPath(f.playerID, "/xp/award"), "xp", map[string]any{"amount": 5}), 200)
	expect(t, f.grant(f.playerID, "iron_sword", 1), 200) // non-stackable: the reward cannot fit
	if r := expect(t, f.playerPost(f.player, "/v1/players/me/achievements/sword_bonus/claim", "c1", nil), 422); r.Error.Code != "STACK_LIMIT" {
		t.Fatalf("code=%s", r.Error.Code)
	}
	if n := f.count(`SELECT count(*) FROM balances WHERE player_id=$1`, f.playerID); n != 0 {
		t.Fatal("currency reward was paid although the claim failed")
	}
	if n := f.count(`SELECT count(*) FROM achievement_claims`); n != 0 {
		t.Fatal("a claim was recorded although its rewards failed")
	}
	// After the player frees the slot, the same claim succeeds.
	expect(t, f.playerPost(f.player, "/v1/players/me/inventory/consume", "free", map[string]any{"item_key": "iron_sword", "quantity": 1}), 200)
	expect(t, f.playerPost(f.player, "/v1/players/me/achievements/sword_bonus/claim", "c1", nil), 200)
}

func TestIntegrationConcurrentClaimsPayOnce(t *testing.T) {
	f := newFixture(t)
	expect(t, f.publish(phase5Config()), 201)
	expect(t, f.server(f.serverPath(f.playerID, "/xp/award"), "xp", map[string]any{"amount": 100}), 200)
	var wg sync.WaitGroup
	statuses := make([]int, 10)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = f.playerPost(f.player, "/v1/players/me/achievements/first_steps/claim", fmt.Sprintf("c-%d", i), nil).Status
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
	if ok != 1 {
		t.Fatalf("%d concurrent claims succeeded, want exactly 1", ok)
	}
	if n := f.count(`SELECT amount::int FROM balances WHERE player_id=$1`, f.playerID); n != 5 {
		t.Fatalf("credits=%d, want 5 (paid once)", n)
	}
}

func TestIntegrationAchievementValidationAndRepublish(t *testing.T) {
	f := newFixture(t)
	bad := phase5Config()
	bad["achievements"] = []map[string]any{
		{"key": "a", "name": "A", "conditions": []map[string]any{{"type": "stat", "stat_key": "talked_to_elder", "target": 1}}},
		{"key": "b", "name": "B", "conditions": []map[string]any{{"type": "level_reached", "target": 9}}, "rewards": map[string]any{"items": []map[string]any{{"item_key": "iron_sword", "quantity": 2}}}},
		{"key": "a", "name": "Dup", "conditions": []map[string]any{{"type": "teleport", "target": 1}}},
	}
	r := expect(t, f.publish(bad), 422)
	for _, field := range []string{"achievements[0].conditions[0].stat_key", "achievements[1].conditions[0].target", "achievements[1].rewards.items[0].quantity", "achievements[2].conditions[0].type", "achievements[2].key"} {
		if !strings.Contains(r.raw, `"`+field+`"`) {
			t.Errorf("missing field error %s in %s", field, r.raw)
		}
	}

	// Definitions edited one at a time are shape-checked; references wait for publish.
	ach := f.envPath("/achievements")
	expect(t, call(t, f.h, "PUT", f.envPath("/configurations/draft"), phase5Config(), bearer(f.userToken)), 200)
	expect(t, call(t, f.h, "POST", ach, map[string]any{"key": "new_one", "name": "New", "conditions": []map[string]any{{"type": "flag", "flag_key": "not_defined_yet"}}}, bearer(f.userToken)), 201)
	expect(t, call(t, f.h, "POST", ach, map[string]any{"key": "broken", "name": "Broken", "conditions": []map[string]any{}}, bearer(f.userToken)), 422)
	validation := expect(t, call(t, f.h, "POST", f.envPath("/configurations/validate"), nil, bearer(f.userToken)), 200).obj(t)
	if validation["valid"] != false || !strings.Contains(fmt.Sprint(validation["errors"]), "flag_key") {
		t.Fatalf("validate should flag the unknown flag: %v", validation)
	}
	expect(t, call(t, f.h, "DELETE", ach+"/new_one", nil, bearer(f.userToken)), 200)
	expect(t, call(t, f.h, "POST", f.envPath("/stats"), map[string]any{"key": "chests", "name": "Chests", "kind": "box"}, bearer(f.userToken)), 422)

	// Claimed rewards survive removing the achievement; it is deactivated, not deleted.
	expect(t, call(t, f.h, "POST", f.envPath("/configurations/publish"), nil, bearer(f.userToken)), 201)
	expect(t, f.server(f.serverPath(f.playerID, "/xp/award"), "xp", map[string]any{"amount": 100}), 200)
	expect(t, f.playerPost(f.player, "/v1/players/me/achievements/first_steps/claim", "c1", nil), 200)
	trimmed := phase5Config()
	trimmed["achievements"] = []map[string]any{}
	expect(t, f.publish(trimmed), 201)
	if a := f.achievements(f.player); len(a) != 0 {
		t.Fatalf("removed achievements still listed: %v", a)
	}
	if n := f.count(`SELECT count(*) FROM achievement_claims WHERE player_id=$1`, f.playerID); n != 1 {
		t.Fatal("removing an achievement revoked its claim")
	}
	if n := f.count(`SELECT count(*) FROM achievement_definitions WHERE key='first_steps' AND NOT active`); n != 1 {
		t.Fatal("removed achievement should be deactivated, not deleted")
	}
}
