// Package demo seeds the local demo project. It runs only when APP_ENV=local and
// DEMO_MODE=true; every value here is public demo data, never a production secret.
package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"realmstack-backend/internal/catalog"
	"realmstack-backend/internal/goals"
	"realmstack-backend/internal/platform"
)

const (
	DemoAdminEmail        = "admin@realmstack.local"
	DemoAdminPassword     = "RealmStackLocal!2026"
	DemoPlayerEmail       = "player@realmstack.local"
	DemoPlayerPassword    = "PlayerLocal!2026"
	DemoPublicEnvironment = "pub_realmstack_demo_dev"
	DemoServerCredential  = "rs_demo.realmstack-local-server-secret"
)

// Seed creates or refreshes the demo accounts, project and published catalog. It is
// idempotent: it never overwrites the draft and grants starter items/currency once.
func Seed(ctx context.Context, db *pgxpool.Pool, logger *slog.Logger) error {
	adminHash, err := platform.HashPassword(DemoAdminPassword)
	if err != nil {
		return err
	}
	playerHash, err := platform.HashPassword(DemoPlayerPassword)
	if err != nil {
		return err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var userID, projectID, envID string
	err = tx.QueryRow(ctx, `INSERT INTO users(email,password_hash) VALUES($1,$2) ON CONFLICT(email) DO UPDATE SET password_hash=EXCLUDED.password_hash RETURNING id::text`, DemoAdminEmail, string(adminHash)).Scan(&userID)
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `INSERT INTO projects(owner_user_id,name,slug) VALUES($1,'RealmStack Demo','realmstack-demo') ON CONFLICT(owner_user_id,slug) DO UPDATE SET name=EXCLUDED.name RETURNING id::text`, userID).Scan(&projectID)
	if err != nil {
		return err
	}
	err = tx.QueryRow(ctx, `INSERT INTO environments(project_id,name,public_id) VALUES($1,'development',$2) ON CONFLICT(project_id,name) DO UPDATE SET public_id=EXCLUDED.public_id RETURNING id::text`, projectID, DemoPublicEnvironment).Scan(&envID)
	if err != nil {
		return err
	}
	// Seed the draft only once; restarting the API must not discard the developer's edits.
	_, err = tx.Exec(ctx, `INSERT INTO configuration_drafts(project_id,environment_id,config) VALUES($1,$2,$3) ON CONFLICT(project_id,environment_id) DO NOTHING`, projectID, envID, demoConfiguration())
	if err != nil {
		return err
	}
	var versions int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM configuration_versions WHERE project_id=$1 AND environment_id=$2`, projectID, envID).Scan(&versions)
	if err != nil {
		return err
	}
	if versions == 0 {
		cfg := Configuration()
		if errs := catalog.ValidateConfig(cfg); len(errs) > 0 {
			return fmt.Errorf("demo configuration is invalid: %v", errs)
		}
		var versionID string
		err = tx.QueryRow(ctx, `INSERT INTO configuration_versions(project_id,environment_id,version,config,created_by) VALUES($1,$2,1,$3,$4) RETURNING id::text`, projectID, envID, demoConfiguration(), userID).Scan(&versionID)
		if err != nil {
			return err
		}
		if _, _, err = catalog.ApplyRuntimeConfig(ctx, tx, projectID, envID, versionID, cfg, time.Now()); err != nil {
			return err
		}
	}
	var credentialID string
	err = tx.QueryRow(ctx, `SELECT id::text FROM server_credentials WHERE key_prefix='rs_demo' AND project_id=$1 AND environment_id=$2`, projectID, envID).Scan(&credentialID)
	if err != nil {
		_, err = tx.Exec(ctx, `INSERT INTO server_credentials(project_id,environment_id,key_prefix,secret_hash) VALUES($1,$2,'rs_demo',$3)`, projectID, envID, platform.Digest(strings.SplitN(DemoServerCredential, ".", 2)[1]))
		if err != nil {
			return err
		}
	}
	var playerID string
	err = tx.QueryRow(ctx, `INSERT INTO players(project_id,environment_id,email,password_hash,display_name) VALUES($1,$2,$3,$4,'Demo Player') ON CONFLICT(project_id,environment_id,email) WHERE email IS NOT NULL DO UPDATE SET password_hash=EXCLUDED.password_hash RETURNING id::text`, projectID, envID, DemoPlayerEmail, string(playerHash)).Scan(&playerID)
	if err != nil {
		return err
	}
	// Starting inventory and balance are granted once, each with a matching ledger row
	// and goal total, so the demo player's balances always reconcile with
	// economy_transactions and player_goal_totals matches a rebuild.
	_, err = tx.Exec(ctx, `WITH granted AS (
		INSERT INTO inventory(player_id,project_id,environment_id,item_id,quantity)
		SELECT $1,$2,$3,id,1 FROM item_definitions WHERE project_id=$2 AND environment_id=$3 AND item_key='health_potion'
		ON CONFLICT(player_id,item_id) DO NOTHING RETURNING item_id),
	ledger AS (INSERT INTO economy_transactions(player_id,project_id,environment_id,item_id,operation,quantity,reason,reference)
		SELECT $1,$2,$3,item_id,'grant',1,'demo_seed','seed' FROM granted RETURNING 1)
	INSERT INTO player_goal_totals(player_id,project_id,environment_id,metric,total)
	SELECT $1,$2,$3,m,1 FROM ledger, unnest(ARRAY['grant:','grant:health_potion']) AS m
	ON CONFLICT(player_id,metric) DO UPDATE SET total=player_goal_totals.total+EXCLUDED.total`, playerID, projectID, envID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `WITH credited AS (
		INSERT INTO balances(player_id,project_id,environment_id,currency_id,amount)
		SELECT $1,$2,$3,id,5000 FROM currency_definitions WHERE project_id=$2 AND environment_id=$3 AND currency_key='credits'
		ON CONFLICT(player_id,currency_id) DO NOTHING RETURNING currency_id),
	ledger AS (INSERT INTO economy_transactions(player_id,project_id,environment_id,currency_id,operation,quantity,balance_after,reason,reference)
		SELECT $1,$2,$3,currency_id,'credit',5000,5000,'demo_seed','seed' FROM credited RETURNING 1)
	INSERT INTO player_goal_totals(player_id,project_id,environment_id,metric,total)
	SELECT $1,$2,$3,'credit:credits',5000 FROM ledger
	ON CONFLICT(player_id,metric) DO UPDATE SET total=player_goal_totals.total+EXCLUDED.total`, playerID, projectID, envID)
	if err != nil {
		return err
	}
	// A cloud save to look at on the Players screen's Saves tab. Inserted once: the demo
	// player's game (or an admin) may have changed it since. Size = the compacted JSON.
	save := `{"chapter":2,"checkpoint":"old_mine_entrance","playtime_minutes":95,"settings":{"difficulty":"normal","subtitles":true}}`
	_, err = tx.Exec(ctx, `INSERT INTO player_saves(player_id,project_id,environment_id,slot,data,version,size_bytes,created_at,updated_at)
		VALUES($1,$2,$3,'slot1',$4,1,$5,now(),now()) ON CONFLICT(player_id,slot) DO NOTHING`, playerID, projectID, envID, save, len(save))
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	// The AI assistant's demo history (once per environment; see ai.go).
	if err = SeedAIGenerations(ctx, db); err != nil {
		return fmt.Errorf("seed AI demo generations: %w", err)
	}
	if logger != nil {
		logger.Warn("LOCAL DEMO ENABLED: seeded demo accounts and server credential", "admin_email", DemoAdminEmail, "player_email", DemoPlayerEmail, "public_environment", DemoPublicEnvironment)
	}
	return nil
}

func demoConfiguration() []byte {
	b, _ := json.Marshal(Configuration())
	return b
}

// Configuration is the demo catalog, also used by tests as a known-valid config.
func Configuration() catalog.Config {
	return catalog.Config{
		Items: []catalog.ItemConfig{
			{Key: "health_potion", Name: "Health Potion", Category: "consumable", Stackable: true, MaxStack: 99, Metadata: map[string]any{"healing": 50}},
			{Key: "iron_sword", Name: "Iron Sword", Category: "weapon", Stackable: false, MaxStack: 1, Metadata: map[string]any{"slot": "weapon", "attack": 12}},
		},
		Currencies:   []catalog.CurrencyConfig{{Key: "credits", Name: "Credits"}, {Key: "gems", Name: "Gems"}},
		Progression:  []int64{100, 300, 700, 1500},
		Leaderboards: []catalog.LeaderboardConfig{{Key: "global", Direction: "higher"}},
		Stats: []catalog.StatConfig{
			{Key: "enemies_defeated", Name: "Enemies defeated", Kind: "counter"},
			{Key: "met_the_mentor", Name: "Met the mentor", Kind: "flag"},
			{Key: "entered_the_mine", Name: "Entered the old mine", Kind: "flag"},
			{Key: "returned_to_mentor", Name: "Returned to the mentor", Kind: "flag"},
		},
		Achievements: []catalog.AchievementConfig{
			{Key: "first_blood", Name: "First Blood", Description: "Defeat your first enemy.",
				Conditions: []goals.Condition{{Type: goals.Stat, StatKey: "enemies_defeated", Target: 1}},
				Rewards:    goals.Rewards{Currencies: []goals.CurrencyReward{{CurrencyKey: "credits", Amount: 100}}}},
			{Key: "veteran", Name: "Veteran", Description: "Reach level 3 and defeat 25 enemies.",
				Conditions: []goals.Condition{{Type: goals.LevelReached, Target: 3}, {Type: goals.Stat, StatKey: "enemies_defeated", Target: 25}},
				Rewards:    goals.Rewards{Currencies: []goals.CurrencyReward{{CurrencyKey: "gems", Amount: 5}}, XP: 100}},
			{Key: "high_roller", Name: "High Roller", Description: "Score at least 10,000 on the global leaderboard.", ClaimableBy: "server",
				Conditions: []goals.Condition{{Type: goals.LeaderboardScore, LeaderboardKey: "global", Target: 10000}},
				Rewards:    goals.Rewards{Items: []goals.ItemReward{{ItemKey: "health_potion", Quantity: 3}}}},
			{Key: "a_wise_word", Name: "A Wise Word", Description: "Listen to the mentor.", Hidden: true,
				Conditions: []goals.Condition{{Type: goals.Flag, FlagKey: "met_the_mentor"}},
				Rewards:    goals.Rewards{XP: 50}},
		},
		// A three-quest story chain (the first starts on its own), a daily and a
		// weekly. "Return to the mentor" is a flag because it must happen after the
		// fight: counted conditions only know what happened, not in which order.
		Quests: []catalog.QuestConfig{
			{Key: "the_awakening", Name: "The Awakening", Description: "Find the mentor.", Chain: "Main Story", AutoAccept: true,
				Stages: []catalog.QuestStageConfig{
					{Key: "meet", Name: "Meet the mentor", Conditions: []goals.Condition{{Type: goals.Flag, FlagKey: "met_the_mentor"}},
						Rewards: goals.Rewards{Currencies: []goals.CurrencyReward{{CurrencyKey: "credits", Amount: 50}}}},
				},
				Rewards: goals.Rewards{XP: 50}},
			{Key: "into_the_mine", Name: "Into the Mine", Description: "Clear the old mine.", Chain: "Main Story", RequiresQuests: []string{"the_awakening"},
				Stages: []catalog.QuestStageConfig{
					{Key: "enter", Name: "Enter the old mine", Conditions: []goals.Condition{{Type: goals.Flag, FlagKey: "entered_the_mine"}}},
					{Key: "clear", Name: "Defeat 5 enemies", Conditions: []goals.Condition{{Type: goals.Stat, StatKey: "enemies_defeated", Target: 5}},
						Rewards: goals.Rewards{Items: []goals.ItemReward{{ItemKey: "health_potion", Quantity: 2}}}},
					{Key: "return", Name: "Return to the mentor", Conditions: []goals.Condition{{Type: goals.Flag, FlagKey: "returned_to_mentor"}}},
				},
				Rewards: goals.Rewards{Currencies: []goals.CurrencyReward{{CurrencyKey: "gems", Amount: 10}}, XP: 200}},
			{Key: "the_mentors_trial", Name: "The Mentor's Trial", Description: "Prove yourself in battle.", Chain: "Main Story",
				RequiresQuests: []string{"into_the_mine"}, RequiresAchievements: []string{"first_blood"},
				Stages: []catalog.QuestStageConfig{
					{Key: "duel", Name: "Defeat 10 enemies", Conditions: []goals.Condition{{Type: goals.Stat, StatKey: "enemies_defeated", Target: 10}}},
				},
				Rewards: goals.Rewards{Currencies: []goals.CurrencyReward{{CurrencyKey: "gems", Amount: 25}}}},
			{Key: "daily_hunt", Name: "Daily Hunt", Description: "Defeat 3 enemies today.", Chain: "Repeatable", Repeat: "daily", AutoAccept: true,
				Stages: []catalog.QuestStageConfig{
					{Key: "hunt", Name: "Defeat 3 enemies", Conditions: []goals.Condition{{Type: goals.Stat, StatKey: "enemies_defeated", Target: 3}}},
				},
				Rewards: goals.Rewards{Currencies: []goals.CurrencyReward{{CurrencyKey: "credits", Amount: 25}}}},
			{Key: "weekly_trader", Name: "Weekly Trader", Description: "Spend 200 credits this week.", Chain: "Repeatable", Repeat: "weekly",
				Stages: []catalog.QuestStageConfig{
					{Key: "spend", Name: "Spend 200 credits", Conditions: []goals.Condition{{Type: goals.CurrencySpent, CurrencyKey: "credits", Target: 200}}},
				},
				Rewards: goals.Rewards{Currencies: []goals.CurrencyReward{{CurrencyKey: "gems", Amount: 5}}}},
		},
	}
}
