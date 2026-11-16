package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

const (
	DemoAdminEmail        = "admin@realmstack.local"
	DemoAdminPassword     = "RealmStackLocal!2026"
	DemoPlayerEmail       = "player@realmstack.local"
	DemoPlayerPassword    = "PlayerLocal!2026"
	DemoPublicEnvironment = "pub_realmstack_demo_dev"
	DemoServerCredential  = "rs_demo.realmstack-local-server-secret"
)

func SeedDemo(ctx context.Context, db *pgxpool.Pool, logger *slog.Logger) error {
	adminHash, err := bcrypt.GenerateFromPassword([]byte(DemoAdminPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	playerHash, err := bcrypt.GenerateFromPassword([]byte(DemoPlayerPassword), bcrypt.DefaultCost)
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
		cfg := demoRuntimeConfiguration()
		if errs := validateConfig(cfg); len(errs) > 0 {
			return fmt.Errorf("demo configuration is invalid: %v", errs)
		}
		var versionID string
		err = tx.QueryRow(ctx, `INSERT INTO configuration_versions(project_id,environment_id,version,config,created_by) VALUES($1,$2,1,$3,$4) RETURNING id::text`, projectID, envID, demoConfiguration(), userID).Scan(&versionID)
		if err != nil {
			return err
		}
		if _, _, err = applyRuntimeConfig(ctx, tx, projectID, envID, versionID, cfg); err != nil {
			return err
		}
	}
	var credentialID string
	err = tx.QueryRow(ctx, `SELECT id::text FROM server_credentials WHERE key_prefix='rs_demo' AND project_id=$1 AND environment_id=$2`, projectID, envID).Scan(&credentialID)
	if err != nil {
		_, err = tx.Exec(ctx, `INSERT INTO server_credentials(project_id,environment_id,key_prefix,secret_hash) VALUES($1,$2,'rs_demo',$3)`, projectID, envID, digest(strings.SplitN(DemoServerCredential, ".", 2)[1]))
		if err != nil {
			return err
		}
	}
	var playerID string
	err = tx.QueryRow(ctx, `INSERT INTO players(project_id,environment_id,email,password_hash,display_name) VALUES($1,$2,$3,$4,'Demo Player') ON CONFLICT(project_id,environment_id,email) WHERE email IS NOT NULL DO UPDATE SET password_hash=EXCLUDED.password_hash RETURNING id::text`, projectID, envID, DemoPlayerEmail, string(playerHash)).Scan(&playerID)
	if err != nil {
		return err
	}
	// Starting inventory and balance are granted once, each with a matching ledger row,
	// so the demo player's balances always reconcile with economy_transactions.
	_, err = tx.Exec(ctx, `WITH granted AS (
		INSERT INTO inventory(player_id,project_id,environment_id,item_id,quantity)
		SELECT $1,$2,$3,id,1 FROM item_definitions WHERE project_id=$2 AND environment_id=$3 AND item_key='health_potion'
		ON CONFLICT(player_id,item_id) DO NOTHING RETURNING item_id)
	INSERT INTO economy_transactions(player_id,project_id,environment_id,item_id,operation,quantity,reason,reference)
	SELECT $1,$2,$3,item_id,'grant',1,'demo_seed','seed' FROM granted`, playerID, projectID, envID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `WITH credited AS (
		INSERT INTO balances(player_id,project_id,environment_id,currency_id,amount)
		SELECT $1,$2,$3,id,5000 FROM currency_definitions WHERE project_id=$2 AND environment_id=$3 AND currency_key='credits'
		ON CONFLICT(player_id,currency_id) DO NOTHING RETURNING currency_id)
	INSERT INTO economy_transactions(player_id,project_id,environment_id,currency_id,operation,quantity,balance_after,reason,reference)
	SELECT $1,$2,$3,currency_id,'credit',5000,5000,'demo_seed','seed' FROM credited`, playerID, projectID, envID)
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if logger != nil {
		logger.Warn("LOCAL DEMO ENABLED: seeded demo accounts and server credential", "admin_email", DemoAdminEmail, "player_email", DemoPlayerEmail, "public_environment", DemoPublicEnvironment)
	}
	return nil
}

func demoConfiguration() []byte {
	b, _ := json.Marshal(demoRuntimeConfiguration())
	return b
}
func demoRuntimeConfiguration() runtimeConfig {
	return runtimeConfig{
		Items: []itemConfig{
			{Key: "health_potion", Name: "Health Potion", Category: "consumable", Stackable: true, MaxStack: 99, Metadata: map[string]any{"healing": 50}},
			{Key: "iron_sword", Name: "Iron Sword", Category: "weapon", Stackable: false, MaxStack: 1, Metadata: map[string]any{"slot": "weapon", "attack": 12}},
		},
		Currencies:   []currencyConfig{{Key: "credits", Name: "Credits"}, {Key: "gems", Name: "Gems"}},
		Progression:  []int64{100, 300, 700, 1500},
		Leaderboards: []leaderboardConfig{{Key: "global", Direction: "higher"}},
	}
}
