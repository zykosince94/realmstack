package httpapi

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"realmstack-backend/internal/ai"
	"realmstack-backend/internal/platform"
)

// fakeProvider answers with canned JSON and records every request, so tests can assert
// exactly what would have been sent to the provider. No test calls a real API.
type fakeProvider struct {
	mu       sync.Mutex
	json     string
	err      error
	checkErr error
	requests []ai.Request
}

func (p *fakeProvider) Generate(_ context.Context, req ai.Request) (ai.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	return ai.Result{JSON: p.json, InputTokens: 1200, OutputTokens: 340}, p.err
}

func (p *fakeProvider) Check(context.Context) error { return p.checkErr }

func (p *fakeProvider) last() ai.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[len(p.requests)-1]
}

var aiTestKey = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

// withAI points the fixture at a server with the assistant enabled and a fake provider.
// Generations run synchronously so each POST returns the finished result.
func (f *fixture) withAI(cfg ai.Config) (*Server, *fakeProvider) {
	cfg.EncryptionKey = aiTestKey
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := New(f.db, Config{JWTSecret: strings.Repeat("k", 40), AccessTTL: time.Minute, RefreshTTL: time.Hour, BodyLimit: 1 << 20, AuthRateLimit: 10000, AI: cfg}, logger)
	fake := &fakeProvider{}
	srv.AI.NewProvider = func(s ai.Settings) (ai.Provider, error) { return fake, nil }
	srv.AI.Sync = true
	f.h = srv.Handler()
	return srv, fake
}

func (f *fixture) aiSettings(key string) apiResult {
	return call(f.t, f.h, "PUT", "/v1/ai/settings", map[string]any{"provider": "openai", "model": "gpt-5.5", "api_key": key}, bearer(f.userToken))
}

func (f *fixture) generate(op string, params map[string]any, policy string) map[string]any {
	f.t.Helper()
	body := map[string]any{"operation": op, "params": params}
	if policy != "" {
		body["merge_policy"] = policy
	}
	return expect(f.t, call(f.t, f.h, "POST", f.envPath("/ai/generations"), body, bearer(f.userToken)), 202).obj(f.t)
}

func (f *fixture) draft() map[string]any {
	f.t.Helper()
	return expect(f.t, call(f.t, f.h, "GET", f.envPath("/configurations/draft"), nil, bearer(f.userToken)), 200).obj(f.t)
}

func keysIn(list any) []string {
	var out []string
	for _, x := range list.([]any) {
		out = append(out, x.(map[string]any)["key"].(string))
	}
	return out
}

const validItems = `{"items":[{"key":"magic_wand","name":"Magic Wand","category":"weapon","stackable":false,"max_stack":1,"slot":"weapon"}],"notes":"One wand."}`

// The achievement rewards an item that does not exist and needs an unreachable level.
const invalidAchievements = `{"stats":[],"notes":"Bad on purpose.","achievements":[{"key":"wand_master","name":"Wand Master","description":"",
	"hidden":false,"claimable_by":"player","conditions":[{"type":"level_reached","target":99}],
	"rewards":{"items":[{"item_key":"no_such_item","quantity":1}],"currencies":[],"xp":0}}]}`

func TestIntegrationAIDisabledLeavesPlatformUsable(t *testing.T) {
	f := newFixture(t) // the default test server has no AI_ENCRYPTION_KEY
	r := expect(t, call(t, f.h, "GET", "/v1/ai/settings", nil, bearer(f.userToken)), 503)
	if r.Error.Code != "AI_UNAVAILABLE" {
		t.Fatalf("code=%s", r.Error.Code)
	}
	expect(t, call(t, f.h, "POST", f.envPath("/ai/generations"), map[string]any{"operation": "items"}, bearer(f.userToken)), 503)
	// Core features are unaffected.
	expect(t, f.grant(f.playerID, "health_potion", 1), 200)
	expect(t, call(t, f.h, "POST", f.envPath("/configurations/publish"), nil, bearer(f.userToken)), 201)
}

func TestIntegrationAISettingsAreEncryptedAndPrivate(t *testing.T) {
	f := newFixture(t)
	_, fake := f.withAI(ai.Config{})
	// Not configured yet.
	if got := expect(t, call(t, f.h, "GET", "/v1/ai/settings", nil, bearer(f.userToken)), 200).obj(t); got["configured"] != false {
		t.Fatalf("settings: %v", got)
	}
	r := expect(t, call(t, f.h, "PUT", "/v1/ai/settings", map[string]any{"provider": "openai", "model": "gpt-5.5"}, bearer(f.userToken)), 422)
	if !strings.Contains(r.raw, "api_key") {
		t.Fatalf("first save needs a key: %s", r.raw)
	}
	expect(t, call(t, f.h, "PUT", "/v1/ai/settings", map[string]any{"provider": "acme", "model": "gpt 5", "api_key": "short"}, bearer(f.userToken)), 422)

	secret := "sk-proj-THISISASECRETKEYVALUE9876"
	saved := expect(t, f.aiSettings(secret), 200)
	if saved.obj(t)["key_last4"] != "9876" || strings.Contains(saved.raw, "THISISASECRET") {
		t.Fatalf("settings response leaks the key: %s", saved.raw)
	}
	if got := expect(t, call(t, f.h, "GET", "/v1/ai/settings", nil, bearer(f.userToken)), 200); strings.Contains(got.raw, "THISISASECRET") {
		t.Fatal("GET leaks the key")
	}
	// At rest the key is ciphertext.
	if n := f.count(`SELECT count(*) FROM ai_provider_settings WHERE position(convert_to('THISISASECRET','UTF8') in encrypted_api_key) > 0`); n != 0 {
		t.Fatal("the key is stored in plaintext")
	}
	// Changing only the model keeps the key.
	expect(t, call(t, f.h, "PUT", "/v1/ai/settings", map[string]any{"provider": "openai", "model": "gpt-6.1-sol"}, bearer(f.userToken)), 200)
	expect(t, call(t, f.h, "POST", "/v1/ai/settings/test", nil, bearer(f.userToken)), 200)
	fake.checkErr = &ai.ProviderError{Code: ai.CodeKeyRejected, Message: "nope"}
	if r := expect(t, call(t, f.h, "POST", "/v1/ai/settings/test", nil, bearer(f.userToken)), 422); r.Error.Code != ai.CodeKeyRejected {
		t.Fatalf("code=%s", r.Error.Code)
	}

	// Another dashboard user sees their own (empty) settings, not these.
	rival := &fixture{t: t, db: f.db, h: f.h}
	rival.setupTenant("rival")
	if got := expect(t, call(t, f.h, "GET", "/v1/ai/settings", nil, bearer(rival.userToken)), 200).obj(t); got["configured"] != false {
		t.Fatalf("rival sees settings: %v", got)
	}
	expect(t, call(t, f.h, "DELETE", "/v1/ai/settings", nil, bearer(f.userToken)), 200)
	if r := expect(t, call(t, f.h, "POST", f.envPath("/ai/generations"), map[string]any{"operation": "items"}, bearer(f.userToken)), 409); r.Error.Code != "AI_NOT_CONFIGURED" {
		t.Fatalf("code=%s", r.Error.Code)
	}
}

// TestIntegrationAIProposalValidation checks that an AI-generated invalid configuration
// is rejected and a valid configuration is published only after human approval.
func TestIntegrationAIProposalValidation(t *testing.T) {
	f := newFixture(t)
	_, fake := f.withAI(ai.Config{})
	expect(t, f.aiSettings("sk-proj-TESTKEYVALUE00000001"), 200)
	published := func() string {
		return fmt.Sprint(expect(t, call(t, f.h, "GET", f.envPath("/configurations/published"), nil, bearer(f.userToken)), 200).obj(t))
	}
	publishedBefore := published()
	draftBefore := f.draft()

	// Invalid: rejected with field errors, and it cannot be applied.
	fake.json = invalidAchievements
	bad := f.generate("achievements", map[string]any{"brief": "wand themed", "count": 1}, "")
	if bad["status"] != "invalid" || bad["appliable"] != false {
		t.Fatalf("invalid generation: %v", bad)
	}
	errs := strings.Join(fields(decodeFieldErrors(t, bad["errors"])), ",")
	for _, want := range []string{"achievements[0].conditions[0].target", "achievements[0].rewards.items[0].item_key"} {
		if !strings.Contains(errs, want) {
			t.Errorf("missing error %s in %s", want, errs)
		}
	}
	if r := expect(t, call(t, f.h, "POST", f.envPath("/ai/generations/"+bad["id"].(string)+"/apply"), nil, bearer(f.userToken)), 422); r.Error.Code != "NOT_APPLICABLE" {
		t.Fatalf("code=%s", r.Error.Code)
	}
	if len(f.draft()["achievements"].([]any)) != len(draftBefore["achievements"].([]any)) {
		t.Fatal("an invalid proposal changed the draft")
	}

	// Valid: nothing changes until a person applies it, and applying edits only the draft.
	fake.json = validItems
	good := f.generate("items", map[string]any{"brief": "a wizard game", "count": 1}, "")
	// errors is always an array, empty for a valid proposal: the dashboard reads its
	// length (a null here once crashed the review screen).
	if errs, ok := good["errors"].([]any); !ok || len(errs) != 0 {
		t.Fatalf("valid generation errors = %#v, want []", good["errors"])
	}
	if f.count(`SELECT count(*) FROM ai_generations WHERE jsonb_typeof(errors) <> 'array'`) != 0 {
		t.Fatal("a generation stored errors that are not an array")
	}
	if good["status"] != "valid" || good["appliable"] != true || !strings.Contains(good["notes"].(string), "One wand") {
		t.Fatalf("valid generation: %v", good)
	}
	if usage := good["usage"].(map[string]any); usage["input_tokens"].(float64) != 1200 {
		t.Fatalf("usage: %v", usage)
	}
	if strings.Contains(strings.Join(keysIn(f.draft()["items"]), ","), "magic_wand") {
		t.Fatal("a generation changed the draft before approval")
	}
	applyPath := f.envPath("/ai/generations/" + good["id"].(string) + "/apply")
	applied := expect(t, call(t, f.h, "POST", applyPath, nil, bearer(f.userToken)), 200).obj(t)
	if applied["status"] != "applied" || applied["applied_at"] == nil {
		t.Fatalf("applied: %v", applied)
	}
	if r := expect(t, call(t, f.h, "POST", applyPath, nil, bearer(f.userToken)), 409); r.Error.Code != "ALREADY_APPLIED" {
		t.Fatalf("code=%s", r.Error.Code)
	}
	if !strings.Contains(strings.Join(keysIn(f.draft()["items"]), ","), "magic_wand") {
		t.Fatal("apply did not change the draft")
	}
	if published() != publishedBefore {
		t.Fatal("applying a proposal changed the live configuration")
	}
	expect(t, call(t, f.h, "POST", f.envPath("/configurations/publish"), nil, bearer(f.userToken)), 201)
	if n := f.count(`SELECT count(*) FROM item_definitions WHERE project_id=$1 AND item_key='magic_wand' AND active`, f.projectID); n != 1 {
		t.Fatal("published item missing")
	}
}

func decodeFieldErrors(t *testing.T, raw any) []platform.FieldError {
	var out []platform.FieldError
	for _, e := range raw.([]any) {
		m := e.(map[string]any)
		out = append(out, platform.FieldError{Field: m["field"].(string), Message: m["message"].(string)})
	}
	return out
}

func TestIntegrationAIConflictsDraftChangesAndFailures(t *testing.T) {
	f := newFixture(t)
	_, fake := f.withAI(ai.Config{})
	expect(t, f.aiSettings("sk-proj-TESTKEYVALUE00000002"), 200)

	// add policy: a key that already exists is a conflict; replace overwrites it.
	fake.json = `{"items":[{"key":"health_potion","name":"Greater Potion","category":"consumable","stackable":true,"max_stack":99,"slot":null}],"notes":""}`
	conflict := f.generate("items", nil, "")
	if conflict["status"] != "invalid" || !strings.Contains(strings.Join(fields(decodeFieldErrors(t, conflict["errors"])), ","), "proposal.items[0].key") {
		t.Fatalf("conflict: %v", conflict)
	}
	replaced := f.generate("items", nil, "replace")
	if replaced["status"] != "valid" {
		t.Fatalf("replace: %v", replaced)
	}

	// The draft changes after generation in a way that conflicts: 409 DRAFT_CHANGED.
	fake.json = validItems
	stale := f.generate("items", nil, "")
	expect(t, call(t, f.h, "POST", f.envPath("/items"), map[string]any{"key": "magic_wand", "name": "Hand-made wand", "stackable": false, "max_stack": 1}, bearer(f.userToken)), 201)
	if r := expect(t, call(t, f.h, "POST", f.envPath("/ai/generations/"+stale["id"].(string)+"/apply"), nil, bearer(f.userToken)), 409); r.Error.Code != "DRAFT_CHANGED" {
		t.Fatalf("code=%s %s", r.Error.Code, r.raw)
	}

	// Provider failures and malformed output become failed generations with a code.
	fake.err = &ai.ProviderError{Code: ai.CodeRateLimited, Message: "quota"}
	failed := f.generate("items", nil, "")
	if failed["status"] != "failed" || failed["error"].(map[string]any)["code"] != ai.CodeRateLimited {
		t.Fatalf("failed: %v", failed)
	}
	fake.err, fake.json = nil, `{"items":"not an array"}`
	if g := f.generate("items", nil, ""); g["error"].(map[string]any)["code"] != ai.CodeMalformedOutput {
		t.Fatalf("malformed: %v", g)
	}
	// A failed generation can be discarded; an applied one cannot.
	expect(t, call(t, f.h, "POST", f.envPath("/ai/generations/"+failed["id"].(string)+"/discard"), nil, bearer(f.userToken)), 200)
	expect(t, call(t, f.h, "POST", f.envPath("/ai/generations/"+replaced["id"].(string)+"/apply"), nil, bearer(f.userToken)), 200)
	expect(t, call(t, f.h, "POST", f.envPath("/ai/generations/"+replaced["id"].(string)+"/discard"), nil, bearer(f.userToken)), 409)

	// explain answers and cannot be applied.
	fake.json = `{"answer":"The potion heals 50."}`
	answer := f.generate("explain", map[string]any{"question": "What does the potion do?"}, "")
	if answer["status"] != "answered" || answer["answer"] != "The potion heals 50." {
		t.Fatalf("explain: %v", answer)
	}
	expect(t, call(t, f.h, "POST", f.envPath("/ai/generations/"+answer["id"].(string)+"/apply"), nil, bearer(f.userToken)), 422)
	if r := expect(t, call(t, f.h, "POST", f.envPath("/ai/generations"), map[string]any{"operation": "explain"}, bearer(f.userToken)), 422); !strings.Contains(r.raw, "params.question") {
		t.Fatalf("explain needs a question: %s", r.raw)
	}

	var list []map[string]any
	expect(t, call(t, f.h, "GET", f.envPath("/ai/generations"), nil, bearer(f.userToken)), 200).into(t, &list)
	if len(list) != 6 || list[0]["operation"] != "explain" {
		t.Fatalf("history: %d %v", len(list), list[0]["operation"])
	}
}

// What leaves the server: the draft's definitions and the brief, never players,
// emails, credentials or ledger data.
func TestIntegrationAIPromptContainsNoPrivateData(t *testing.T) {
	f := newFixture(t)
	_, fake := f.withAI(ai.Config{})
	expect(t, f.aiSettings("sk-proj-TESTKEYVALUE00000003"), 200)
	expect(t, f.grant(f.playerID, "health_potion", 3), 200)
	fake.json = validItems
	f.generate("items", map[string]any{"brief": "add a wand"}, "")
	req := fake.last()
	prompt := req.Instructions + req.Input
	for _, private := range []string{"owner-player@example.test", "owner@example.test", f.playerID, f.credential, strings.SplitN(f.credential, ".", 2)[1], f.userToken, "Hero owner"} {
		if strings.Contains(prompt, private) {
			t.Errorf("prompt contains private value %q", private)
		}
	}
	if !strings.Contains(req.Input, "health_potion") || !strings.Contains(req.Input, "add a wand") || req.Schema == nil || req.SchemaName != "realmstack_items" {
		t.Fatalf("prompt is missing the draft or brief: %+v", req)
	}
}

func TestIntegrationAITenancyAndLimits(t *testing.T) {
	f := newFixture(t)
	_, fake := f.withAI(ai.Config{PerMinute: 2})
	expect(t, f.aiSettings("sk-proj-TESTKEYVALUE00000004"), 200)
	fake.json = validItems
	g := f.generate("items", nil, "")

	rival := &fixture{t: t, db: f.db, h: f.h}
	rival.setupTenant("rival")
	expect(t, call(t, f.h, "GET", f.envPath("/ai/generations/"+g["id"].(string)), nil, bearer(rival.userToken)), 404)
	expect(t, call(t, f.h, "POST", f.envPath("/ai/generations/"+g["id"].(string)+"/apply"), nil, bearer(rival.userToken)), 404)
	// The rival's own environment cannot reach this generation either.
	expect(t, call(t, f.h, "GET", rival.envPath("/ai/generations/"+g["id"].(string)), nil, bearer(rival.userToken)), 404)

	f.generate("items", nil, "")
	if r := expect(t, call(t, f.h, "POST", f.envPath("/ai/generations"), map[string]any{"operation": "items"}, bearer(f.userToken)), 429); r.Error.Code != "RATE_LIMITED" {
		t.Fatalf("code=%s", r.Error.Code)
	}

	// Generations left running by a crash are closed on startup.
	if _, err := f.db.Exec(context.Background(), `UPDATE ai_generations SET status='running' WHERE id=$1`, g["id"]); err != nil {
		t.Fatal(err)
	}
	if err := ai.RecoverAbandoned(context.Background(), f.db); err != nil {
		t.Fatal(err)
	}
	got := expect(t, call(t, f.h, "GET", f.envPath("/ai/generations/"+g["id"].(string)), nil, bearer(f.userToken)), 200).obj(t)
	if got["status"] != "failed" || got["error"].(map[string]any)["code"] != ai.CodeAbandonedByRestart {
		t.Fatalf("recovered: %v", got)
	}
}

func TestIntegrationConfigurationRollback(t *testing.T) {
	f := newFixture(t)
	v2 := testConfig()
	v2["currencies"] = []map[string]any{{"key": "credits", "name": "Credits"}, {"key": "gems", "name": "Gems"}, {"key": "tokens", "name": "Tokens"}}
	v2["editor_layout"] = map[string]any{"quests": map[string]any{}}
	expect(t, f.publish(v2), 201)

	r := expect(t, call(t, f.h, "POST", f.envPath("/configurations/versions/1/rollback"), nil, bearer(f.userToken)), 201).obj(t)
	if r["version"].(float64) != 3 || r["rolled_back_from"].(float64) != 1 {
		t.Fatalf("rollback: %v", r)
	}
	if n := f.count(`SELECT count(*) FROM currency_definitions WHERE project_id=$1 AND currency_key='tokens' AND active`, f.projectID); n != 0 {
		t.Fatal("rolled-back currency is still active")
	}
	if n := f.count(`SELECT count(*) FROM currency_definitions WHERE project_id=$1 AND currency_key='tokens'`, f.projectID); n != 1 {
		t.Fatal("definitions are deactivated, never deleted")
	}
	draft := f.draft()
	if len(draft["currencies"].([]any)) != 2 || draft["editor_layout"] == nil {
		t.Fatalf("draft after rollback: %v", draft)
	}
	if n := f.count(`SELECT count(*) FROM configuration_versions WHERE project_id=$1`, f.projectID); n != 3 {
		t.Fatalf("versions=%d, history must not be rewritten", n)
	}
	expect(t, call(t, f.h, "POST", f.envPath("/configurations/versions/99/rollback"), nil, bearer(f.userToken)), 404)
	expect(t, call(t, f.h, "POST", f.envPath("/configurations/versions/abc/rollback"), nil, bearer(f.userToken)), 404)
}

// OpenAI-compatible providers: any Chat Completions service at a base URL the user
// chooses. The URL is user-supplied, so it is validated (https, no private addresses),
// and a stored key can only be kept for the destination it was saved with.
func TestIntegrationAICompatibleSettings(t *testing.T) {
	f := newFixture(t)
	_, fake := f.withAI(ai.Config{})
	put := func(body map[string]any) apiResult {
		return call(t, f.h, "PUT", "/v1/ai/settings", body, bearer(f.userToken))
	}
	const (
		base   = "https://openrouter.ai/api/v1"
		model  = "nvidia/nemotron-3.5-super-vl:free"
		secret = "sk-or-v1-THISISASECRETKEYVALUE5432"
	)
	field := func(r apiResult, name string) bool { return strings.Contains(r.raw, `"`+name+`"`) }

	// Validation: each rule names its field.
	for name, c := range map[string]struct {
		body  map[string]any
		field string
	}{
		"slash model on openai": {map[string]any{"provider": "openai", "model": model, "api_key": secret}, "model"},
		"no base url":           {map[string]any{"provider": "openai_compatible", "model": model, "api_key": secret}, "base_url"},
		"http":                  {map[string]any{"provider": "openai_compatible", "model": model, "api_key": secret, "base_url": "http://openrouter.ai/api/v1"}, "base_url"},
		"private ip":            {map[string]any{"provider": "openai_compatible", "model": model, "api_key": secret, "base_url": "https://169.254.169.254/latest"}, "base_url"},
		"localhost":             {map[string]any{"provider": "openai_compatible", "model": model, "api_key": secret, "base_url": "https://localhost/v1"}, "base_url"},
		"credentials in url":    {map[string]any{"provider": "openai_compatible", "model": model, "api_key": secret, "base_url": "https://u:p@openrouter.ai/v1"}, "base_url"},
		"base url on openai":    {map[string]any{"provider": "openai", "model": "gpt-5.5", "api_key": secret, "base_url": base}, "base_url"},
		"bad model":             {map[string]any{"provider": "openai_compatible", "model": "bad model!", "api_key": secret, "base_url": base}, "model"},
	} {
		if r := expect(t, put(c.body), 422); !field(r, c.field) {
			t.Errorf("%s: 422 does not name %s: %s", name, c.field, r.raw)
		}
	}

	// A valid compatible setting; the trailing slash is normalised away.
	saved := expect(t, put(map[string]any{"provider": "openai_compatible", "model": model, "api_key": secret, "base_url": base + "/"}), 200)
	got := saved.obj(t)
	if got["provider"] != "openai_compatible" || got["model"] != model || got["base_url"] != base || got["key_last4"] != "5432" || strings.Contains(saved.raw, "THISISASECRET") {
		t.Fatalf("saved settings: %s", saved.raw)
	}
	if providers := got["providers"].([]any); len(providers) != 2 {
		t.Fatalf("providers: %v", providers)
	}

	// The same destination may change its model without re-entering the key.
	expect(t, put(map[string]any{"provider": "openai_compatible", "model": "meta-llama/llama-3.3-70b-instruct", "base_url": base}), 200)
	// A different base URL or provider needs the key again: otherwise a hijacked session
	// could aim a stored key at a server it controls.
	for name, body := range map[string]map[string]any{
		"new base url": {"provider": "openai_compatible", "model": model, "base_url": "https://attacker.example/v1"},
		"to openai":    {"provider": "openai", "model": "gpt-5.5"},
	} {
		r := expect(t, put(body), 422)
		if !field(r, "api_key") || !strings.Contains(r.raw, "changes") {
			t.Errorf("%s: %s", name, r.raw)
		}
	}
	if got := expect(t, call(t, f.h, "GET", "/v1/ai/settings", nil, bearer(f.userToken)), 200).obj(t); got["base_url"] != base || got["model"] != "meta-llama/llama-3.3-70b-instruct" {
		t.Fatalf("a refused change altered the settings: %v", got)
	}

	// Switching back to OpenAI with a key clears the base URL (null in the API).
	expect(t, put(map[string]any{"provider": "openai", "model": "gpt-5.5", "api_key": secret}), 200)
	if got := expect(t, call(t, f.h, "GET", "/v1/ai/settings", nil, bearer(f.userToken)), 200).obj(t); got["base_url"] != nil || got["provider"] != "openai" {
		t.Fatalf("after switching to openai: %v", got)
	}

	// Saving checks the key and model with the provider first. If the provider refuses,
	// nothing is saved, so a typo can never replace a working setup.
	fake.checkErr = &ai.ProviderError{Code: ai.CodeKeyRejected, Message: "rejected"}
	r := expect(t, put(map[string]any{"provider": "openai_compatible", "model": model, "api_key": "sk-or-v1-TYPOTYPOTYPOTYPO0000", "base_url": base}), 422)
	if r.Error.Code != ai.CodeKeyRejected {
		t.Fatalf("code=%s", r.Error.Code)
	}
	expect(t, put(map[string]any{"provider": "openai", "model": "gpt-5.5-typo"}), 422) // a model-only change is checked too
	if got := expect(t, call(t, f.h, "GET", "/v1/ai/settings", nil, bearer(f.userToken)), 200).obj(t); got["provider"] != "openai" || got["model"] != "gpt-5.5" || got["key_last4"] != "5432" {
		t.Fatalf("a refused save changed the stored settings: %v", got)
	}
	fake.checkErr = &ai.ProviderError{Code: ai.CodeModelNotFound, Message: "no model"}
	if r = expect(t, put(map[string]any{"provider": "openai", "model": "gpt-9"}), 422); r.Error.Code != ai.CodeModelNotFound {
		t.Fatalf("code=%s", r.Error.Code)
	}
	// With no setting stored yet, a refused first save leaves nothing behind.
	fake.checkErr = nil
	expect(t, call(t, f.h, "DELETE", "/v1/ai/settings", nil, bearer(f.userToken)), 200)
	fake.checkErr = &ai.ProviderError{Code: ai.CodeKeyRejected, Message: "rejected"}
	expect(t, put(map[string]any{"provider": "openai", "model": "gpt-5.5", "api_key": secret}), 422)
	if n := f.count(`SELECT count(*) FROM ai_provider_settings`); n != 0 {
		t.Fatalf("a refused first save stored %d rows", n)
	}
}

// An operator running a local model server opts in to http and private addresses.
func TestIntegrationAIAllowPrivateURLs(t *testing.T) {
	f := newFixture(t)
	f.withAI(ai.Config{AllowPrivateURLs: true})
	r := call(t, f.h, "PUT", "/v1/ai/settings", map[string]any{"provider": "openai_compatible", "model": "llama3.1", "api_key": "ollama-local-key-000000", "base_url": "http://host.docker.internal:11434/v1"}, bearer(f.userToken))
	if got := expect(t, r, 200).obj(t); got["base_url"] != "http://host.docker.internal:11434/v1" {
		t.Fatalf("settings: %v", got)
	}
	// Credentials in the URL stay forbidden even then.
	expect(t, call(t, f.h, "PUT", "/v1/ai/settings", map[string]any{"provider": "openai_compatible", "model": "llama3.1", "api_key": "ollama-local-key-000000", "base_url": "http://u:p@localhost:11434/v1"}, bearer(f.userToken)), 422)
}
