package goals

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/platform"
	"realmstack-backend/internal/progression"
)

// Window is a counting window [From, Until). nil bounds are open; achievements count
// all time, quests count from acceptance (see quests.Evaluator).
type Window struct {
	From, Until *time.Time
}

// Score is a player's best score on a board, already respecting its direction.
type Score struct {
	Direction string
	Best      int64
}

// Facts are a player's ledger totals for one window. Everything an evaluation needs
// is loaded up front by LoadFacts, so Evaluate is a pure function.
type Facts struct {
	XP             int64            // api XP inside the window
	Level          int              // from all-time api XP on the pinned curve
	ItemGranted    map[string]int64 // item key → quantity; "" is any item
	ItemConsumed   map[string]int64 //
	CurrencyEarned map[string]int64 // currency key → amount
	CurrencySpent  map[string]int64 //
	Stats          map[string]int64 // counter key → increments inside the window
	Flags          map[string]bool  // set flags (state: ignores the window)
	Scores         map[string]Score // board key → best score inside window and season
}

// NewFacts returns empty facts (level 1, nothing counted).
func NewFacts() *Facts {
	return &Facts{Level: 1, ItemGranted: map[string]int64{}, ItemConsumed: map[string]int64{}, CurrencyEarned: map[string]int64{},
		CurrencySpent: map[string]int64{}, Stats: map[string]int64{}, Flags: map[string]bool{}, Scores: map[string]Score{}}
}

// windowSQL limits a timestamp column to [$4, $5).
func windowSQL(column string) string {
	return ` AND ($4::timestamptz IS NULL OR ` + column + ` >= $4) AND ($5::timestamptz IS NULL OR ` + column + ` < $5)`
}

// LoadFacts reads every total a player's goals can depend on. It is the single
// data-access function for goal progress, so how totals are stored can change without
// touching callers. It only reads.
//
// An all-time window (achievements) reads the maintained totals: player_goal_totals and
// leaderboard_best_scores, a handful of indexed rows however long the player's
// history. A bounded window (quests) still sums the ledgers inside the window, which
// the composite (player_id, created_at) indexes keep proportional to the window.
func LoadFacts(ctx context.Context, q platform.Querier, scope platform.Principal, playerID string, w Window) (*Facts, error) {
	if w.From == nil && w.Until == nil {
		return loadAllTime(ctx, q, scope, playerID)
	}
	f := NewFacts()
	args := []any{playerID, scope.ProjectID, scope.EnvironmentID, w.From, w.Until}

	rows, err := q.Query(ctx, `SELECT t.operation,COALESCE(i.item_key,''),COALESCE(cd.currency_key,''),SUM(t.quantity)::bigint
		FROM economy_transactions t LEFT JOIN item_definitions i ON i.id=t.item_id LEFT JOIN currency_definitions cd ON cd.id=t.currency_id
		WHERE t.player_id=$1 AND t.project_id=$2 AND t.environment_id=$3 AND t.source='api'`+windowSQL("t.created_at")+` GROUP BY 1,2,3`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var op, item, currency string
		var total int64
		if err = rows.Scan(&op, &item, &currency, &total); err != nil {
			rows.Close()
			return nil, err
		}
		switch op {
		case "grant":
			f.ItemGranted[item] += total
			f.ItemGranted[""] += total
		case "consume":
			f.ItemConsumed[item] += total
			f.ItemConsumed[""] += total
		case "credit":
			f.CurrencyEarned[currency] += total
		case "spend":
			f.CurrencySpent[currency] += total
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}

	if err = q.QueryRow(ctx, `SELECT COALESCE(SUM(amount),0)::bigint FROM xp_transactions WHERE player_id=$1 AND project_id=$2 AND environment_id=$3 AND source='api'`+windowSQL("created_at"), args...).Scan(&f.XP); err != nil {
		return nil, err
	}

	if f.Level, err = loadLevel(ctx, q, scope, playerID); err != nil {
		return nil, err
	}

	rows, err = q.Query(ctx, `SELECT d.key,SUM(st.amount)::bigint FROM stat_transactions st JOIN stat_definitions d ON d.id=st.stat_definition_id
		WHERE st.player_id=$1 AND st.project_id=$2 AND st.environment_id=$3 AND st.operation='increment'`+windowSQL("st.created_at")+` GROUP BY d.key`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key string
		var total int64
		if err = rows.Scan(&key, &total); err != nil {
			rows.Close()
			return nil, err
		}
		f.Stats[key] = total
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}

	if err = loadFlags(ctx, q, scope, playerID, f); err != nil {
		return nil, err
	}

	rows, err = q.Query(ctx, `SELECT d.key,d.score_direction,CASE WHEN d.score_direction='lower' THEN MIN(s.score) ELSE MAX(s.score) END
		FROM score_submissions s JOIN leaderboard_definitions d ON d.id=s.leaderboard_id
		WHERE s.player_id=$1 AND s.project_id=$2 AND s.environment_id=$3`+windowSQL("s.submitted_at")+`
		AND (d.season_start IS NULL OR s.submitted_at >= d.season_start) AND (d.season_end IS NULL OR s.submitted_at < d.season_end)
		GROUP BY d.key,d.score_direction`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key string
		var s Score
		if err = rows.Scan(&key, &s.Direction, &s.Best); err != nil {
			rows.Close()
			return nil, err
		}
		f.Scores[key] = s
	}
	rows.Close()
	return f, rows.Err()
}

// loadLevel is level_reached's input, a state condition that ignores any window: the
// all-time api XP total on the player's pinned curve, so XP paid by claims cannot
// complete it either.
func loadLevel(ctx context.Context, q platform.Querier, scope platform.Principal, playerID string) (int, error) {
	var apiXP int64
	var raw []byte
	err := q.QueryRow(ctx, `SELECT COALESCE((SELECT total FROM player_goal_totals WHERE player_id=$1 AND metric='xp'),0)::bigint,pv.thresholds
		FROM player_progression pp JOIN progression_versions pv ON pv.id=pp.progression_version_id WHERE pp.player_id=$1 AND pp.project_id=$2 AND pp.environment_id=$3`,
		playerID, scope.ProjectID, scope.EnvironmentID).Scan(&apiXP, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return 1, nil
	}
	if err != nil {
		return 0, err
	}
	var thresholds []int64
	_ = json.Unmarshal(raw, &thresholds)
	return progression.LevelFor(thresholds, apiXP), nil
}

// loadFlags reads the player's set flags (state: flags ignore any window).
func loadFlags(ctx context.Context, q platform.Querier, scope platform.Principal, playerID string, f *Facts) error {
	rows, err := q.Query(ctx, `SELECT d.key FROM player_stats ps JOIN stat_definitions d ON d.id=ps.stat_definition_id
		WHERE ps.player_id=$1 AND ps.project_id=$2 AND ps.environment_id=$3 AND d.kind='flag' AND ps.value>0`, playerID, scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			return err
		}
		f.Flags[key] = true
	}
	return rows.Err()
}

// loadAllTime builds all-time facts from the maintained totals (three indexed reads).
func loadAllTime(ctx context.Context, q platform.Querier, scope platform.Principal, playerID string) (*Facts, error) {
	f := NewFacts()
	rows, err := q.Query(ctx, `SELECT metric,total FROM player_goal_totals WHERE player_id=$1 AND project_id=$2 AND environment_id=$3`, playerID, scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		return nil, err
	}
	var apiXP int64
	for rows.Next() {
		var metric string
		var total int64
		if err = rows.Scan(&metric, &total); err != nil {
			rows.Close()
			return nil, err
		}
		kind, key, _ := strings.Cut(metric, ":")
		switch kind {
		case "grant":
			f.ItemGranted[key] = total // "grant:" (empty key) is the any-item total
		case "consume":
			f.ItemConsumed[key] = total
		case "credit":
			f.CurrencyEarned[key] = total
		case "spend":
			f.CurrencySpent[key] = total
		case "stat":
			f.Stats[key] = total
		case "xp":
			apiXP = total
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	f.XP = apiXP
	if f.Level, err = loadLevel(ctx, q, scope, playerID); err != nil {
		return nil, err
	}
	if err = loadFlags(ctx, q, scope, playerID, f); err != nil {
		return nil, err
	}
	// Best scores are maintained per board in the submission transaction; a board's
	// season bounds which scores were accepted in the first place.
	rows, err = q.Query(ctx, `SELECT d.key,d.score_direction,b.best_score FROM leaderboard_best_scores b JOIN leaderboard_definitions d ON d.id=b.leaderboard_id
		WHERE b.player_id=$1 AND b.project_id=$2 AND b.environment_id=$3`, playerID, scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var s Score
		if err = rows.Scan(&key, &s.Direction, &s.Best); err != nil {
			return nil, err
		}
		f.Scores[key] = s
	}
	return f, rows.Err()
}

// Progress is the evaluated state of one condition.
type Progress struct {
	Type    string `json:"type"`
	Current int64  `json:"current"`
	Target  int64  `json:"target"`
	Met     bool   `json:"met"`
	// HasValue is false for a leaderboard_score condition with no score yet, where
	// Current (0) is not a real score.
	HasValue bool `json:"has_value"`
}

// Evaluate computes progress for each condition from facts. Pure and deterministic.
func Evaluate(conds []Condition, f *Facts) []Progress {
	out := make([]Progress, len(conds))
	for i, c := range conds {
		p := Progress{Type: c.Type, Target: c.Target, HasValue: true}
		counted := func(v int64) { p.Current, p.Met = v, v >= c.Target }
		switch c.Type {
		case XPTotal:
			counted(f.XP)
		case LevelReached:
			counted(int64(f.Level))
		case ItemGranted:
			counted(f.ItemGranted[c.ItemKey])
		case ItemConsumed:
			counted(f.ItemConsumed[c.ItemKey])
		case CurrencyEarned:
			counted(f.CurrencyEarned[c.CurrencyKey])
		case CurrencySpent:
			counted(f.CurrencySpent[c.CurrencyKey])
		case Stat:
			counted(f.Stats[c.StatKey])
		case Flag:
			p.Target = 1
			if f.Flags[c.FlagKey] {
				p.Current, p.Met = 1, true
			}
		case LeaderboardScore:
			s, ok := f.Scores[c.LeaderboardKey]
			p.HasValue = ok
			if ok {
				p.Current = s.Best
				// At least the target on higher-is-better boards, at most on lower-is-better.
				p.Met = s.Best >= c.Target
				if s.Direction == "lower" {
					p.Met = s.Best <= c.Target
				}
			}
		}
		out[i] = p
	}
	return out
}

// AllMet reports whether every condition is met (all must be met; none means not met).
func AllMet(progress []Progress) bool {
	for _, p := range progress {
		if !p.Met {
			return false
		}
	}
	return len(progress) > 0
}
