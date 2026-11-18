package httpapi

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"realmstack-backend/internal/ai"
	"realmstack-backend/internal/demo"
)

// The demo's AI assistant history covers every operation and outcome, and its "valid"
// proposals are real: each can be applied to the demo draft, and the result publishes.
func TestIntegrationDemoAIHistory(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := demo.Seed(ctx, db, nil); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, db: db}
	got := strings.Join(f.snapshot(`SELECT operation||':'||status FROM ai_generations WHERE params ? 'demo_seed' ORDER BY created_at DESC`), ",")
	want := "items:valid,economy:valid,progression:valid,achievements:valid,quests:valid,explain:answered,tests:answered,items:invalid,quests:failed,items:discarded"
	if got != want {
		t.Fatalf("demo history = %s\nwant %s", got, want)
	}
	if n := f.count(`SELECT count(*) FROM ai_generations WHERE status='invalid' AND jsonb_array_length(errors) > 0`); n != 1 {
		t.Fatal("the invalid demo proposal carries no field errors")
	}
	// Seeding again (every API restart) adds nothing, and a developer's own generation
	// neither blocks the demo history nor is touched by it.
	if _, err := db.Exec(ctx, `INSERT INTO ai_generations(project_id,environment_id,user_id,operation,provider,model,status)
		SELECT project_id,environment_id,user_id,'items','openai','gpt-5.5','failed' FROM ai_generations LIMIT 1`); err != nil {
		t.Fatal(err)
	}
	if err := demo.SeedAIGenerations(ctx, db); err != nil {
		t.Fatal(err)
	}
	if n := f.count(`SELECT count(*) FROM ai_generations WHERE params ? 'demo_seed'`); n != 10 {
		t.Fatalf("reseeding duplicated the history: %d rows", n)
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := New(db, Config{JWTSecret: strings.Repeat("k", 40), AccessTTL: time.Minute, RefreshTTL: time.Hour, BodyLimit: 1 << 20, AuthRateLimit: 10000, AI: ai.Config{EncryptionKey: aiTestKey}}, logger)
	h := srv.Handler()
	token := expect(t, call(t, h, "POST", "/v1/auth/login", map[string]any{"email": demo.DemoAdminEmail, "password": demo.DemoAdminPassword}, nil), 200).obj(t)["access_token"].(string)
	var projectID, envID string
	if err := db.QueryRow(ctx, `SELECT project_id::text,environment_id::text FROM ai_generations LIMIT 1`).Scan(&projectID, &envID); err != nil {
		t.Fatal(err)
	}
	base := "/v1/projects/" + projectID + "/environments/" + envID
	var list []map[string]any
	expect(t, call(t, h, "GET", base+"/ai/generations", nil, bearer(token)), 200).into(t, &list)
	applied := 0
	for _, g := range list {
		if errs, ok := g["errors"].([]any); !ok {
			t.Fatalf("errors is not an array: %v", g)
		} else if g["status"] == "valid" && len(errs) != 0 {
			t.Fatalf("valid proposal with errors: %v", g)
		}
		if g["status"] != "valid" {
			continue
		}
		expect(t, call(t, h, "POST", base+"/ai/generations/"+g["id"].(string)+"/apply", nil, bearer(token)), 200)
		applied++
	}
	if applied != 5 {
		t.Fatalf("applied %d valid proposals, want 5", applied)
	}
	expect(t, call(t, h, "POST", base+"/configurations/publish", nil, bearer(token)), 201)
	for _, key := range []string{"ember_staff", "guild_marks", "boss_hunter", "herbalists_request", "bosses_defeated"} {
		if f.count(`SELECT count(*) FROM configuration_versions WHERE version=2 AND config::text LIKE '%"'||$1||'"%'`, key) != 1 {
			t.Errorf("published configuration lacks %s", key)
		}
	}
}
