package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Cloud saves: create, replace with optimistic versions, limits, delete, and the same
// slots seen by the player, the game server and the dashboard, within one tenant only.
func TestIntegrationCloudSaves(t *testing.T) {
	f := newFixture(t)
	me := "/v1/players/me/saves"
	put := func(path, key string, body any) apiResult {
		return call(t, f.h, "PUT", path, body, withKey(bearer(f.player), key))
	}
	get := func(path string) apiResult { return call(t, f.h, "GET", path, nil, bearer(f.player)) }

	// Empty, with the limits in meta.
	r := expect(t, get(me), 200)
	var listMeta struct {
		Data []map[string]any `json:"data"`
		Meta struct {
			MaxSlots int `json:"max_slots"`
			MaxBytes int `json:"max_bytes"`
		} `json:"meta"`
	}
	_ = json.Unmarshal([]byte(r.raw), &listMeta)
	if len(listMeta.Data) != 0 || listMeta.Meta.MaxSlots != 10 || listMeta.Meta.MaxBytes != 256<<10 {
		t.Fatalf("empty list: %s", r.raw)
	}

	// Create: 201, version 1, size of the compacted JSON. A replay returns the same result.
	save := map[string]any{"level": 3, "gold": 120}
	compact, _ := json.Marshal(save)
	created := expect(t, put(me+"/slot1", "p1", map[string]any{"data": save}), 201).obj(t)
	if created["version"].(float64) != 1 || int(created["size_bytes"].(float64)) != len(compact) {
		t.Fatalf("created: %v (want size %d)", created, len(compact))
	}
	expect(t, put(me+"/slot1", "p1", map[string]any{"data": save}), 201)
	if n := f.count(`SELECT version::int FROM player_saves WHERE player_id=$1 AND slot='slot1'`, f.playerID); n != 1 {
		t.Fatalf("replay wrote again: version %d", n)
	}

	// Optimistic concurrency: the right version writes, a stale one conflicts and reports
	// the current version, no version means last write wins.
	if v := expect(t, put(me+"/slot1", "p2", map[string]any{"data": map[string]any{"level": 4}, "expected_version": 1}), 200).obj(t)["version"]; v.(float64) != 2 {
		t.Fatalf("update version: %v", v)
	}
	stale := put(me+"/slot1", "p3", map[string]any{"data": map[string]any{"level": 99}, "expected_version": 1})
	wantCode(t, stale, 409, "SAVE_VERSION_CONFLICT")
	if d := errorDetails(t, stale); d["current_version"].(float64) != 2 {
		t.Fatalf("conflict details: %v", d)
	}
	wantCode(t, put(me+"/slot1", "p4", map[string]any{"data": map[string]any{"level": 99}, "expected_version": 0}), 409, "SAVE_VERSION_CONFLICT")
	expect(t, put(me+"/slot1", "p5", map[string]any{"data": map[string]any{"level": 5, "inventory": []string{"sword"}}}), 200)
	one := expect(t, get(me+"/slot1"), 200).obj(t)
	if one["version"].(float64) != 3 || one["data"].(map[string]any)["level"].(float64) != 5 {
		t.Fatalf("read back: %v", one)
	}

	// Validation and missing slots.
	wantCode(t, put(me+"/Bad%20Slot", "v1", map[string]any{"data": 1}), 422, "VALIDATION_ERROR")
	wantCode(t, put(me+"/slot2", "v2", map[string]any{"data": nil}), 422, "VALIDATION_ERROR")
	wantCode(t, put(me+"/slot2", "v3", map[string]any{}), 422, "VALIDATION_ERROR")
	wantCode(t, put(me+"/slot2", "v4", map[string]any{"data": 1, "expected_version": -1}), 422, "VALIDATION_ERROR")
	wantCode(t, put(me+"/slot2", "v5", map[string]any{"data": map[string]any{"blob": strings.Repeat("a", 256<<10)}}), 422, "SAVE_TOO_LARGE")
	expect(t, call(t, f.h, "PUT", me+"/slot2", map[string]any{"data": 1}, bearer(f.player)), 400) // no Idempotency-Key
	expect(t, get(me+"/missing"), 404)
	expect(t, get(me+"/Bad%20Slot"), 404)
	if n := f.count(`SELECT count(*) FROM player_saves WHERE player_id=$1`, f.playerID); n != 1 {
		t.Fatalf("refused writes stored a slot: %d", n)
	}

	// Slot limit: ten slots, then 422; replacing an existing slot still works.
	for i := 2; i <= 10; i++ {
		expect(t, put(fmt.Sprintf("%s/slot%d", me, i), fmt.Sprintf("fill-%d", i), map[string]any{"data": i}), 201)
	}
	wantCode(t, put(me+"/slot11", "over", map[string]any{"data": 11}), 422, "SAVE_SLOT_LIMIT")
	expect(t, put(me+"/slot10", "again", map[string]any{"data": "ten"}), 200)

	// Delete: a stale expected_version conflicts; then the slot is gone.
	del := func(path, key string) apiResult {
		return call(t, f.h, "DELETE", path, nil, withKey(bearer(f.player), key))
	}
	wantCode(t, del(me+"/slot1?expected_version=1", "d1"), 409, "SAVE_VERSION_CONFLICT")
	gone := expect(t, del(me+"/slot1?expected_version=3", "d2"), 200).obj(t)
	if gone["deleted"] != true || gone["version"].(float64) != 3 {
		t.Fatalf("delete: %v", gone)
	}
	expect(t, del(me+"/slot1?expected_version=3", "d2"), 200) // replay
	expect(t, get(me+"/slot1"), 404)
	expect(t, del(me+"/slot1", "d3"), 404)
	expect(t, put(me+"/slot11", "now-fits", map[string]any{"data": 11}), 201) // the freed slot can be used

	// The game server and the dashboard see and change the same slots.
	server := map[string]string{"X-Server-Credential": f.credential}
	serverPath := "/v1/server/players/" + f.playerID + "/saves"
	adminPath := f.envPath("/players/" + f.playerID + "/saves")
	mine := expect(t, get(me+"/slot2"), 200)
	if s := expect(t, call(t, f.h, "GET", serverPath+"/slot2", nil, server), 200); string(s.Data) != string(mine.Data) {
		t.Fatalf("server view differs:\n%s\n%s", s.Data, mine.Data)
	}
	if a := expect(t, call(t, f.h, "GET", adminPath+"/slot2", nil, bearer(f.userToken)), 200); string(a.Data) != string(mine.Data) {
		t.Fatalf("admin view differs:\n%s\n%s", a.Data, mine.Data)
	}
	expect(t, call(t, f.h, "PUT", serverPath+"/slot2", map[string]any{"data": "from server", "expected_version": 1}, withKey(server, "s1")), 200)
	if v := expect(t, get(me+"/slot2"), 200).obj(t); v["data"] != "from server" || v["version"].(float64) != 2 {
		t.Fatalf("player does not see the server's write: %v", v)
	}
	expect(t, call(t, f.h, "DELETE", adminPath+"/slot3", nil, withKey(bearer(f.userToken), "a1")), 200)
	var slots []map[string]any
	expect(t, call(t, f.h, "GET", adminPath, nil, bearer(f.userToken)), 200).into(t, &slots)
	if len(slots) != 9 {
		t.Fatalf("admin list: %d slots", len(slots))
	}
	// The same key in the player's and the server's namespace is not a replay.
	expect(t, call(t, f.h, "PUT", serverPath+"/slot4", map[string]any{"data": "s"}, withKey(server, "p1")), 200)

	// Isolation: another tenant's server, admin and player see none of it.
	rival := &fixture{t: t, db: f.db, h: f.h}
	rival.setupTenant("rival")
	expect(t, call(t, f.h, "GET", serverPath, nil, map[string]string{"X-Server-Credential": rival.credential}), 404)
	expect(t, call(t, f.h, "PUT", serverPath+"/slot2", map[string]any{"data": "pwned"}, withKey(map[string]string{"X-Server-Credential": rival.credential}, "x")), 404)
	expect(t, call(t, f.h, "GET", adminPath, nil, bearer(rival.userToken)), 404)
	var rivalSlots []map[string]any
	expect(t, call(t, f.h, "GET", me, nil, bearer(rival.player)), 200).into(t, &rivalSlots)
	if len(rivalSlots) != 0 {
		t.Fatalf("rival player sees saves: %v", rivalSlots)
	}
	if v := expect(t, get(me+"/slot2"), 200).obj(t); v["data"] != "from server" {
		t.Fatalf("a refused request changed the save: %v", v)
	}
}
