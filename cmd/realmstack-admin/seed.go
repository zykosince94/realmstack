package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"realmstack-backend/internal/catalog"
	"realmstack-backend/internal/goals"
	"realmstack-backend/internal/leaderboards"
	"realmstack-backend/internal/platform"
)

// LoadPassword is every load-test player's password (a disposable local tenant).
const LoadPassword = "LoadTest!2026"

// loadConfig is a small but realistic catalog: stackable items for grants, two
// currencies, a curve, two boards, and goals that make progress reads do real work.
func loadConfig() catalog.Config {
	return catalog.Config{
		Items: []catalog.ItemConfig{
			{Key: "potion", Name: "Potion", Category: "consumable", Stackable: true, MaxStack: 100000},
			{Key: "ore", Name: "Ore", Category: "material", Stackable: true, MaxStack: 100000},
		},
		Currencies:   []catalog.CurrencyConfig{{Key: "gold", Name: "Gold"}, {Key: "gems", Name: "Gems"}},
		Progression:  []int64{100, 300, 700, 1500, 3000, 6000, 10000, 20000, 40000, 80000},
		Leaderboards: []catalog.LeaderboardConfig{{Key: "arena", Direction: "higher"}, {Key: "speedrun", Direction: "lower"}},
		Stats:        []catalog.StatConfig{{Key: "kills", Name: "Kills", Kind: "counter"}, {Key: "met_guide", Name: "Met the guide", Kind: "flag"}},
		Achievements: []catalog.AchievementConfig{
			{Key: "slayer", Name: "Slayer", Conditions: []goals.Condition{{Type: goals.Stat, StatKey: "kills", Target: 100}}, Rewards: goals.Rewards{XP: 50}},
			{Key: "rich", Name: "Rich", Conditions: []goals.Condition{{Type: goals.CurrencyEarned, CurrencyKey: "gold", Target: 10000}}},
			{Key: "veteran", Name: "Veteran", Conditions: []goals.Condition{{Type: goals.LevelReached, Target: 5}, {Type: goals.ItemGranted, Target: 50}}},
		},
		Quests: []catalog.QuestConfig{
			{Key: "daily_hunt", Name: "Daily Hunt", Repeat: "daily", AutoAccept: true,
				Stages:  []catalog.QuestStageConfig{{Key: "hunt", Name: "Hunt", Conditions: []goals.Condition{{Type: goals.Stat, StatKey: "kills", Target: 20}}}},
				Rewards: goals.Rewards{Currencies: []goals.CurrencyReward{{CurrencyKey: "gold", Amount: 50}}}},
		},
	}
}

type seedOutput struct {
	BaseURLs            []string     `json:"base_urls"`
	PublicEnvironmentID string       `json:"public_environment_id"`
	Credential          string       `json:"credential"`
	Password            string       `json:"password"`
	Players             []seedPlayer `json:"players"`
}

type seedPlayer struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// seedLoad creates the load-test tenant: N players sharing one real bcrypt hash, one
// arena score each, and a JSON file the k6 suite reads. It refuses to run twice:
// load-test data lives in its own disposable database (loadtest/docker-compose.yml),
// which is recreated to reseed rather than deleting rows from append-only ledgers.
func seedLoad(ctx context.Context, db *pgxpool.Pool, args []string) error {
	fs := flag.NewFlagSet("seed-load", flag.ExitOnError)
	players := fs.Int("players", 1_000_000, "number of players")
	sample := fs.Int("sample", 20_000, "players written to the output file for k6")
	out := fs.String("out", "loadtest/data/tenant.json", "output file")
	bases := fs.String("base-urls", "http://api1:8080,http://api2:8080", "API base URLs k6 should spread requests across")
	cost := fs.Int("bcrypt-cost", platform.BcryptCost, "bcrypt cost for the shared player password hash")
	_ = fs.Parse(args)
	if *cost >= 4 && *cost <= 31 {
		platform.BcryptCost = *cost
	}

	var exists bool
	if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE slug='load-test')`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("a load-test tenant already exists; recreate the load-test database to reseed")
	}
	hash, err := platform.HashPassword(LoadPassword)
	if err != nil {
		return err
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var userID, projectID, envID string
	if err = tx.QueryRow(ctx, `INSERT INTO users(email,password_hash) VALUES('load-admin@realmstack.test',$1)
		ON CONFLICT(email) DO UPDATE SET password_hash=EXCLUDED.password_hash RETURNING id::text`, string(hash)).Scan(&userID); err != nil {
		return err
	}
	if err = tx.QueryRow(ctx, `INSERT INTO projects(owner_user_id,name,slug) VALUES($1,'Load Test','load-test') RETURNING id::text`, userID).Scan(&projectID); err != nil {
		return err
	}
	public := "pub_load_" + platform.RandomSecret(9)
	if err = tx.QueryRow(ctx, `INSERT INTO environments(project_id,name,public_id) VALUES($1,'production',$2) RETURNING id::text`, projectID, public).Scan(&envID); err != nil {
		return err
	}
	cfg := loadConfig()
	if errs := catalog.ValidateConfig(cfg); len(errs) > 0 {
		return fmt.Errorf("load config invalid: %v", errs)
	}
	raw, _ := json.Marshal(cfg)
	if _, err = tx.Exec(ctx, `INSERT INTO configuration_drafts(project_id,environment_id,config) VALUES($1,$2,$3)`, projectID, envID, raw); err != nil {
		return err
	}
	var versionID string
	if err = tx.QueryRow(ctx, `INSERT INTO configuration_versions(project_id,environment_id,version,config,created_by) VALUES($1,$2,1,$3,$4) RETURNING id::text`,
		projectID, envID, raw, userID).Scan(&versionID); err != nil {
		return err
	}
	if _, _, err = catalog.ApplyRuntimeConfig(ctx, tx, projectID, envID, versionID, cfg, time.Now()); err != nil {
		return err
	}
	prefix, secret := "rs_load"+platform.RandomSecret(4), platform.RandomSecret(36)
	if _, err = tx.Exec(ctx, `INSERT INTO server_credentials(project_id,environment_id,key_prefix,secret_hash) VALUES($1,$2,$3,$4)`,
		projectID, envID, prefix, platform.Digest(secret)); err != nil {
		return err
	}
	var arenaID string
	if err = tx.QueryRow(ctx, `SELECT id::text FROM leaderboard_definitions WHERE project_id=$1 AND key='arena'`, projectID).Scan(&arenaID); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}

	// Players and one arena score each, one statement per chunk of 100,000.
	const chunk = 100_000
	for from := 1; from <= *players; from += chunk {
		to := min(from+chunk-1, *players)
		if _, err = db.Exec(ctx, `WITH ins AS (
				INSERT INTO players(project_id,environment_id,email,password_hash,display_name)
				SELECT $1,$2,'p'||g||'@load.test',$3,'Player '||g FROM generate_series($4::int,$5::int) g
				RETURNING id,project_id,environment_id)
			INSERT INTO score_submissions(leaderboard_id,player_id,project_id,environment_id,score,idempotency_key,request_hash,submitted_at)
			SELECT $6,id,project_id,environment_id,(random()*100000)::bigint,'seed-'||id,'seed',now()-random()*interval '20 days' FROM ins`,
			projectID, envID, string(hash), from, to, arenaID); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "seeded %d/%d players\n", to, *players)
	}
	if _, err = leaderboards.RebuildBestScores(ctx, db); err != nil {
		return err
	}
	for _, table := range []string{"players", "leaderboard_best_scores", "score_submissions"} {
		if _, err = db.Exec(ctx, "ANALYZE "+table); err != nil {
			return err
		}
	}

	rows, err := db.Query(ctx, `SELECT id::text,email FROM players WHERE project_id=$1 ORDER BY random() LIMIT $2`, projectID, *sample)
	if err != nil {
		return err
	}
	res := seedOutput{BaseURLs: strings.Split(*bases, ","), PublicEnvironmentID: public, Credential: prefix + "." + secret, Password: LoadPassword}
	for rows.Next() {
		var p seedPlayer
		if err = rows.Scan(&p.ID, &p.Email); err != nil {
			rows.Close()
			return err
		}
		res.Players = append(res.Players, p)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	if err = os.WriteFile(*out, b, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d sample players)\n", *out, len(res.Players))
	return nil
}
