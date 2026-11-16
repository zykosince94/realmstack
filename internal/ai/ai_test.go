package ai

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"realmstack-backend/internal/catalog"
	"realmstack-backend/internal/goals"
)

var testKey = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

func TestSealerRoundTripAndTamper(t *testing.T) {
	s, err := NewSealer(testKey)
	if err != nil {
		t.Fatal(err)
	}
	sealed, nonce, err := s.Seal("sk-test-secret-value-1234", []byte("user-1"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(sealed), "secret") {
		t.Fatal("ciphertext contains the plaintext")
	}
	if got, err := s.Open(sealed, nonce, []byte("user-1")); err != nil || got != "sk-test-secret-value-1234" {
		t.Fatalf("open: %q %v", got, err)
	}
	// Bound to the user: the same ciphertext does not open for another user.
	if _, err := s.Open(sealed, nonce, []byte("user-2")); err == nil {
		t.Fatal("ciphertext opened for another user")
	}
	sealed[0] ^= 0xff
	if _, err := s.Open(sealed, nonce, []byte("user-1")); err == nil {
		t.Fatal("tampered ciphertext opened")
	}
	// A fresh nonce every time: sealing the same key twice gives different bytes.
	again, nonce2, _ := s.Seal("sk-test-secret-value-1234", []byte("user-1"))
	if string(again) == string(sealed) || string(nonce2) == string(nonce) {
		t.Fatal("nonce reused")
	}
	for _, bad := range []string{"not base64!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := NewSealer(bad); err == nil {
			t.Fatalf("accepted bad key %q", bad)
		}
	}
}

// walk visits every object schema in a schema tree.
func walk(t *testing.T, path string, node any, visit func(path string, obj map[string]any)) {
	switch n := node.(type) {
	case map[string]any:
		if n["type"] == "object" {
			visit(path, n)
		}
		for k, v := range n {
			walk(t, path+"."+k, v, visit)
		}
	case []any:
		for i, v := range n {
			walk(t, path+"["+string(rune('0'+i))+"]", v, visit)
		}
	}
}

// OpenAI strict mode rejects a schema where an object allows extra properties or does
// not list every property as required; check that for every operation.
func TestSchemasAreStrict(t *testing.T) {
	for _, op := range Operations {
		schema := schemaFor(op)
		if schema == nil {
			t.Fatalf("%s: no schema", op)
		}
		objects := 0
		walk(t, op, schema, func(path string, obj map[string]any) {
			objects++
			if obj["additionalProperties"] != false {
				t.Errorf("%s: additionalProperties is not false", path)
			}
			props := obj["properties"].(map[string]any)
			required := obj["required"].([]string)
			if len(required) != len(props) {
				t.Errorf("%s: %d properties but %d required", path, len(props), len(required))
			}
			for _, r := range required {
				if _, ok := props[r]; !ok {
					t.Errorf("%s: required %q is not a property", path, r)
				}
			}
		})
		if objects == 0 {
			t.Fatalf("%s: no objects found", op)
		}
	}
}

func TestDecodeOperations(t *testing.T) {
	items, err := decode(OpItems, `{"items":[{"key":"sword","name":"Sword","category":"weapon","stackable":false,"max_stack":1,"slot":"weapon"},
		{"key":"herb","name":"Herb","category":"material","stackable":true,"max_stack":50,"slot":null}],"notes":"Two items."}`)
	if err != nil {
		t.Fatal(err)
	}
	if items.Proposal.Items[0].Metadata["slot"] != "weapon" || len(items.Proposal.Items[1].Metadata) != 0 || items.Notes != "Two items." {
		t.Fatalf("items: %+v", items)
	}
	// Unknown fields are rejected even if the provider let them through.
	if _, err := decode(OpItems, `{"items":[],"notes":"","script":"rm -rf /"}`); err == nil {
		t.Fatal("unknown field accepted")
	}
	quests, err := decode(OpQuests, `{"stats":[{"key":"entered_mine","name":"Entered","kind":"flag"}],"notes":"A quest.",
		"quests":[{"key":"q","name":"Q","description":"","chain":null,"requires_quests":[],"requires_achievements":[],"repeat":"none","auto_accept":false,
		"claimable_by":"player","available_from":null,"available_until":null,"rewards":{"items":[],"currencies":[],"xp":10},
		"stages":[{"key":"s1","name":"Enter","conditions":[{"type":"flag","flag_key":"entered_mine"}],"rewards":{"items":[],"currencies":[],"xp":0}},
		          {"key":"s2","name":"Fight","conditions":[{"type":"stat","stat_key":"kills","target":3},{"type":"item_granted","item_key":null,"target":1}],"rewards":{"items":[],"currencies":[],"xp":0}}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	q := quests.Proposal.Quests[0]
	if q.Chain != "" || len(q.Stages) != 2 || q.Stages[1].Conditions[1].ItemKey != "" || q.Stages[1].Conditions[1].Type != goals.ItemGranted {
		t.Fatalf("quest: %+v", q)
	}
	if !strings.Contains(quests.Notes, "counters kills") || !strings.Contains(quests.Notes, "flags entered_mine") {
		t.Fatalf("notes should list what the game server must report: %q", quests.Notes)
	}
	economy, err := decode(OpEconomy, `{"currencies":[{"key":"gold","name":"Gold"}],"price_suggestions":[{"item_key":"sword","currency_key":"gold","price":120,"rationale":"mid-tier"}],"notes":"Balanced."}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(economy.Notes, "sword: 120 gold") || len(economy.Proposal.Currencies) != 1 {
		t.Fatalf("economy: %+v", economy)
	}
	if _, err := decode(OpExplain, `{"answer":"It unlocks after the tutorial."}`); err != nil {
		t.Fatal(err)
	}
}

func TestContextIsAllowListed(t *testing.T) {
	cfg := catalog.Config{
		Items: []catalog.ItemConfig{{Key: "sword", Name: "Sword", MaxStack: 1, Metadata: map[string]any{"slot": "weapon", "internal_note": "admin@studio.test"}}},
		Achievements: []catalog.AchievementConfig{{Key: "a", Name: "A", Description: "secret description",
			Conditions: []goals.Condition{{Type: goals.XPTotal, Target: 10}}}},
		Progression: []int64{100, 300},
	}
	got, err := buildContext(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"admin@studio.test", "internal_note", "secret description"} {
		if strings.Contains(got, leak) {
			t.Errorf("context leaked %q: %s", leak, got)
		}
	}
	for _, want := range []string{`"slot":"weapon"`, `"max_level":3`, `"key":"sword"`} {
		if !strings.Contains(got, want) {
			t.Errorf("context missing %s: %s", want, got)
		}
	}
	huge := catalog.Config{}
	for i := 0; i < 3000; i++ {
		huge.Items = append(huge.Items, catalog.ItemConfig{Key: strings.Repeat("k", 60), Name: strings.Repeat("n", 90), MaxStack: 1})
	}
	if _, err := buildContext(huge); err == nil {
		t.Fatal("oversized context accepted (it must fail, not truncate)")
	}
}

func TestParamsDefaultsAndLimits(t *testing.T) {
	p := Params{}
	if errs := p.normalize(OpQuests); len(errs) != 0 || p.Count != 3 || p.StageCount != 3 || p.QuestType != "story" {
		t.Fatalf("quest defaults: %+v %v", p, errs)
	}
	p = Params{Count: 99, Pacing: "warp"}
	if errs := p.normalize(OpItems); len(errs) != 1 || errs[0].Field != "params.count" {
		t.Fatalf("items limits: %v", errs)
	}
	p = Params{Pacing: "warp"}
	if errs := p.normalize(OpProgression); len(errs) != 1 || errs[0].Field != "params.pacing" {
		t.Fatalf("progression: %v", errs)
	}
	if errs := (&Params{}).normalize(OpExplain); len(errs) != 1 || errs[0].Field != "params.question" {
		t.Fatalf("explain: %v", errs)
	}
	if errs := (&Params{}).normalize("delete_everything"); len(errs) != 1 || errs[0].Field != "operation" {
		t.Fatalf("unknown op: %v", errs)
	}
}

func TestTranslateProviderErrors(t *testing.T) {
	ctx := context.Background()
	for status, code := range map[int]string{401: CodeKeyRejected, 403: CodeKeyRejected, 404: CodeModelNotFound, 429: CodeRateLimited, 500: CodeProviderDown, 503: CodeProviderDown, 400: CodeRequestRejected} {
		resp := &http.Response{Header: http.Header{"Retry-After": []string{"7"}}}
		err := translate(ctx, &openai.Error{StatusCode: status, Response: resp})
		var pe *ProviderError
		if !errors.As(err, &pe) || pe.Code != code {
			t.Errorf("status %d: %v, want %s", status, err, code)
		}
		if status == 429 && pe.RetryAfter.Seconds() != 7 {
			t.Errorf("retry-after: %v", pe.RetryAfter)
		}
	}
	timeout, cancel := context.WithTimeout(ctx, 0)
	defer cancel()
	<-timeout.Done()
	if err := translate(timeout, errors.New("boom")); err.(*ProviderError).Code != CodeTimeout {
		t.Fatalf("timeout: %v", err)
	}
	if err := translate(ctx, errors.New("dial tcp: refused")); err.(*ProviderError).Code != CodeProviderDown {
		t.Fatalf("network: %v", err)
	}
}
