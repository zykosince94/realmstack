// Command realmstack-admin runs operator tasks against the database directly:
// migrations, maintenance, rebuilding and verifying the derived tables, and seeding a
// load-test tenant. It uses DATABASE_DIRECT_URL (or DATABASE_URL): rebuilds take
// table locks and seeding runs long transactions, neither of which belongs behind a
// transaction-mode pooler.
//
//	realmstack-admin migrate
//	realmstack-admin maintenance                 # partitions, idempotency purge, rate-limit windows
//	realmstack-admin rebuild-leaderboards        # recompute leaderboard_best_scores from submissions (and replay Redis)
//	realmstack-admin rebuild-projection          # replay every board into the Redis rank projection
//	realmstack-admin rebuild-goal-totals         # recompute player_goal_totals from the ledgers
//	realmstack-admin verify                      # compare both (and Redis) with a recomputation; exit 1 on any difference
//	realmstack-admin seed-load -players 500000 -out loadtest/data/tenant.json
//
// REDIS_URL, when set, adds the Redis rank projection to rebuild and verify.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"realmstack-backend/internal/leaderboards"
	"realmstack-backend/internal/maintenance"
	"realmstack-backend/internal/persistence"
	"realmstack-backend/internal/totals"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: realmstack-admin migrate|maintenance|rebuild-leaderboards|rebuild-projection|rebuild-goal-totals|verify|seed-load [flags]")
		os.Exit(2)
	}
	ctx := context.Background()
	url := os.Getenv("DATABASE_DIRECT_URL")
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		fail(err)
	}
	defer db.Close()
	var rdb redis.UniversalClient
	if u := os.Getenv("REDIS_URL"); u != "" {
		opts, err := redis.ParseURL(u)
		if err != nil {
			fail(err)
		}
		client := redis.NewClient(opts)
		defer client.Close()
		rdb = client
	}
	started := time.Now()
	switch os.Args[1] {
	case "migrate":
		err = persistence.Migrate(ctx, db)
	case "maintenance":
		maintenance.RunOnce(ctx, db, logger)
	case "rebuild-leaderboards":
		var n int64
		if n, err = leaderboards.RebuildBestScores(ctx, db); err == nil {
			fmt.Printf("rebuilt %d best-score rows\n", n)
			// Best scores may have changed in any direction, which the projection's
			// improve-only writes cannot follow: replay every board.
			if rdb != nil {
				err = rebuildProjection(ctx, db, rdb, logger)
			}
		}
	case "rebuild-projection":
		err = rebuildProjection(ctx, db, rdb, logger)
	case "rebuild-goal-totals":
		var n int64
		if n, err = totals.Rebuild(ctx, db); err == nil {
			fmt.Printf("rebuilt %d goal totals\n", n)
		}
	case "verify":
		err = verify(ctx, db, rdb, logger)
	case "seed-load":
		err = seedLoad(ctx, db, os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
	if err != nil {
		fail(err)
	}
	fmt.Fprintf(os.Stderr, "done in %s\n", time.Since(started).Round(time.Millisecond))
}

// verify checks the exit criterion "rebuild commands reproduce leaderboard and goal
// totals exactly from source data" against live data, without rebuilding anything.
func verify(ctx context.Context, db *pgxpool.Pool, rdb redis.UniversalClient, logger *slog.Logger) error {
	boards, err := leaderboards.VerifyBestScores(ctx, db)
	if err != nil {
		return err
	}
	goalTotals, err := totals.Verify(ctx, db)
	if err != nil {
		return err
	}
	fmt.Printf("leaderboard_best_scores differences: %d\nplayer_goal_totals differences: %d\n", boards, goalTotals)
	var projected int64
	if rdb != nil {
		p, err := leaderboards.NewProjection(db, rdb, logger)
		if err != nil {
			return err
		}
		res, err := p.Verify(ctx)
		if err != nil {
			return err
		}
		projected = res.Mismatches
		fmt.Printf("redis rank projection differences: %d (%d boards, %d not ready and served by PostgreSQL)\n", res.Mismatches, res.Boards, res.NotReady)
	}
	if boards+goalTotals+projected > 0 {
		return fmt.Errorf("maintained tables differ from a rebuild")
	}
	return nil
}

// rebuildProjection enqueues a replay of every board; running API instances apply it.
func rebuildProjection(ctx context.Context, db *pgxpool.Pool, rdb redis.UniversalClient, logger *slog.Logger) error {
	p, err := leaderboards.NewProjection(db, rdb, logger)
	if err != nil {
		return err
	}
	n, err := p.RebuildAll(ctx)
	if err == nil {
		fmt.Printf("queued a Redis replay of %d boards (API instances apply it)\n", n)
	}
	return err
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
