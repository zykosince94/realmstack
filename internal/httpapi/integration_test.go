package httpapi

// PostgreSQL integration tests. They exercise the real HTTP handlers against a real
// database and are skipped unless TEST_DATABASE_URL is set. Run them with:
//
//	docker compose -f docker-compose.test.yml run --rm test
//
// Each test gets its own PostgreSQL schema, so tests are isolated and leave nothing behind.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"realmstack-backend/internal/demo"
	"realmstack-backend/internal/persistence"
	"realmstack-backend/internal/platform"
)

var extensionOnce sync.Once

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	extensionOnce.Do(func() {
		if _, err := admin.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public`); err != nil {
			t.Fatalf("create extension: %v", err)
		}
	})
	schema := "test_" + platform.RandomID()[:16]
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	cfg.MaxConns = 30
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	if err = persistence.MigrateDir(ctx, pool, "../../migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

type apiResult struct {
	Status int
	Header http.Header
	Data   json.RawMessage
	Error  struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	raw string
}

func (r apiResult) into(t *testing.T, dst any) {
	t.Helper()
	if err := json.Unmarshal(r.Data, dst); err != nil {
		t.Fatalf("decode %s: %v", r.raw, err)
	}
}

func (r apiResult) obj(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	r.into(t, &m)
	return m
}

func call(t *testing.T, h http.Handler, method, path string, body any, headers map[string]string) apiResult {
	t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	default:
		payload, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(payload)
	}
	req := httptest.NewRequest(method, path, reader)
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error json.RawMessage `json:"error"`
	}
	res := apiResult{Status: rec.Code, Header: rec.Header(), raw: rec.Body.String()}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("%s %s returned non-JSON (%d): %s", method, path, rec.Code, rec.Body.String())
	}
	res.Data = envelope.Data
	if len(envelope.Error) > 0 {
		_ = json.Unmarshal(envelope.Error, &res.Error)
	}
	return res
}

func expect(t *testing.T, r apiResult, status int) apiResult {
	t.Helper()
	if r.Status != status {
		t.Fatalf("status=%d want %d: %s", r.Status, status, r.raw)
	}
	return r
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func withKey(h map[string]string, key string) map[string]string {
	out := map[string]string{"Idempotency-Key": key}
	for k, v := range h {
		out[k] = v
	}
	return out
}

// testConfig is the published configuration every fixture starts with.
func testConfig() map[string]any {
	return map[string]any{
		"items": []map[string]any{
			{"key": "health_potion", "name": "Health Potion", "category": "consumable", "stackable": true, "max_stack": 99, "metadata": map[string]any{"healing": 50}},
			{"key": "iron_sword", "name": "Iron Sword", "category": "weapon", "stackable": false, "max_stack": 1, "metadata": map[string]any{"slot": "weapon"}},
			{"key": "wooden_shield", "name": "Wooden Shield", "category": "armor", "stackable": false, "max_stack": 1, "metadata": map[string]any{"slot": "offhand"}},
		},
		"currencies":   []map[string]any{{"key": "credits", "name": "Credits"}, {"key": "gems", "name": "Gems"}},
		"progression":  []int{100, 300, 700},
		"leaderboards": []map[string]any{{"key": "global", "direction": "higher"}, {"key": "speedrun", "direction": "lower"}},
	}
}

type fixture struct {
	t          *testing.T
	db         *pgxpool.Pool
	h          http.Handler
	userToken  string
	projectID  string
	envID      string
	publicID   string
	credential string
	playerID   string
	player     string // player access token
	refresh    string
}

func newServer(t *testing.T, db *pgxpool.Pool) *Server {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return New(db, Config{JWTSecret: strings.Repeat("k", 40), AccessTTL: time.Minute, RefreshTTL: time.Hour, BodyLimit: 1 << 20, AuthRateLimit: 10000}, logger)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db := testDB(t)
	f := &fixture{t: t, db: db, h: newServer(t, db).Handler()}
	f.setupTenant("owner")
	return f
}

// setupTenant creates a dashboard user, project, published config, credential and player.
func (f *fixture) setupTenant(name string) {
	t := f.t
	t.Helper()
	user := expect(t, call(t, f.h, "POST", "/v1/auth/register", map[string]any{"email": name + "@example.test", "password": "DashboardPass123!"}, nil), 201).obj(t)
	f.userToken = user["access_token"].(string)
	project := expect(t, call(t, f.h, "POST", "/v1/projects", map[string]any{"name": name + " game", "slug": name + "-game"}, bearer(f.userToken)), 201).obj(t)
	f.projectID = project["id"].(string)
	env := project["environments"].([]any)[0].(map[string]any)
	f.envID, f.publicID = env["id"].(string), env["public_id"].(string)
	expect(t, call(t, f.h, "PUT", f.envPath("/configurations/draft"), testConfig(), bearer(f.userToken)), 200)
	expect(t, call(t, f.h, "POST", f.envPath("/configurations/publish"), nil, bearer(f.userToken)), 201)
	cred := expect(t, call(t, f.h, "POST", f.envPath("/credentials"), nil, bearer(f.userToken)), 201).obj(t)
	f.credential = cred["credential"].(string)
	f.playerID, f.player, f.refresh = f.registerPlayer(name+"-player@example.test", "Hero "+name)
}

func (f *fixture) envPath(suffix string) string {
	return "/v1/projects/" + f.projectID + "/environments/" + f.envID + suffix
}

func (f *fixture) registerPlayer(email, displayName string) (id, access, refresh string) {
	t := f.t
	t.Helper()
	body := map[string]any{"public_environment_id": f.publicID, "email": email, "password": "PlayerPass123!"}
	if displayName != "" {
		body["display_name"] = displayName
	}
	res := expect(t, call(t, f.h, "POST", "/v1/players/register", body, nil), 201).obj(t)
	return res["player"].(map[string]any)["id"].(string), res["access_token"].(string), res["refresh_token"].(string)
}

func (f *fixture) server(path, key string, body any) apiResult {
	return call(f.t, f.h, "POST", path, body, map[string]string{"X-Server-Credential": f.credential, "Idempotency-Key": key})
}

func (f *fixture) playerPost(token, path, key string, body any) apiResult {
	return call(f.t, f.h, "POST", path, body, withKey(bearer(token), key))
}

func (f *fixture) grant(playerID, item string, qty int) apiResult {
	return f.server("/v1/server/players/"+playerID+"/items/grant", "grant-"+platform.RandomID(), map[string]any{"item_key": item, "quantity": qty})
}

func (f *fixture) credit(playerID, currency string, amount int) apiResult {
	return f.server("/v1/server/players/"+playerID+"/currency/credit", "credit-"+platform.RandomID(), map[string]any{"currency_key": currency, "amount": amount})
}

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		f.t.Fatalf("count: %v", err)
	}
	return n
}

func TestIntegrationTenantIsolation(t *testing.T) {
	a := newFixture(t)
	b := &fixture{t: t, db: a.db, h: a.h}
	b.setupTenant("rival")

	// Another dashboard user cannot see or change the project.
	for _, path := range []string{"/v1/projects/" + a.projectID, a.envPath("/configurations/draft"), a.envPath("/configurations/published"), a.envPath("/credentials")} {
		expect(t, call(t, a.h, "GET", path, nil, bearer(b.userToken)), 404)
	}
	expect(t, call(t, a.h, "PATCH", "/v1/projects/"+a.projectID, map[string]any{"name": "pwned"}, bearer(b.userToken)), 404)
	expect(t, call(t, a.h, "DELETE", "/v1/projects/"+a.projectID, nil, bearer(b.userToken)), 404)
	var creds []map[string]any
	expect(t, call(t, a.h, "GET", a.envPath("/credentials"), nil, bearer(a.userToken)), 200).into(t, &creds)
	expect(t, call(t, a.h, "POST", "/v1/credentials/"+creds[0]["id"].(string)+"/revoke", nil, bearer(b.userToken)), 404)

	// Project B's server credential cannot touch project A's player.
	res := call(t, a.h, "POST", "/v1/server/players/"+a.playerID+"/items/grant", map[string]any{"item_key": "health_potion", "quantity": 1},
		map[string]string{"X-Server-Credential": b.credential, "Idempotency-Key": "cross-tenant"})
	expect(t, res, 404)
	expect(t, call(t, a.h, "POST", "/v1/server/players/"+a.playerID+"/xp/award", map[string]any{"amount": 10},
		map[string]string{"X-Server-Credential": b.credential, "Idempotency-Key": "cross-xp"}), 404)

	// A player cannot log in to another project with valid credentials.
	expect(t, call(t, a.h, "POST", "/v1/players/login", map[string]any{"public_environment_id": b.publicID, "email": "owner-player@example.test", "password": "PlayerPass123!"}, nil), 401)

	// A revoked credential stops working.
	expect(t, call(t, a.h, "POST", "/v1/credentials/"+creds[0]["id"].(string)+"/revoke", nil, bearer(a.userToken)), 200)
	expect(t, a.grant(a.playerID, "health_potion", 1), 401)
}

func TestIntegrationPlayerAuthRefreshAndPrivacy(t *testing.T) {
	f := newFixture(t)
	login := func(email, password string) apiResult {
		return call(t, f.h, "POST", "/v1/players/login", map[string]any{"public_environment_id": f.publicID, "email": email, "password": password}, nil)
	}
	expect(t, login("owner-player@example.test", "wrong-password"), 401)
	expect(t, login("nobody@example.test", "PlayerPass123!"), 401)
	expect(t, login("owner-player@example.test", "PlayerPass123!"), 200)
	expect(t, call(t, f.h, "POST", "/v1/players/register", map[string]any{"public_environment_id": f.publicID, "email": "owner-player@example.test", "password": "PlayerPass123!"}, nil), 409)

	// Refresh rotates; reusing the old token revokes the whole family.
	rotated := expect(t, call(t, f.h, "POST", "/v1/players/refresh", map[string]any{"refresh_token": f.refresh}, nil), 200).obj(t)
	next := rotated["refresh_token"].(string)
	if next == f.refresh {
		t.Fatal("refresh token was not rotated")
	}
	reuse := expect(t, call(t, f.h, "POST", "/v1/players/refresh", map[string]any{"refresh_token": f.refresh}, nil), 401)
	if reuse.Error.Code != "REFRESH_TOKEN_REUSE" {
		t.Fatalf("code=%s", reuse.Error.Code)
	}
	expect(t, call(t, f.h, "POST", "/v1/players/refresh", map[string]any{"refresh_token": next}, nil), 401)

	// Players registered without a display name never expose their email publicly.
	_, token, _ := f.registerPlayer("jane.smith@corp.example", "")
	me := expect(t, call(t, f.h, "GET", "/v1/players/me", nil, bearer(token)), 200).obj(t)
	name := me["display_name"].(string)
	if !strings.HasPrefix(name, "Player-") || strings.Contains(name, "jane") {
		t.Fatalf("default display name leaks email: %q", name)
	}

	// Player tokens are not dashboard tokens and vice versa.
	expect(t, call(t, f.h, "GET", "/v1/projects", nil, bearer(f.player)), 401)
	expect(t, call(t, f.h, "GET", "/v1/players/me", nil, bearer(f.userToken)), 401)
}

func TestIntegrationIdempotentGrantAndReplay(t *testing.T) {
	f := newFixture(t)
	path := "/v1/server/players/" + f.playerID + "/items/grant"
	body := map[string]any{"item_key": "health_potion", "quantity": 3, "reason": "match_reward", "reference": "m1"}
	first := expect(t, f.server(path, "reward-1", body), 200)
	second := expect(t, f.server(path, "reward-1", body), 200)
	// Stored results are jsonb, so key order may differ; compare values.
	if !reflect.DeepEqual(first.obj(t), second.obj(t)) {
		t.Fatalf("replay changed the result: %s vs %s", first.Data, second.Data)
	}
	if n := f.count(`SELECT quantity FROM inventory WHERE player_id=$1`, f.playerID); n != 3 {
		t.Fatalf("quantity=%d, want 3 (replay double-applied)", n)
	}
	if n := f.count(`SELECT count(*) FROM economy_transactions WHERE player_id=$1`, f.playerID); n != 1 {
		t.Fatalf("ledger rows=%d, want 1", n)
	}
	body["quantity"] = 4
	conflict := expect(t, f.server(path, "reward-1", body), 409)
	if conflict.Error.Code != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("code=%s", conflict.Error.Code)
	}
	// Missing key is rejected; non-stackable items respect max_stack; unknown items are 404.
	expect(t, call(t, f.h, "POST", path, body, map[string]string{"X-Server-Credential": f.credential}), 400)
	expect(t, f.grant(f.playerID, "iron_sword", 1), 200)
	if r := expect(t, f.grant(f.playerID, "iron_sword", 1), 422); r.Error.Code != "STACK_LIMIT" {
		t.Fatalf("code=%s", r.Error.Code)
	}
	expect(t, f.grant(f.playerID, "unknown_item", 1), 404)

	// Replayed score submissions keep their original 201 status.
	scorePath := "/v1/leaderboards/global/scores"
	expect(t, f.playerPost(f.player, scorePath, "score-1", map[string]any{"score": 10}), 201)
	expect(t, f.playerPost(f.player, scorePath, "score-1", map[string]any{"score": 10}), 201)
}

func TestIntegrationSpendAndConcurrency(t *testing.T) {
	f := newFixture(t)
	expect(t, f.credit(f.playerID, "credits", 100), 200)
	spendPath := "/v1/players/me/currency/spend"
	if r := expect(t, f.playerPost(f.player, spendPath, "too-much", map[string]any{"currency_key": "credits", "amount": 150}), 422); r.Error.Code != "INSUFFICIENT_FUNDS" {
		t.Fatalf("code=%s", r.Error.Code)
	}
	expect(t, f.playerPost(f.player, spendPath, "zero", map[string]any{"currency_key": "credits", "amount": 0}), 422)

	// 20 concurrent spends of 10 against a balance of 100: exactly 10 may succeed.
	var wg sync.WaitGroup
	statuses := make(chan int, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := call(t, f.h, "POST", spendPath, map[string]any{"currency_key": "credits", "amount": 10}, withKey(bearer(f.player), fmt.Sprintf("spend-%d", i)))
			statuses <- r.Status
		}(i)
	}
	wg.Wait()
	close(statuses)
	ok, rejected := 0, 0
	for s := range statuses {
		switch s {
		case 200:
			ok++
		case 422:
			rejected++
		default:
			t.Fatalf("unexpected status %d", s)
		}
	}
	if ok != 10 || rejected != 10 {
		t.Fatalf("ok=%d rejected=%d, want 10/10", ok, rejected)
	}
	if n := f.count(`SELECT amount FROM balances WHERE player_id=$1`, f.playerID); n != 0 {
		t.Fatalf("final balance=%d, want 0", n)
	}
	// The ledger reconciles with the balance.
	if n := f.count(`SELECT COALESCE(SUM(CASE WHEN operation='credit' THEN quantity ELSE -quantity END),0)::int FROM economy_transactions WHERE player_id=$1 AND currency_id IS NOT NULL`, f.playerID); n != 0 {
		t.Fatalf("ledger sum=%d, want 0", n)
	}
}

func TestIntegrationEquipRules(t *testing.T) {
	f := newFixture(t)
	for _, item := range []string{"health_potion", "iron_sword", "wooden_shield"} {
		expect(t, f.grant(f.playerID, item, 1), 200)
	}
	equip := func(key, item, slot string) apiResult {
		return f.playerPost(f.player, "/v1/players/me/inventory/equip", key, map[string]any{"item_key": item, "slot": slot})
	}
	if r := expect(t, equip("e1", "health_potion", "weapon"), 422); r.Error.Code != "ITEM_NOT_EQUIPPABLE" {
		t.Fatalf("code=%s", r.Error.Code)
	}
	if r := expect(t, equip("e2", "iron_sword", "offhand"), 422); r.Error.Code != "INVALID_SLOT" {
		t.Fatalf("code=%s", r.Error.Code)
	}
	expect(t, equip("e3", "iron_sword", "weapon"), 200)
	expect(t, equip("e4", "wooden_shield", "offhand"), 200)
	if r := expect(t, equip("e5", "unknown", "weapon"), 422); r.Error.Code != "ITEM_NOT_OWNED" {
		t.Fatalf("code=%s", r.Error.Code)
	}

	var inventory []map[string]any
	expect(t, call(t, f.h, "GET", "/v1/players/me/inventory", nil, bearer(f.player)), 200).into(t, &inventory)
	slots := map[string]string{}
	for _, row := range inventory {
		slots[row["item_key"].(string)] = row["equipped_slot"].(string)
	}
	if slots["iron_sword"] != "weapon" || slots["wooden_shield"] != "offhand" || slots["health_potion"] != "" {
		t.Fatalf("unexpected slots: %v", slots)
	}

	// Consuming the last unit of an equipped item unequips it.
	consumed := expect(t, f.playerPost(f.player, "/v1/players/me/inventory/consume", "c1", map[string]any{"item_key": "iron_sword", "quantity": 1}), 200).obj(t)
	if consumed["unequipped"] != true || consumed["remaining"].(float64) != 0 {
		t.Fatalf("consume result: %v", consumed)
	}
	if n := f.count(`SELECT count(*) FROM equipment WHERE player_id=$1 AND slot_key='weapon'`, f.playerID); n != 0 {
		t.Fatalf("consumed item is still equipped")
	}
	expect(t, f.playerPost(f.player, "/v1/players/me/inventory/consume", "c2", map[string]any{"item_key": "iron_sword", "quantity": 1}), 422)

	// Unequip removes the slot; unequipping an empty slot is 404.
	expect(t, f.playerPost(f.player, "/v1/players/me/inventory/unequip", "u1", map[string]any{"slot": "offhand"}), 200)
	expect(t, f.playerPost(f.player, "/v1/players/me/inventory/unequip", "u2", map[string]any{"slot": "offhand"}), 404)
}

func TestIntegrationXPAndPinnedProgression(t *testing.T) {
	f := newFixture(t)
	award := func(playerID, key string, amount int) apiResult {
		return f.server("/v1/server/players/"+playerID+"/xp/award", key, map[string]any{"amount": amount, "reason": "quest"})
	}
	progression := func(token string) map[string]any {
		return expect(t, call(t, f.h, "GET", "/v1/players/me/progression", nil, bearer(token)), 200).obj(t)
	}
	if p := progression(f.player); p["level"].(float64) != 1 || p["total_xp"].(float64) != 0 {
		t.Fatalf("fresh progression: %v", p)
	}
	expect(t, award(f.playerID, "xp-1", 150), 200)
	expect(t, award(f.playerID, "xp-1", 150), 200) // replay
	if p := progression(f.player); p["level"].(float64) != 2 || p["total_xp"].(float64) != 150 {
		t.Fatalf("after award: %v", p)
	}

	// Publishing a new curve does not change existing players' pinned version.
	cfg := testConfig()
	cfg["progression"] = []int{50, 60, 70}
	expect(t, call(t, f.h, "PUT", f.envPath("/configurations/draft"), cfg, bearer(f.userToken)), 200)
	expect(t, call(t, f.h, "POST", f.envPath("/configurations/publish"), nil, bearer(f.userToken)), 201)
	if p := progression(f.player); p["level"].(float64) != 2 || p["version"].(float64) != 1 {
		t.Fatalf("existing player changed level/version: %v", p)
	}
	newID, newToken, _ := f.registerPlayer("late@example.test", "Latecomer")
	expect(t, award(newID, "xp-late", 65), 200)
	if p := progression(newToken); p["level"].(float64) != 3 || p["version"].(float64) != 2 {
		t.Fatalf("new player should use version 2: %v", p)
	}

	// Malformed and unknown player IDs are 404, not 500.
	expect(t, award("not-a-uuid", "xp-bad", 10), 404)
	expect(t, award("00000000-0000-4000-8000-000000000000", "xp-missing", 10), 404)
	expect(t, award(f.playerID, "xp-zero", 0), 422)
}

func TestIntegrationLeaderboards(t *testing.T) {
	f := newFixture(t)
	type player struct{ id, token string }
	players := []player{{f.playerID, f.player}}
	for i := 1; i < 3; i++ {
		id, token, _ := f.registerPlayer(fmt.Sprintf("p%d@example.test", i), fmt.Sprintf("Player %d", i))
		players = append(players, player{id, token})
	}
	submit := func(p player, board string, score int) {
		t.Helper()
		expect(t, f.playerPost(p.token, "/v1/leaderboards/"+board+"/scores", "s-"+platform.RandomID(), map[string]any{"score": score}), 201)
	}
	// Player 0 and 1 tie on 500; player 0 got there first. Player 2 has a lower best.
	submit(players[0], "global", 200)
	submit(players[0], "global", 500)
	submit(players[1], "global", 500)
	submit(players[1], "global", 100)
	submit(players[2], "global", 450)
	board := expect(t, call(t, f.h, "GET", "/v1/leaderboards/global?environment_id="+f.publicID, nil, nil), 200).obj(t)
	entries := board["entries"].([]any)
	order := []string{}
	for _, e := range entries {
		row := e.(map[string]any)
		order = append(order, fmt.Sprintf("%s:%v", row["player_id"], row["score"]))
	}
	want := []string{players[0].id + ":500", players[1].id + ":500", players[2].id + ":450"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("ranking=%v want %v", order, want)
	}

	// Lower-is-better boards rank ascending; the player's own rank is included with a token.
	submit(players[0], "speedrun", 90)
	submit(players[1], "speedrun", 60)
	submit(players[1], "speedrun", 75)
	mine := expect(t, call(t, f.h, "GET", "/v1/leaderboards/speedrun", nil, bearer(players[0].token)), 200).obj(t)
	if mine["player_rank"].(float64) != 2 || mine["player_score"].(float64) != 90 {
		t.Fatalf("player rank: %v", mine)
	}
	first := mine["entries"].([]any)[0].(map[string]any)
	if first["player_id"] != players[1].id || first["score"].(float64) != 60 {
		t.Fatalf("lower-is-better leader: %v", first)
	}
	expect(t, call(t, f.h, "GET", "/v1/leaderboards/missing?environment_id="+f.publicID, nil, nil), 404)
	expect(t, call(t, f.h, "GET", "/v1/leaderboards/global?environment_id=pub_unknown", nil, nil), 404)
}

func TestIntegrationConfigurationLifecycle(t *testing.T) {
	f := newFixture(t)
	draft := f.envPath("/configurations/draft")
	expect(t, call(t, f.h, "PUT", draft, "null", bearer(f.userToken)), 400)
	expect(t, call(t, f.h, "PUT", draft, "[]", bearer(f.userToken)), 400)

	bad := testConfig()
	bad["progression"] = []int{0, 10}
	expect(t, call(t, f.h, "PUT", draft, bad, bearer(f.userToken)), 200)
	r := expect(t, call(t, f.h, "POST", f.envPath("/configurations/publish"), nil, bearer(f.userToken)), 422)
	if r.Error.Code != "INVALID_CONFIGURATION" || !strings.Contains(r.raw, `"progression[0]"`) {
		t.Fatalf("publish error should name the offending field: %s", r.raw)
	}

	published := expect(t, call(t, f.h, "GET", f.envPath("/configurations/published"), nil, bearer(f.userToken)), 200).obj(t)
	if published["version"].(float64) != 1 {
		t.Fatalf("published: %v", published)
	}

	// A new environment gets its own draft (created atomically) and nothing published.
	env := expect(t, call(t, f.h, "POST", "/v1/projects/"+f.projectID+"/environments", map[string]any{"name": "production"}, bearer(f.userToken)), 201).obj(t)
	prodPath := "/v1/projects/" + f.projectID + "/environments/" + env["id"].(string)
	expect(t, call(t, f.h, "GET", prodPath+"/configurations/draft", nil, bearer(f.userToken)), 200)
	if p := expect(t, call(t, f.h, "GET", prodPath+"/configurations/published", nil, bearer(f.userToken)), 200).obj(t); p["version"] != nil {
		t.Fatalf("new environment should have nothing published: %v", p)
	}
	expect(t, call(t, f.h, "POST", "/v1/projects/"+f.projectID+"/environments", map[string]any{"name": "production"}, bearer(f.userToken)), 409)
	expect(t, call(t, f.h, "POST", "/v1/projects", map[string]any{"name": "Dup", "slug": "owner-game"}, bearer(f.userToken)), 409)
}

func TestIntegrationDemoSeedIsIdempotentAndReconciles(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if err := demo.Seed(ctx, db, logger); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, db: db}
	seededSessions := `SELECT count(*) FROM refresh_tokens WHERE token_hash LIKE 'demo-activity:%'`
	firstRun := f.count(seededSessions)
	if _, err := db.Exec(ctx, `UPDATE configuration_drafts SET config=jsonb_set(config,'{progression}','[10,20]')`); err != nil {
		t.Fatal(err)
	}
	if err := demo.Seed(ctx, db, logger); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	// The activity history: 20 crowd players with a month of sessions; seeding again only
	// fills the gap since the last seeded bucket (at most one new bucket here).
	if n := f.count(`SELECT count(*) FROM players WHERE email LIKE 'crowd%@realmstack.local' AND password_hash IS NULL`); n != 20 {
		t.Fatalf("crowd players=%d, want 20 without passwords", n)
	}
	if firstRun < 1000 {
		t.Fatalf("seeded sessions=%d, want a month of history", firstRun)
	}
	if again := f.count(seededSessions); again < firstRun || again > firstRun+20 {
		t.Fatalf("second seed went from %d to %d sessions; it must only fill the gap", firstRun, again)
	}
	if n := f.count(`SELECT count(*) FROM refresh_tokens WHERE token_hash LIKE 'demo-activity:%' AND revoked_at IS NULL`); n != 0 {
		t.Fatalf("%d seeded sessions are usable", n)
	}
	if n := f.count(`SELECT (config->'progression'->>0)::int FROM configuration_drafts`); n != 10 {
		t.Fatalf("seeding again overwrote the developer's draft (progression[0]=%d)", n)
	}
	if n := f.count(`SELECT amount::int FROM balances WHERE player_id=(SELECT id FROM players WHERE email=$1)`, demo.DemoPlayerEmail); n != 5000 {
		t.Fatalf("balance=%d", n)
	}
	if n := f.count(`SELECT COALESCE(SUM(quantity),0)::int FROM economy_transactions WHERE currency_id IS NOT NULL`); n != 5000 {
		t.Fatalf("currency ledger sum=%d, want 5000 to match the balance", n)
	}
	if n := f.count(`SELECT COALESCE(SUM(quantity),0)::int FROM economy_transactions WHERE item_id IS NOT NULL`); n != 1 {
		t.Fatalf("item ledger sum=%d, want 1", n)
	}

	// The seeded credentials work end to end.
	h := newServer(t, db).Handler()
	// The second admin signs in with a plain username and owns no projects.
	clean := expect(t, call(t, h, "POST", "/v1/auth/login", map[string]any{"email": demo.DemoCleanAdmin, "password": demo.DemoAdminPassword}, nil), 200).obj(t)
	var owned []map[string]any
	expect(t, call(t, h, "GET", "/v1/projects", nil, bearer(clean["access_token"].(string))), 200).into(t, &owned)
	if len(owned) != 0 {
		t.Fatalf("the clean admin owns %d projects, want none", len(owned))
	}
	login := expect(t, call(t, h, "POST", "/v1/players/login", map[string]any{"public_environment_id": demo.DemoPublicEnvironment, "email": demo.DemoPlayerEmail, "password": demo.DemoPlayerPassword}, nil), 200).obj(t)
	playerID := login["player"].(map[string]any)["id"].(string)
	expect(t, call(t, h, "POST", "/v1/server/players/"+playerID+"/currency/credit", map[string]any{"currency_key": "gems", "amount": 5},
		map[string]string{"X-Server-Credential": demo.DemoServerCredential, "Idempotency-Key": "seed-check"}), 200)
}

func TestIntegrationDashboardProjectsAndAuth(t *testing.T) {
	f := newFixture(t)
	auth := func(path, email, password string) apiResult {
		return call(t, f.h, "POST", path, map[string]any{"email": email, "password": password}, nil)
	}
	expect(t, auth("/v1/auth/register", "short@example.test", "short"), 422)
	expect(t, auth("/v1/auth/register", "OWNER@example.test", "AnotherPass123!"), 409) // emails are case-insensitive
	expect(t, auth("/v1/auth/login", "owner@example.test", "wrong-password"), 401)
	expect(t, auth("/v1/auth/login", "ghost@example.test", "DashboardPass123!"), 401)
	expect(t, auth("/v1/auth/login", " Owner@Example.test ", "DashboardPass123!"), 200)
	expect(t, call(t, f.h, "GET", "/v1/projects", nil, nil), 401)
	expect(t, call(t, f.h, "GET", "/v1/projects", nil, bearer("not.a.jwt")), 401)

	var projects []map[string]any
	expect(t, call(t, f.h, "GET", "/v1/projects", nil, bearer(f.userToken)), 200).into(t, &projects)
	if len(projects) != 1 || projects[0]["id"] != f.projectID {
		t.Fatalf("projects: %v", projects)
	}
	var envs []map[string]any
	expect(t, call(t, f.h, "GET", "/v1/projects/"+f.projectID+"/environments", nil, bearer(f.userToken)), 200).into(t, &envs)
	if len(envs) != 1 || envs[0]["public_id"] != f.publicID {
		t.Fatalf("environments: %v", envs)
	}
	expect(t, call(t, f.h, "POST", "/v1/projects/"+f.projectID+"/environments", map[string]any{"name": "staging"}, bearer(f.userToken)), 422)
	var versions []map[string]any
	expect(t, call(t, f.h, "GET", f.envPath("/configurations/versions"), nil, bearer(f.userToken)), 200).into(t, &versions)
	if len(versions) != 1 || versions[0]["version"].(float64) != 1 {
		t.Fatalf("versions: %v", versions)
	}

	// Project updates: rename, invalid slug, slug already used by another project.
	project := "/v1/projects/" + f.projectID
	renamed := expect(t, call(t, f.h, "PATCH", project, map[string]any{"name": "Renamed"}, bearer(f.userToken)), 200).obj(t)
	if renamed["name"] != "Renamed" || renamed["slug"] != "owner-game" {
		t.Fatalf("rename: %v", renamed)
	}
	expect(t, call(t, f.h, "PATCH", project, map[string]any{"slug": "Not A Slug"}, bearer(f.userToken)), 422)
	expect(t, call(t, f.h, "PATCH", project, map[string]any{"name": ""}, bearer(f.userToken)), 422)
	other := expect(t, call(t, f.h, "POST", "/v1/projects", map[string]any{"name": "Other", "slug": "other-game"}, bearer(f.userToken)), 201).obj(t)
	expect(t, call(t, f.h, "PATCH", project, map[string]any{"slug": "other-game"}, bearer(f.userToken)), 409)
	expect(t, call(t, f.h, "POST", "/v1/projects", map[string]any{"name": "", "slug": "x"}, bearer(f.userToken)), 422)

	expect(t, call(t, f.h, "PATCH", "/v1/projects/"+other["id"].(string), map[string]any{"status": "deleted"}, bearer(f.userToken)), 422)
	// Projects cannot be hard-deleted.
	expect(t, call(t, f.h, "DELETE", project, nil, bearer(f.userToken)), 404)
}

func TestIntegrationArchiveKeepsDataAndStopsRuntime(t *testing.T) {
	f := newFixture(t)
	expect(t, f.grant(f.playerID, "health_potion", 2), 200)
	project := "/v1/projects/" + f.projectID

	archived := expect(t, call(t, f.h, "PATCH", project, map[string]any{"status": "archived"}, bearer(f.userToken)), 200).obj(t)
	if archived["status"] != "archived" || archived["archived_at"] == nil {
		t.Fatalf("archive: %v", archived)
	}
	var projects []map[string]any
	expect(t, call(t, f.h, "GET", "/v1/projects", nil, bearer(f.userToken)), 200).into(t, &projects)
	if len(projects) != 0 {
		t.Fatalf("archived project listed by default: %v", projects)
	}
	expect(t, call(t, f.h, "GET", "/v1/projects?include_archived=true", nil, bearer(f.userToken)), 200).into(t, &projects)
	if len(projects) != 1 {
		t.Fatalf("include_archived: %v", projects)
	}

	// Runtime entry points stop: server credentials, registration, login, refresh, public reads.
	expect(t, f.grant(f.playerID, "health_potion", 1), 401)
	expect(t, call(t, f.h, "POST", "/v1/players/login", map[string]any{"public_environment_id": f.publicID, "email": "owner-player@example.test", "password": "PlayerPass123!"}, nil), 404)
	expect(t, call(t, f.h, "POST", "/v1/players/refresh", map[string]any{"refresh_token": f.refresh}, nil), 401)
	expect(t, call(t, f.h, "GET", "/v1/leaderboards/global?environment_id="+f.publicID, nil, nil), 404)

	// The data is untouched, and restoring the project brings the runtime back.
	if n := f.count(`SELECT count(*) FROM economy_transactions WHERE project_id=$1`, f.projectID); n != 1 {
		t.Fatalf("ledger rows=%d after archive, want 1", n)
	}
	restored := expect(t, call(t, f.h, "PATCH", project, map[string]any{"status": "active"}, bearer(f.userToken)), 200).obj(t)
	if restored["status"] != "active" || restored["archived_at"] != nil {
		t.Fatalf("restore: %v", restored)
	}
	expect(t, f.grant(f.playerID, "health_potion", 1), 200)
}

func TestIntegrationLedgersAreAppendOnly(t *testing.T) {
	f := newFixture(t)
	expect(t, f.credit(f.playerID, "credits", 10), 200)
	expect(t, f.server("/v1/server/players/"+f.playerID+"/xp/award", "xp-1", map[string]any{"amount": 10}), 200)
	expect(t, f.playerPost(f.player, "/v1/leaderboards/global/scores", "s1", map[string]any{"score": 1}), 201)
	ctx := context.Background()
	for _, stmt := range []string{
		`UPDATE economy_transactions SET quantity=999`, `DELETE FROM economy_transactions`,
		`UPDATE xp_transactions SET amount=999`, `DELETE FROM xp_transactions`,
		`UPDATE score_submissions SET score=999`, `DELETE FROM score_submissions`,
	} {
		if _, err := f.db.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("%s: err=%v, want append-only rejection", stmt, err)
		}
	}
	if n := f.count(`SELECT count(*) FROM economy_transactions WHERE source='api'`); n != 1 {
		t.Fatalf("ledger rows tagged api=%d", n)
	}
	if n := f.count(`SELECT count(*) FROM xp_transactions x JOIN player_progression p ON p.progression_version_id=x.progression_version_id WHERE x.source='api'`); n != 1 {
		t.Fatalf("xp rows should record the pinned progression version: %d", n)
	}
}

func TestIntegrationMalformedIDsAre404(t *testing.T) {
	f := newFixture(t)
	expect(t, call(t, f.h, "POST", "/v1/credentials/not-a-uuid/revoke", nil, bearer(f.userToken)), 404)
	expect(t, call(t, f.h, "PATCH", "/v1/projects/not-a-uuid", map[string]any{"name": "x"}, bearer(f.userToken)), 404)
	expect(t, call(t, f.h, "GET", "/v1/projects/"+f.projectID+"/environments/bad/configurations/draft", nil, bearer(f.userToken)), 404)
}

func TestIntegrationEnvironmentIsolation(t *testing.T) {
	f := newFixture(t)
	env := expect(t, call(t, f.h, "POST", "/v1/projects/"+f.projectID+"/environments", map[string]any{"name": "production"}, bearer(f.userToken)), 201).obj(t)
	prod := &fixture{t: t, db: f.db, h: f.h, userToken: f.userToken, projectID: f.projectID, envID: env["id"].(string), publicID: env["public_id"].(string)}
	expect(t, call(t, f.h, "PUT", prod.envPath("/configurations/draft"), testConfig(), bearer(f.userToken)), 200)
	expect(t, call(t, f.h, "POST", prod.envPath("/configurations/publish"), nil, bearer(f.userToken)), 201)
	prod.credential = expect(t, call(t, f.h, "POST", prod.envPath("/credentials"), nil, bearer(f.userToken)), 201).obj(t)["credential"].(string)
	prodPlayer, prodToken, _ := prod.registerPlayer("prod-player@example.test", "Prod")

	// Same project, different environment: credentials and players do not cross over.
	expect(t, prod.grant(f.playerID, "health_potion", 1), 404)
	expect(t, f.grant(prodPlayer, "health_potion", 1), 404)
	// The same email may register separately per environment; dev login does not reach prod.
	expect(t, call(t, f.h, "POST", "/v1/players/login", map[string]any{"public_environment_id": f.publicID, "email": "prod-player@example.test", "password": "PlayerPass123!"}, nil), 401)
	f.registerPlayer("prod-player@example.test", "Dev twin")
	// A prod player's scores do not appear on the dev board of the same key.
	expect(t, prod.playerPost(prodToken, "/v1/leaderboards/global/scores", "s1", map[string]any{"score": 99}), 201)
	board := expect(t, call(t, f.h, "GET", "/v1/leaderboards/global?environment_id="+f.publicID, nil, nil), 200).obj(t)
	if len(board["entries"].([]any)) != 0 {
		t.Fatalf("prod score leaked into dev board: %v", board)
	}
	// Registration accepts only the public identifier, not the internal environment UUID.
	expect(t, call(t, f.h, "POST", "/v1/players/register", map[string]any{"public_environment_id": prod.envID, "email": "uuid@example.test", "password": "PlayerPass123!"}, nil), 404)
}

func TestIntegrationIdempotencyNamespacesAndRotation(t *testing.T) {
	f := newFixture(t)
	expect(t, f.credit(f.playerID, "credits", 100), 200)
	// A player using a key the server will later use does not block the server.
	shared := "match-981-reward-1"
	expect(t, f.playerPost(f.player, "/v1/players/me/currency/spend", shared, map[string]any{"currency_key": "credits", "amount": 5}), 200)
	grantPath := "/v1/server/players/" + f.playerID + "/items/grant"
	body := map[string]any{"item_key": "health_potion", "quantity": 1}
	first := expect(t, f.server(grantPath, shared, body), 200).obj(t)

	// After rotating credentials, a retry of the same request replays instead of conflicting.
	f.credential = expect(t, call(t, f.h, "POST", f.envPath("/credentials"), nil, bearer(f.userToken)), 201).obj(t)["credential"].(string)
	replay := expect(t, f.server(grantPath, shared, body), 200).obj(t)
	if !reflect.DeepEqual(first, replay) {
		t.Fatalf("replay after rotation differs: %v vs %v", first, replay)
	}
	if n := f.count(`SELECT quantity::int FROM inventory WHERE player_id=$1`, f.playerID); n != 1 {
		t.Fatalf("quantity=%d, want 1", n)
	}
}

func TestIntegrationConcurrentSameKeyAndStackLimit(t *testing.T) {
	f := newFixture(t)
	path := "/v1/server/players/" + f.playerID + "/items/grant"
	run := func(n int, key func(int) string, qty int) []int {
		var wg sync.WaitGroup
		statuses := make([]int, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				statuses[i] = f.server(path, key(i), map[string]any{"item_key": "health_potion", "quantity": qty}).Status
			}(i)
		}
		wg.Wait()
		return statuses
	}
	// 10 concurrent requests with one key apply once and all return the same success.
	for _, status := range run(10, func(int) string { return "same-key" }, 9) {
		if status != 200 {
			t.Fatalf("same-key status %d", status)
		}
	}
	if n := f.count(`SELECT quantity::int FROM inventory WHERE player_id=$1`, f.playerID); n != 9 {
		t.Fatalf("quantity=%d after same-key burst, want 9", n)
	}
	// 20 concurrent grants of 10 onto 9 (max 99): exactly 9 fit.
	ok, limited := 0, 0
	for _, status := range run(20, func(i int) string { return fmt.Sprintf("burst-%d", i) }, 10) {
		switch status {
		case 200:
			ok++
		case 422:
			limited++
		default:
			t.Fatalf("unexpected status %d", status)
		}
	}
	if ok != 9 || limited != 11 {
		t.Fatalf("ok=%d limited=%d, want 9/11", ok, limited)
	}
	if n := f.count(`SELECT quantity::int FROM inventory WHERE player_id=$1`, f.playerID); n != 99 {
		t.Fatalf("quantity=%d, want 99", n)
	}
}

func TestIntegrationRefreshTokenExpiry(t *testing.T) {
	f := newFixture(t)
	srv := newServer(t, f.db)
	srv.Env.Now = func() time.Time { return time.Now().Add(2 * time.Hour) } // past the 1h refresh TTL
	r := expect(t, call(t, srv.Handler(), "POST", "/v1/players/refresh", map[string]any{"refresh_token": f.refresh}, nil), 401)
	if r.Error.Code != "INVALID_REFRESH_TOKEN" {
		t.Fatalf("code=%s", r.Error.Code)
	}
	// Access tokens expire on the same clock.
	expect(t, call(t, srv.Handler(), "GET", "/v1/players/me", nil, bearer(f.player)), 401)
}

func TestIntegrationSeasonBoundaries(t *testing.T) {
	f := newFixture(t)
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(7 * 24 * time.Hour)
	cfg := testConfig()
	cfg["leaderboards"] = append(cfg["leaderboards"].([]map[string]any), map[string]any{"key": "season-1", "direction": "higher", "season_start": start, "season_end": end})
	expect(t, call(t, f.h, "PUT", f.envPath("/configurations/draft"), cfg, bearer(f.userToken)), 200)
	expect(t, call(t, f.h, "POST", f.envPath("/configurations/publish"), nil, bearer(f.userToken)), 201)

	srv := newServer(t, f.db)
	submitAt := func(at time.Time, score int) apiResult {
		srv.Env.Now = func() time.Time { return at }
		token := srv.Env.Token(platform.Principal{ID: f.playerID, ProjectID: f.projectID, EnvironmentID: f.envID, Kind: "player"}, time.Minute)
		return call(t, srv.Handler(), "POST", "/v1/leaderboards/season-1/scores", map[string]any{"score": score}, withKey(bearer(token), "s-"+platform.RandomID()))
	}
	expect(t, submitAt(start.Add(-time.Nanosecond), 1), 422) // before start
	expect(t, submitAt(start, 10), 201)                      // start is inclusive
	expect(t, submitAt(end.Add(-time.Microsecond), 20), 201) // last instant inside
	expect(t, submitAt(end, 30), 422)                        // end is exclusive

	board := expect(t, call(t, f.h, "GET", "/v1/leaderboards/season-1?environment_id="+f.publicID, nil, nil), 200).obj(t)
	entry := board["entries"].([]any)[0].(map[string]any)
	if entry["score"].(float64) != 20 {
		t.Fatalf("best in-season score: %v", entry)
	}
	// Stored timestamps are the ones the season check used.
	if n := f.count(`SELECT count(*) FROM score_submissions WHERE submitted_at >= $1 AND submitted_at < $2`, start, end); n != 2 {
		t.Fatalf("stored in-season submissions=%d, want 2", n)
	}
}

func TestIntegrationScoredLeaderboardIsImmutable(t *testing.T) {
	f := newFixture(t)
	cfg := testConfig()
	cfg["leaderboards"] = []map[string]any{{"key": "global", "direction": "lower"}, {"key": "speedrun", "direction": "higher"}}
	// No scores yet: direction may still change.
	expect(t, call(t, f.h, "PUT", f.envPath("/configurations/draft"), cfg, bearer(f.userToken)), 200)
	expect(t, call(t, f.h, "POST", f.envPath("/configurations/publish"), nil, bearer(f.userToken)), 201)

	expect(t, f.playerPost(f.player, "/v1/leaderboards/global/scores", "s1", map[string]any{"score": 5}), 201)
	cfg["leaderboards"] = []map[string]any{{"key": "global", "direction": "higher"}, {"key": "speedrun", "direction": "lower"}}
	expect(t, call(t, f.h, "PUT", f.envPath("/configurations/draft"), cfg, bearer(f.userToken)), 200)
	r := expect(t, call(t, f.h, "POST", f.envPath("/configurations/publish"), nil, bearer(f.userToken)), 422)
	if !strings.Contains(r.raw, `"leaderboards[0].direction"`) || strings.Contains(r.raw, "leaderboards[1]") {
		t.Fatalf("expected only the scored board to be rejected: %s", r.raw)
	}
}

func TestIntegrationPublishFieldErrorsAndValidate(t *testing.T) {
	f := newFixture(t)
	bad := testConfig()
	bad["progression"] = []int{0, 10}
	bad["currencies"] = []map[string]any{{"key": "credits", "name": "Credits"}, {"key": "credits", "name": "Again"}}
	expect(t, call(t, f.h, "PUT", f.envPath("/configurations/draft"), bad, bearer(f.userToken)), 200)

	r := expect(t, call(t, f.h, "POST", f.envPath("/configurations/publish"), nil, bearer(f.userToken)), 422)
	var envelope struct {
		Error struct {
			Details struct {
				Errors []platform.FieldError `json:"errors"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.raw), &envelope); err != nil {
		t.Fatal(err)
	}
	if got := fields(envelope.Error.Details.Errors); strings.Join(got, ",") != "currencies[1].key,progression[0]" {
		t.Fatalf("publish field errors=%v", got)
	}
	// Validate the stored draft, or a supplied body, without saving.
	stored := expect(t, call(t, f.h, "POST", f.envPath("/configurations/validate"), nil, bearer(f.userToken)), 200).obj(t)
	if stored["valid"] != false || len(stored["errors"].([]any)) != 2 {
		t.Fatalf("validate stored: %v", stored)
	}
	supplied := expect(t, call(t, f.h, "POST", f.envPath("/configurations/validate"), testConfig(), bearer(f.userToken)), 200).obj(t)
	if supplied["valid"] != true {
		t.Fatalf("validate supplied: %v", supplied)
	}
}

func TestIntegrationPublishOnlyVersionsChangedProgression(t *testing.T) {
	f := newFixture(t)
	// Republishing unchanged thresholds keeps progression version 1.
	r := expect(t, call(t, f.h, "POST", f.envPath("/configurations/publish"), nil, bearer(f.userToken)), 201).obj(t)
	if r["version"].(float64) != 2 || r["progression_version"].(float64) != 1 || r["progression_version_created"] != false {
		t.Fatalf("unchanged publish: %v", r)
	}
	expect(t, call(t, f.h, "PUT", f.envPath("/progression"), map[string]any{"thresholds": []int{10, 20}}, bearer(f.userToken)), 200)
	r = expect(t, call(t, f.h, "POST", f.envPath("/configurations/publish"), nil, bearer(f.userToken)), 201).obj(t)
	if r["version"].(float64) != 3 || r["progression_version"].(float64) != 2 || r["progression_version_created"] != true {
		t.Fatalf("changed publish: %v", r)
	}
	var versions []map[string]any
	expect(t, call(t, f.h, "GET", f.envPath("/configurations/versions"), nil, bearer(f.userToken)), 200).into(t, &versions)
	if versions[0]["published_by"] == nil {
		t.Fatalf("version does not record the publisher: %v", versions[0])
	}
}

func TestIntegrationDefinitionCRUD(t *testing.T) {
	f := newFixture(t)
	items := f.envPath("/items")
	created := expect(t, call(t, f.h, "POST", items, map[string]any{"key": "mana_potion", "name": "Mana Potion", "category": "consumable", "stackable": true, "max_stack": 20}, bearer(f.userToken)), 201).obj(t)
	if created["key"] != "mana_potion" {
		t.Fatalf("created: %v", created)
	}
	expect(t, call(t, f.h, "POST", items, map[string]any{"key": "mana_potion", "name": "Dup", "stackable": true, "max_stack": 1}, bearer(f.userToken)), 409)
	invalid := expect(t, call(t, f.h, "POST", items, map[string]any{"key": "bad", "name": "", "max_stack": 0}, bearer(f.userToken)), 422)
	if !strings.Contains(invalid.raw, `"item.name"`) || !strings.Contains(invalid.raw, `"item.max_stack"`) {
		t.Fatalf("field errors: %s", invalid.raw)
	}
	expect(t, call(t, f.h, "POST", items, map[string]any{"key": "x", "name": "X", "max_stack": 1, "unknown": true}, bearer(f.userToken)), 400)

	// Replace (key may be omitted from the body), read, list, delete.
	expect(t, call(t, f.h, "PUT", items+"/mana_potion", map[string]any{"name": "Greater Mana", "stackable": true, "max_stack": 50}, bearer(f.userToken)), 200)
	expect(t, call(t, f.h, "PUT", items+"/mana_potion", map[string]any{"key": "other", "name": "X", "max_stack": 1}, bearer(f.userToken)), 422)
	got := expect(t, call(t, f.h, "GET", items+"/mana_potion", nil, bearer(f.userToken)), 200).obj(t)
	if got["name"] != "Greater Mana" || got["max_stack"].(float64) != 50 {
		t.Fatalf("replaced: %v", got)
	}
	var list []map[string]any
	expect(t, call(t, f.h, "GET", items, nil, bearer(f.userToken)), 200).into(t, &list)
	if len(list) != 4 {
		t.Fatalf("items in draft=%d, want 4", len(list))
	}
	expect(t, call(t, f.h, "DELETE", items+"/health_potion", nil, bearer(f.userToken)), 200)
	expect(t, call(t, f.h, "DELETE", items+"/health_potion", nil, bearer(f.userToken)), 404)
	expect(t, call(t, f.h, "GET", items+"/health_potion", nil, bearer(f.userToken)), 404)

	// Other collections and progression use the same draft.
	expect(t, call(t, f.h, "POST", f.envPath("/currencies"), map[string]any{"key": "shards", "name": "Shards"}, bearer(f.userToken)), 201)
	expect(t, call(t, f.h, "POST", f.envPath("/leaderboards"), map[string]any{"key": "weekly", "direction": "up"}, bearer(f.userToken)), 422)
	expect(t, call(t, f.h, "PUT", f.envPath("/progression"), map[string]any{"thresholds": []int{50, 40}}, bearer(f.userToken)), 422)
	expect(t, call(t, f.h, "PUT", f.envPath("/progression"), map[string]any{"thresholds": []int{50, 400}}, bearer(f.userToken)), 200)
	draft := expect(t, call(t, f.h, "GET", f.envPath("/configurations/draft"), nil, bearer(f.userToken)), 200).obj(t)
	if len(draft["currencies"].([]any)) != 3 || len(draft["progression"].([]any)) != 2 {
		t.Fatalf("draft: %v", draft)
	}
	// Edits are draft-only until published, and another owner cannot edit.
	expect(t, f.grant(f.playerID, "mana_potion", 1), 404)
	expect(t, call(t, f.h, "POST", f.envPath("/configurations/publish"), nil, bearer(f.userToken)), 201)
	expect(t, f.grant(f.playerID, "mana_potion", 1), 200)
	other := &fixture{t: t, db: f.db, h: f.h}
	other.setupTenant("intruder")
	expect(t, call(t, f.h, "GET", items, nil, bearer(other.userToken)), 404)
	expect(t, call(t, f.h, "DELETE", items+"/mana_potion", nil, bearer(other.userToken)), 404)
}

func TestIntegrationTransactionPagination(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 5; i++ {
		expect(t, f.credit(f.playerID, "credits", i+1), 200)
	}
	var seen []string
	cursor := ""
	for page := 0; page < 5; page++ {
		path := "/v1/players/me/transactions?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		res := expect(t, call(t, f.h, "GET", path, nil, bearer(f.player)), 200)
		var rows []map[string]any
		res.into(t, &rows)
		for _, row := range rows {
			seen = append(seen, fmt.Sprint(row["amount"]))
		}
		var envelope struct {
			Meta struct {
				NextCursor string `json:"next_cursor"`
			} `json:"meta"`
		}
		_ = json.Unmarshal([]byte(res.raw), &envelope)
		if cursor = envelope.Meta.NextCursor; cursor == "" {
			break
		}
	}
	if strings.Join(seen, ",") != "5,4,3,2,1" {
		t.Fatalf("paged history=%v, want newest first without gaps or repeats", seen)
	}
	expect(t, call(t, f.h, "GET", "/v1/players/me/transactions?cursor=garbage", nil, bearer(f.player)), 400)
}

func TestIntegrationXPAwardReportsPinnedVersion(t *testing.T) {
	f := newFixture(t)
	path := "/v1/server/players/" + f.playerID + "/xp/award"
	r := expect(t, f.server(path, "xp-1", map[string]any{"amount": 150}), 200).obj(t)
	if r["level"].(float64) != 2 || r["leveled_up"] != true || r["progression_version"].(float64) != 1 {
		t.Fatalf("award: %v", r)
	}
	expect(t, call(t, f.h, "PUT", f.envPath("/progression"), map[string]any{"thresholds": []int{10, 20, 30}}, bearer(f.userToken)), 200)
	expect(t, call(t, f.h, "POST", f.envPath("/configurations/publish"), nil, bearer(f.userToken)), 201)
	r = expect(t, f.server(path, "xp-2", map[string]any{"amount": 10}), 200).obj(t)
	if r["progression_version"].(float64) != 1 || r["level"].(float64) != 2 || r["leveled_up"] != false {
		t.Fatalf("pinned player should stay on version 1: %v", r)
	}
}

func TestIntegrationRegistrationIgnoresExternalKey(t *testing.T) {
	f := newFixture(t)
	expect(t, call(t, f.h, "POST", "/v1/players/register", map[string]any{"public_environment_id": f.publicID, "email": "ext@example.test", "password": "PlayerPass123!", "external_player_key": "steam:123"}, nil), 201)
	if n := f.count(`SELECT count(*) FROM players WHERE external_player_key IS NOT NULL`); n != 0 {
		t.Fatalf("client-supplied external key was stored")
	}
}

func TestIntegrationPlayerReadsAndProfile(t *testing.T) {
	f := newFixture(t)
	expect(t, f.grant(f.playerID, "health_potion", 2), 200)
	expect(t, f.credit(f.playerID, "credits", 40), 200)
	expect(t, f.playerPost(f.player, "/v1/players/me/currency/spend", "buy-1", map[string]any{"currency_key": "credits", "amount": 15, "reason": "shop", "reference": "order-1"}), 200)

	item := expect(t, call(t, f.h, "GET", "/v1/players/me/inventory/health_potion", nil, bearer(f.player)), 200).obj(t)
	if item["quantity"].(float64) != 2 {
		t.Fatalf("item: %v", item)
	}
	expect(t, call(t, f.h, "GET", "/v1/players/me/inventory/iron_sword", nil, bearer(f.player)), 404)

	var balances []map[string]any
	expect(t, call(t, f.h, "GET", "/v1/players/me/currency", nil, bearer(f.player)), 200).into(t, &balances)
	got := map[string]float64{}
	for _, b := range balances {
		got[b["currency_key"].(string)] = b["balance"].(float64)
	}
	if got["credits"] != 25 || got["gems"] != 0 {
		t.Fatalf("balances: %v", got)
	}

	var history []map[string]any
	expect(t, call(t, f.h, "GET", "/v1/players/me/transactions?limit=2", nil, bearer(f.player)), 200).into(t, &history)
	if len(history) != 2 || history[0]["operation"] != "spend" || history[0]["reference"] != "order-1" || history[0]["balance"].(float64) != 25 {
		t.Fatalf("history: %v", history)
	}

	// Profile updates require an idempotency key, validate input, and replay safely.
	expect(t, call(t, f.h, "PATCH", "/v1/players/me", map[string]any{"display_name": "Nova"}, bearer(f.player)), 400)
	expect(t, call(t, f.h, "PATCH", "/v1/players/me", map[string]any{}, withKey(bearer(f.player), "p0")), 422)
	expect(t, call(t, f.h, "PATCH", "/v1/players/me", map[string]any{"display_name": strings.Repeat("x", 81)}, withKey(bearer(f.player), "p1")), 422)
	expect(t, call(t, f.h, "PATCH", "/v1/players/me", map[string]any{"metadata": map[string]any{"bio": strings.Repeat("x", 17000)}}, withKey(bearer(f.player), "p2")), 422)
	update := map[string]any{"display_name": "Nova", "metadata": map[string]any{"title": "Pathfinder"}}
	first := expect(t, call(t, f.h, "PATCH", "/v1/players/me", update, withKey(bearer(f.player), "p3")), 200).obj(t)
	second := expect(t, call(t, f.h, "PATCH", "/v1/players/me", update, withKey(bearer(f.player), "p3")), 200).obj(t)
	if !reflect.DeepEqual(first, second) || first["display_name"] != "Nova" {
		t.Fatalf("profile update: %v / %v", first, second)
	}
	me := expect(t, call(t, f.h, "GET", "/v1/players/me", nil, bearer(f.player)), 200).obj(t)
	if me["metadata"].(map[string]any)["title"] != "Pathfinder" {
		t.Fatalf("metadata not stored: %v", me)
	}
}

// fields returns the field paths of validation errors.
func fields(errs []platform.FieldError) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Field
	}
	return out
}
