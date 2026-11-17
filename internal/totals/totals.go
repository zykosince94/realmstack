// Package totals maintains player_goal_totals: each player's all-time sums of the
// ledger rows that goals count (source='api' only). They are written in the same
// transaction as the ledger insert, so a total always equals the sum of committed
// ledger rows, and they can be rebuilt from the ledgers at any time (Rebuild).
//
// Why: evaluating "earn 10,000 credits" by summing a player's whole ledger on every
// achievement read grows with the player's history. A total is one indexed row
// (spec 6.6). Windowed (quest) conditions still read the ledgers, bounded by the window.
package totals

import (
	"context"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/platform"
)

// Metric names. The empty-key forms count any item.
func Grant(itemKey string) string      { return "grant:" + itemKey }
func Consume(itemKey string) string    { return "consume:" + itemKey }
func Credit(currencyKey string) string { return "credit:" + currencyKey }
func Spend(currencyKey string) string  { return "spend:" + currencyKey }
func Stat(statKey string) string       { return "stat:" + statKey }

const (
	GrantAny   = "grant:"
	ConsumeAny = "consume:"
	XP         = "xp"
)

// Delta adds Amount to one metric.
type Delta struct {
	Metric string
	Amount int64
}

// Add upserts deltas for one player inside tx. Metrics are written in sorted order so
// two transactions touching the same player's totals always lock rows in the same
// order and cannot deadlock.
func Add(ctx context.Context, tx pgx.Tx, scope platform.Principal, playerID string, at time.Time, deltas ...Delta) error {
	if len(deltas) == 0 {
		return nil
	}
	sort.Slice(deltas, func(i, j int) bool { return deltas[i].Metric < deltas[j].Metric })
	metrics := make([]string, len(deltas))
	amounts := make([]int64, len(deltas))
	for i, d := range deltas {
		metrics[i], amounts[i] = d.Metric, d.Amount
	}
	err := platform.DeferExec(ctx, tx, `INSERT INTO player_goal_totals(player_id,project_id,environment_id,metric,total,updated_at)
		SELECT $1,$2,$3,m.metric,m.amount,$6 FROM unnest($4::text[],$5::bigint[]) WITH ORDINALITY AS m(metric,amount,ord) ORDER BY m.ord
		ON CONFLICT (player_id,metric) DO UPDATE SET total=player_goal_totals.total+EXCLUDED.total,updated_at=EXCLUDED.updated_at`,
		playerID, scope.ProjectID, scope.EnvironmentID, metrics, amounts, at)
	return err
}

// recomputeSQL recomputes every total from the ledgers. Keys come from the definitions
// the ledger rows reference; keys are stable per definition, so the result matches
// what the write path recorded.
const recomputeSQL = `SELECT player_id,project_id,environment_id,metric,SUM(amount)::bigint AS total FROM (
	SELECT t.player_id,t.project_id,t.environment_id,'grant:'||i.item_key AS metric,t.quantity AS amount FROM economy_transactions t JOIN item_definitions i ON i.id=t.item_id WHERE t.source='api' AND t.operation='grant'
	UNION ALL SELECT t.player_id,t.project_id,t.environment_id,'grant:',t.quantity FROM economy_transactions t WHERE t.source='api' AND t.operation='grant'
	UNION ALL SELECT t.player_id,t.project_id,t.environment_id,'consume:'||i.item_key,t.quantity FROM economy_transactions t JOIN item_definitions i ON i.id=t.item_id WHERE t.source='api' AND t.operation='consume'
	UNION ALL SELECT t.player_id,t.project_id,t.environment_id,'consume:',t.quantity FROM economy_transactions t WHERE t.source='api' AND t.operation='consume'
	UNION ALL SELECT t.player_id,t.project_id,t.environment_id,'credit:'||c.currency_key,t.quantity FROM economy_transactions t JOIN currency_definitions c ON c.id=t.currency_id WHERE t.source='api' AND t.operation='credit'
	UNION ALL SELECT t.player_id,t.project_id,t.environment_id,'spend:'||c.currency_key,t.quantity FROM economy_transactions t JOIN currency_definitions c ON c.id=t.currency_id WHERE t.source='api' AND t.operation='spend'
	UNION ALL SELECT x.player_id,x.project_id,x.environment_id,'xp',x.amount FROM xp_transactions x WHERE x.source='api'
	UNION ALL SELECT s.player_id,s.project_id,s.environment_id,'stat:'||d.key,s.amount FROM stat_transactions s JOIN stat_definitions d ON d.id=s.stat_definition_id WHERE s.operation='increment'
) src GROUP BY player_id,project_id,environment_id,metric`

const rebuildSQL = `INSERT INTO player_goal_totals(player_id,project_id,environment_id,metric,total,updated_at)
SELECT player_id,project_id,environment_id,metric,total,now() FROM (` + recomputeSQL + `) r`

// Rebuild replaces every goal total with values recomputed from the ledgers. It locks
// the ledgers against writes for its duration (SHARE mode still allows reads), so no
// write can land between the delete and the recomputation. Run it in a quiet period.
func Rebuild(ctx context.Context, db interface {
	Begin(context.Context) (pgx.Tx, error)
}) (int64, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `LOCK TABLE economy_transactions, xp_transactions, stat_transactions IN SHARE MODE`); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM player_goal_totals`); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, rebuildSQL)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), tx.Commit(ctx)
}

// Verify counts totals that differ from a recomputation from the ledgers (missing,
// extra or different), without changing anything. Zero means the maintained totals
// are exactly what a rebuild would produce.
func Verify(ctx context.Context, q platform.Querier) (int64, error) {
	var n int64
	err := q.QueryRow(ctx, `SELECT count(*) FROM player_goal_totals t FULL OUTER JOIN (`+recomputeSQL+`) r
		ON r.player_id=t.player_id AND r.metric=t.metric
		WHERE t.player_id IS NULL OR r.player_id IS NULL OR r.total<>t.total`).Scan(&n)
	return n, err
}
