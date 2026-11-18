package demo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"realmstack-backend/internal/catalog"
	"realmstack-backend/internal/goals"
	"realmstack-backend/internal/platform"
)

// demoGeneration is one AI assistant history row for the demo. Proposals go through the
// same merge and validation as a real generation (catalog.MergeProposal + CheckConfig),
// so a "valid" demo row really can be applied and published, and the invalid one shows
// the field errors validation actually produces.
type demoGeneration struct {
	op, policy, status string // status is the intended outcome; validation decides valid/invalid
	params             map[string]any
	proposal           catalog.Proposal
	answer             any
	notes              string
	errorCode, errMsg  string
	in, out            int64
	ago                time.Duration
}

// SeedAIGenerations gives the demo's AI assistant a history covering every operation
// and outcome: valid proposals for items, economy, progression, achievements and
// quests, an answered question and test ideas, an invalid proposal, a failed call and a
// discarded proposal. Seeded rows carry params.demo_seed, and seeding runs only while no
// such row exists: once per environment, alongside (never instead of) a developer's own
// generations, which it never touches.
func SeedAIGenerations(ctx context.Context, db *pgxpool.Pool) error {
	var projectID, envID, userID string
	err := db.QueryRow(ctx, `SELECT p.id::text,e.id::text,u.id::text FROM users u JOIN projects p ON p.owner_user_id=u.id AND p.slug='realmstack-demo'
		JOIN environments e ON e.project_id=p.id AND e.name='development' WHERE u.email=$1`, DemoAdminEmail).Scan(&projectID, &envID, &userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var exists bool
	if err = db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ai_generations WHERE project_id=$1 AND environment_id=$2 AND params ? 'demo_seed')`, projectID, envID).Scan(&exists); err != nil || exists {
		return err
	}
	var draft []byte
	if err = db.QueryRow(ctx, `SELECT config FROM configuration_drafts WHERE project_id=$1 AND environment_id=$2`, projectID, envID).Scan(&draft); err != nil {
		return err
	}
	sum := sha256.Sum256(draft)
	draftHash := hex.EncodeToString(sum[:])
	now := time.Now()
	for _, g := range demoGenerations() {
		created := now.Add(-g.ago)
		finished := created.Add(time.Duration(8+g.out/200) * time.Second)
		status, errs, output := g.status, []platform.FieldError{}, []byte(nil)
		var merged []byte
		if g.status != "failed" {
			out, _ := json.Marshal(map[string]any{"proposal": g.proposal, "answer": g.answer})
			output = out
		}
		if g.status == "valid" || g.status == "invalid" || g.status == "discarded" {
			var conflicts, checkErrs []platform.FieldError
			if merged, conflicts, err = catalog.MergeProposal(draft, g.proposal, g.policy == "replace"); err != nil {
				return err
			}
			if _, checkErrs, err = catalog.CheckConfig(ctx, db, projectID, envID, merged); err != nil {
				return err
			}
			errs = append(append(errs, conflicts...), checkErrs...)
			if g.status != "discarded" {
				status = "valid"
				if len(errs) > 0 {
					status = "invalid"
				}
			}
			// On a fresh demo database every "valid" row validates (a test checks it). On an
			// older database the draft may have moved on; then the row is honestly stored
			// as invalid rather than failing startup.
		}
		g.params["demo_seed"] = true
		params, _ := json.Marshal(g.params)
		errsJSON, _ := json.Marshal(errs)
		if _, err = db.Exec(ctx, `INSERT INTO ai_generations(project_id,environment_id,user_id,operation,params,merge_policy,provider,model,status,
				output,merged,errors,notes,error_code,error_message,input_tokens,output_tokens,base_draft_hash,created_at,finished_at)
			VALUES($1,$2,$3,$4,$5,$6,'openai','gpt-5.5',$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
			projectID, envID, userID, g.op, params, g.policy, status, output, merged, errsJSON, g.notes, g.errorCode, g.errMsg,
			g.in, g.out, draftHash, created, finished); err != nil {
			return err
		}
	}
	return nil
}

func demoGenerations() []demoGeneration {
	gems := func(n int64) goals.Rewards {
		return goals.Rewards{Currencies: []goals.CurrencyReward{{CurrencyKey: "gems", Amount: n}}}
	}
	return []demoGeneration{
		{op: "items", policy: "add", status: "valid", ago: 4 * time.Minute, in: 1310, out: 420,
			params: map[string]any{"brief": "Starter gear for a mage who explores the old mine", "count": 3},
			proposal: catalog.Proposal{Items: []catalog.ItemConfig{
				{Key: "ember_staff", Name: "Ember Staff", Category: "weapon", MaxStack: 1, Metadata: map[string]any{"slot": "weapon"}},
				{Key: "travelers_cloak", Name: "Traveler's Cloak", Category: "armor", MaxStack: 1, Metadata: map[string]any{"slot": "body"}},
				{Key: "mana_crystal", Name: "Mana Crystal", Category: "material", Stackable: true, MaxStack: 250, Metadata: map[string]any{}},
			}},
			notes: "Two equippable pieces for an early mage build and a stackable crafting material that drops in the mine."},
		{op: "economy", policy: "add", status: "valid", ago: 11 * time.Minute, in: 1450, out: 510,
			params:   map[string]any{"brief": "A reputation currency earned from guild contracts", "count": 1},
			proposal: catalog.Proposal{Currencies: []catalog.CurrencyConfig{{Key: "guild_marks", Name: "Guild Marks"}}},
			answer: []map[string]any{
				{"item_key": "ember_staff", "currency_key": "guild_marks", "price": 40, "rationale": "About four daily contracts."},
				{"item_key": "health_potion", "currency_key": "credits", "price": 25, "rationale": "Cheap enough to buy before every dungeon run."},
			},
			notes: "Guild Marks are earned only from contracts, so they cannot be farmed with credits.\nSuggested prices (advisory; not part of the configuration):\n- ember_staff: 40 guild_marks (About four daily contracts.)\n- health_potion: 25 credits (Cheap enough to buy before every dungeon run.)"},
		{op: "progression", policy: "add", status: "valid", ago: 26 * time.Minute, in: 980, out: 190,
			params:   map[string]any{"brief": "Extend the curve to level 9 without changing the first levels", "levels": 9, "pacing": "steady"},
			proposal: catalog.Proposal{Progression: []int64{100, 300, 700, 1500, 2600, 4000, 6000, 8500}},
			notes:    "Keeps the existing thresholds and grows each later level by roughly 40%, so a player levels about once per play session after level 5."},
		{op: "achievements", policy: "add", status: "valid", ago: 47 * time.Minute, in: 1720, out: 640,
			params: map[string]any{"brief": "Achievements for boss fights and for collecting loot", "count": 2},
			proposal: catalog.Proposal{
				Stats: []catalog.StatConfig{{Key: "bosses_defeated", Name: "Bosses defeated", Kind: "counter"}},
				Achievements: []catalog.AchievementConfig{
					{Key: "boss_hunter", Name: "Boss Hunter", Description: "Defeat 3 bosses.",
						Conditions: []goals.Condition{{Type: goals.Stat, StatKey: "bosses_defeated", Target: 3}}, Rewards: gems(15)},
					{Key: "pack_rat", Name: "Pack Rat", Description: "Receive 25 items of any kind.",
						Conditions: []goals.Condition{{Type: goals.ItemGranted, Target: 25}}, Rewards: goals.Rewards{XP: 75}},
				},
			},
			notes: "Adds a bosses_defeated counter for the first achievement; the second reuses item grants, so it needs no new stat."},
		{op: "quests", policy: "add", status: "valid", ago: 75 * time.Minute, in: 2210, out: 880,
			params: map[string]any{"brief": "A side quest for an herbalist after the first story quest", "count": 1, "quest_type": "side", "stage_count": 2},
			proposal: catalog.Proposal{
				Stats: []catalog.StatConfig{
					{Key: "herbs_gathered", Name: "Herbs gathered", Kind: "counter"},
					{Key: "returned_to_herbalist", Name: "Returned to the herbalist", Kind: "flag"},
				},
				Quests: []catalog.QuestConfig{{Key: "herbalists_request", Name: "The Herbalist's Request", Description: "Gather herbs for the village herbalist.",
					Chain: "Side Quests", RequiresQuests: []string{"the_awakening"},
					Stages: []catalog.QuestStageConfig{
						{Key: "gather", Name: "Gather 5 herbs", Conditions: []goals.Condition{{Type: goals.Stat, StatKey: "herbs_gathered", Target: 5}}},
						{Key: "deliver", Name: "Return to the herbalist", Conditions: []goals.Condition{{Type: goals.Flag, FlagKey: "returned_to_herbalist"}},
							Rewards: goals.Rewards{Items: []goals.ItemReward{{ItemKey: "health_potion", Quantity: 3}}}},
					},
					Rewards: goals.Rewards{Currencies: []goals.CurrencyReward{{CurrencyKey: "credits", Amount: 150}}, XP: 120}}},
			},
			notes: "Unlocks after The Awakening. Delivering is a flag because it has to happen after gathering, and counted conditions cannot express order."},
		{op: "explain", policy: "add", status: "answered", ago: 2 * time.Hour, in: 1890, out: 160,
			params: map[string]any{"question": "What does a new player need to do to start The Mentor's Trial?"},
			answer: "The Mentor's Trial needs two things. First, finish the story so far: The Awakening (meet the mentor), then Into the Mine (enter the mine, defeat 5 enemies, return to the mentor). Second, earn the First Blood achievement by defeating one enemy, which happens naturally during Into the Mine. Once both are done the quest becomes available; it is not auto-accepted, so the player accepts it and then defeats 10 enemies."},
		{op: "tests", policy: "add", status: "answered", ago: 3 * time.Hour, in: 2050, out: 700,
			params: map[string]any{"brief": "The daily hunt and the weekly trader quests"},
			answer: []map[string]any{
				{"name": "Daily hunt resets at UTC midnight", "kind": "positive", "description": "A completed and claimed daily hunt becomes available again the next UTC day.",
					"steps": []string{"Defeat 3 enemies", "Claim the Daily Hunt reward", "Advance the clock past 00:00 UTC", "Open the quest log"}, "expected": "Daily Hunt is active again with 0/3 progress."},
				{"name": "Weekly trader cannot be claimed twice", "kind": "negative", "description": "A second claim in the same week is refused.",
					"steps": []string{"Spend 200 credits", "Claim Weekly Trader", "Claim it again"}, "expected": "The second claim returns 409 ALREADY_CLAIMED and pays nothing."},
				{"name": "Spending before acceptance does not count", "kind": "negative", "description": "Weekly Trader is not auto-accepted, so credits spent before accepting it do not count.",
					"steps": []string{"Spend 200 credits", "Accept Weekly Trader", "Open the quest log"}, "expected": "Progress is 0/200."},
			}},
		{op: "items", policy: "add", status: "invalid", ago: 5 * time.Hour, in: 1200, out: 300,
			params: map[string]any{"brief": "Better potions", "count": 2},
			proposal: catalog.Proposal{Items: []catalog.ItemConfig{
				{Key: "health_potion", Name: "Greater Health Potion", Category: "consumable", Stackable: true, MaxStack: 99, Metadata: map[string]any{}},
				{Key: "elixir", Name: "Elixir", Category: "consumable", Stackable: false, MaxStack: 5, Metadata: map[string]any{}},
			}},
			notes: "A stronger potion and a rare elixir."},
		{op: "quests", policy: "add", status: "failed", ago: 6 * time.Hour, in: 0, out: 0,
			params:    map[string]any{"brief": "A weekly event quest for the winter festival", "count": 2, "quest_type": "event"},
			errorCode: "AI_RATE_LIMITED", errMsg: "The provider's rate limit or quota for this key was reached."},
		{op: "items", policy: "add", status: "discarded", ago: 26 * time.Hour, in: 1150, out: 260,
			params:   map[string]any{"brief": "Joke items for April Fools", "count": 1},
			proposal: catalog.Proposal{Items: []catalog.ItemConfig{{Key: "rubber_chicken", Name: "Rubber Chicken", Category: "weapon", MaxStack: 1, Metadata: map[string]any{"slot": "weapon"}}}},
			notes:    "A harmless novelty weapon."},
	}
}
