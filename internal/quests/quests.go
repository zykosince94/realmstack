// Package quests serves active, staged goals (spec 2.7, 6.5): the player accepts a
// quest, works through ordered stages and claims stage and completion rewards. Quests
// share the goal engine with achievements: the same conditions, ledger-derived
// evaluation (inside an acceptance-anchored window) and transactional reward payment.
package quests

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/achievements"
	"realmstack-backend/internal/goals"
	"realmstack-backend/internal/platform"
)

// Handler serves quest routes.
type Handler struct{ *platform.Env }

// Definitions loads the active quests in their configured order, plus a by-key index.
func Definitions(ctx context.Context, q platform.Querier, scope platform.Principal) ([]*Definition, map[string]*Definition, error) {
	rows, err := q.Query(ctx, `SELECT id::text,key,name,description,COALESCE(chain,''),requires_quests,requires_achievements,stages,rewards,
		claimable_by,auto_accept,repeat,available_from,available_until,activated_at
		FROM quest_definitions WHERE project_id=$1 AND environment_id=$2 AND active ORDER BY sort_order,key`, scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var list []*Definition
	byKey := map[string]*Definition{}
	for rows.Next() {
		d := &Definition{}
		var reqQ, reqA, stages, rewards []byte
		if err = rows.Scan(&d.ID, &d.Key, &d.Name, &d.Description, &d.Chain, &reqQ, &reqA, &stages, &rewards,
			&d.ClaimableBy, &d.AutoAccept, &d.Repeat, &d.AvailableFrom, &d.AvailableUntil, &d.ActivatedAt); err != nil {
			return nil, nil, err
		}
		for _, x := range []struct {
			raw []byte
			dst any
		}{{reqQ, &d.RequiresQuests}, {reqA, &d.RequiresAchievements}, {stages, &d.Stages}, {rewards, &d.Rewards}} {
			if err = json.Unmarshal(x.raw, x.dst); err != nil {
				return nil, nil, err
			}
		}
		list = append(list, d)
		byKey[d.Key] = d
	}
	return list, byKey, rows.Err()
}

// loadPlayer reads the acceptance and claim records evaluation needs: the current
// period's for each quest, plus each quest's first completion in any period.
func loadPlayer(ctx context.Context, q platform.Querier, scope platform.Principal, playerID string, now time.Time, defs []*Definition) (Player, error) {
	p := Player{Acceptances: map[string]Acceptance{}, PeriodClaims: map[string]map[int]time.Time{}, FirstCompletion: map[string]time.Time{}}
	byID := map[string]*Definition{}
	needAchievements := false
	for _, d := range defs {
		byID[d.ID] = d
		needAchievements = needAchievements || len(d.RequiresAchievements) > 0
	}
	// Only three period keys can be current right now: '' (non-repeating), today's
	// and this week's. Each row is then matched against its own quest's period.
	periods := []string{"", PeriodAt(RepeatDaily, now).Key, PeriodAt(RepeatWeekly, now).Key}
	current := func(questID, period string) (*Definition, bool) {
		d, ok := byID[questID]
		return d, ok && PeriodAt(d.Repeat, now).Key == period
	}

	rows, err := q.Query(ctx, `SELECT quest_id::text,period_key,status,accepted_at FROM player_quests WHERE player_id=$1 AND project_id=$2 AND environment_id=$3 AND period_key=ANY($4)`,
		playerID, scope.ProjectID, scope.EnvironmentID, periods)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		var questID, period, status string
		var at time.Time
		if err = rows.Scan(&questID, &period, &status, &at); err != nil {
			rows.Close()
			return p, err
		}
		if d, ok := current(questID, period); ok {
			p.Acceptances[d.Key] = Acceptance{Active: status == "active", AcceptedAt: at}
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return p, err
	}

	rows, err = q.Query(ctx, `SELECT quest_id::text,period_key,stage_index,claimed_at FROM quest_claims WHERE player_id=$1 AND project_id=$2 AND environment_id=$3 AND period_key=ANY($4)`,
		playerID, scope.ProjectID, scope.EnvironmentID, periods)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		var questID, period string
		var stage int
		var at time.Time
		if err = rows.Scan(&questID, &period, &stage, &at); err != nil {
			rows.Close()
			return p, err
		}
		if d, ok := current(questID, period); ok {
			if p.PeriodClaims[d.Key] == nil {
				p.PeriodClaims[d.Key] = map[int]time.Time{}
			}
			p.PeriodClaims[d.Key][stage] = at
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return p, err
	}

	rows, err = q.Query(ctx, `SELECT quest_id::text,MIN(claimed_at) FROM quest_claims WHERE player_id=$1 AND project_id=$2 AND environment_id=$3 AND stage_index=-1 GROUP BY quest_id`,
		playerID, scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		var questID string
		var at time.Time
		if err = rows.Scan(&questID, &at); err != nil {
			rows.Close()
			return p, err
		}
		if d, ok := byID[questID]; ok {
			p.FirstCompletion[d.Key] = at
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return p, err
	}

	if needAchievements {
		done, err := achievements.Completions(ctx, q, scope, playerID)
		if err != nil {
			return p, err
		}
		p.Achievements = map[string]AchievementState{}
		for key, c := range done {
			p.Achievements[key] = AchievementState{Completed: c.Completed, ClaimedAt: c.ClaimedAt}
		}
	}
	return p, nil
}

// evaluator loads everything for one player and returns a ready evaluator.
func evaluator(ctx context.Context, q platform.Querier, scope platform.Principal, playerID string, now time.Time) (*Evaluator, []*Definition, error) {
	list, byKey, err := Definitions(ctx, q, scope)
	if err != nil {
		return nil, nil, err
	}
	p, err := loadPlayer(ctx, q, scope, playerID, now, list)
	if err != nil {
		return nil, nil, err
	}
	facts := func(w goals.Window) (*goals.Facts, error) { return goals.LoadFacts(ctx, q, scope, playerID, w) }
	return NewEvaluator(now, byKey, p, facts), list, nil
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// view renders one quest's state for the quest log.
func view(s *State) gin.H {
	d := s.Def
	stages := make([]gin.H, len(d.Stages))
	for i, st := range d.Stages {
		x := gin.H{"key": st.Key, "name": st.Name, "conditions": st.Conditions, "rewards": st.Rewards,
			"progress": nil, "complete": s.Stages[i].Complete, "claimed": s.Stages[i].Claimed}
		if s.Engaged {
			x["progress"] = s.Stages[i].Progress
		}
		stages[i] = x
	}
	var current any
	if s.CurrentStage >= 0 {
		current = s.CurrentStage
	}
	claimable := s.ClaimableStages
	if claimable == nil {
		claimable = []int{}
	}
	var chain any
	if d.Chain != "" {
		chain = d.Chain
	}
	return gin.H{
		"key": d.Key, "name": d.Name, "description": d.Description, "chain": chain,
		"repeat": d.Repeat, "auto_accept": d.AutoAccept, "claimable_by": d.ClaimableBy,
		"requires_quests": orEmpty(d.RequiresQuests), "requires_achievements": orEmpty(d.RequiresAchievements),
		"status": s.Status, "period_key": s.Period.Key, "accepted_at": s.AcceptedAt, "auto_accepted": s.AutoAccepted,
		"current_stage": current, "stages": stages, "rewards": d.Rewards, "completion_claimed": s.CompletionClaimed,
		"claimable":      gin.H{"stages": claimable, "completion": s.ClaimableComplete},
		"locked_by":      gin.H{"quests": orEmpty(s.LockedByQuests), "achievements": orEmpty(s.LockedByAchievs)},
		"available_from": d.AvailableFrom, "available_until": d.AvailableUntil,
		// resets_at is when a repeatable quest's period ends; expires_at is when an
		// event quest's window closes.
		"resets_at": s.Period.End, "expires_at": d.AvailableUntil,
	}
}

// log reads from the replica when one is configured; accept, abandon and claim
// evaluate on the primary inside their transaction.
func (h Handler) log(c *gin.Context, scope platform.Principal, playerID string) {
	e, list, err := evaluator(c, h.Reader(), scope, playerID, h.Now())
	if err != nil {
		platform.Internal(c, err)
		return
	}
	out := make([]gin.H, 0, len(list))
	for _, d := range list {
		s, err := e.State(d.Key)
		if err != nil {
			platform.Internal(c, err)
			return
		}
		out = append(out, view(s))
	}
	platform.Success(c, http.StatusOK, out)
}

// Mine is the player's quest log.
func (h Handler) Mine(c *gin.Context) {
	p := platform.GetPrincipal(c)
	h.log(c, p, p.ID)
}

// ForPlayer is a player's quest log for a trusted game server.
func (h Handler) ForPlayer(c *gin.Context) {
	actor, ok := platform.ServerTarget(c, h.DB)
	if !ok {
		return
	}
	h.log(c, actor, actor.ID)
}

// stateIn locks the player and evaluates one quest inside tx. Locking first makes
// accept, abandon and claim for the same player run one at a time.
func (h Handler) stateIn(c *gin.Context, tx pgx.Tx, actor platform.Principal, key string, now time.Time) (*State, error) {
	if err := platform.LockPlayer(c, tx, actor, actor.ID); err != nil {
		return nil, err
	}
	e, _, err := evaluator(c, tx, actor, actor.ID, now)
	if err != nil {
		return nil, err
	}
	s, err := e.State(key)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, platform.Errf(http.StatusNotFound, "NOT_FOUND", "No published quest is named %q.", key)
	}
	return s, nil
}

// fresh re-evaluates one quest after a write in the same transaction.
func fresh(c *gin.Context, tx pgx.Tx, actor platform.Principal, key string, now time.Time) (gin.H, error) {
	e, _, err := evaluator(c, tx, actor, actor.ID, now)
	if err != nil {
		return nil, err
	}
	s, err := e.State(key)
	if err != nil {
		return nil, err
	}
	return view(s), nil
}

// AcceptMine accepts a quest for the signed-in player.
func (h Handler) AcceptMine(c *gin.Context) { h.accept(c, platform.GetPrincipal(c)) }

// AcceptForPlayer accepts a quest on a player's behalf (trusted server).
func (h Handler) AcceptForPlayer(c *gin.Context) {
	actor, ok := platform.ServerTarget(c, h.DB)
	if !ok {
		return
	}
	h.accept(c, actor)
}

// accept is allowed only when the quest is available and inside its window (spec
// 6.5). Accepting again after abandoning reactivates the period's row with a new
// acceptance time, so progress restarts; stage claims already made this period stay,
// so their rewards are not paid twice.
func (h Handler) accept(c *gin.Context, actor platform.Principal) {
	key := c.Param("key")
	h.Mutate(c, actor, gin.H{"quest": key}, func(tx pgx.Tx) (int, any, error) {
		now := h.Now()
		s, err := h.stateIn(c, tx, actor, key, now)
		if err != nil {
			return 0, nil, err
		}
		switch s.Status {
		case StatusAvailable:
		case StatusClaimed:
			return 0, nil, platform.Errf(http.StatusConflict, "ALREADY_CLAIMED", "This quest was already completed and claimed in this period.")
		case StatusActive, StatusCompleted:
			return 0, nil, platform.Errf(http.StatusConflict, "QUEST_ALREADY_ACTIVE", "This quest is already active.")
		case StatusLocked:
			e := platform.Errf(http.StatusUnprocessableEntity, "QUEST_LOCKED", "Complete this quest's prerequisites first.")
			e.Details = gin.H{"locked_by": gin.H{"quests": orEmpty(s.LockedByQuests), "achievements": orEmpty(s.LockedByAchievs)}}
			return 0, nil, e
		case StatusUpcoming:
			return 0, nil, platform.Errf(http.StatusUnprocessableEntity, "QUEST_NOT_AVAILABLE", "This quest is not available yet.")
		case StatusExpired:
			return 0, nil, platform.Errf(http.StatusUnprocessableEntity, "QUEST_EXPIRED", "This quest's availability window has closed.")
		}
		if _, err = tx.Exec(c, `INSERT INTO player_quests(player_id,project_id,environment_id,quest_id,period_key,status,accepted_at) VALUES($1,$2,$3,$4,$5,'active',$6)
			ON CONFLICT(player_id,quest_id,period_key) DO UPDATE SET status='active',accepted_at=EXCLUDED.accepted_at,abandoned_at=NULL`,
			actor.ID, actor.ProjectID, actor.EnvironmentID, s.Def.ID, s.Period.Key, now); err != nil {
			return 0, nil, err
		}
		out, err := fresh(c, tx, actor, key, now)
		return http.StatusOK, out, err
	})
}

// AbandonMine abandons an active quest. It returns to available; claimed rewards are
// kept. An auto_accept quest gets an abandoned record so it stops counting.
func (h Handler) AbandonMine(c *gin.Context) {
	actor := platform.GetPrincipal(c)
	key := c.Param("key")
	h.Mutate(c, actor, gin.H{"quest": key}, func(tx pgx.Tx) (int, any, error) {
		now := h.Now()
		s, err := h.stateIn(c, tx, actor, key, now)
		if err != nil {
			return 0, nil, err
		}
		if s.Status != StatusActive {
			e := platform.Errf(http.StatusUnprocessableEntity, "QUEST_NOT_ACTIVE", "Only an active quest can be abandoned.")
			e.Details = gin.H{"status": s.Status}
			return 0, nil, e
		}
		if _, err = tx.Exec(c, `INSERT INTO player_quests(player_id,project_id,environment_id,quest_id,period_key,status,accepted_at,abandoned_at) VALUES($1,$2,$3,$4,$5,'abandoned',$6,$7)
			ON CONFLICT(player_id,quest_id,period_key) DO UPDATE SET status='abandoned',abandoned_at=EXCLUDED.abandoned_at`,
			actor.ID, actor.ProjectID, actor.EnvironmentID, s.Def.ID, s.Period.Key, *s.AcceptedAt, now); err != nil {
			return 0, nil, err
		}
		out, err := fresh(c, tx, actor, key, now)
		return http.StatusOK, out, err
	})
}

// ClaimMine claims a quest's earned rewards (claim policy "player").
func (h Handler) ClaimMine(c *gin.Context) { h.claim(c, platform.GetPrincipal(c), "player") }

// ClaimForPlayer claims a quest's earned rewards on a player's behalf.
func (h Handler) ClaimForPlayer(c *gin.Context) {
	actor, ok := platform.ServerTarget(c, h.DB)
	if !ok {
		return
	}
	h.claim(c, actor, "server")
}

// claim delivers every earned but unclaimed stage reward plus the completion reward
// if the quest is complete, in one transaction (spec 2.7, 6.4): idempotency → lock the
// player → evaluate → reject → for each reward: insert its claim record, then pay it
// tagged source='quest' → commit. Any failing reward rolls the whole claim back. The
// unique (player, quest, period, stage) key is the final guard against paying twice.
func (h Handler) claim(c *gin.Context, actor platform.Principal, by string) {
	key := c.Param("key")
	h.Mutate(c, actor, gin.H{"quest": key}, func(tx pgx.Tx) (int, any, error) {
		now := h.Now()
		s, err := h.stateIn(c, tx, actor, key, now)
		if err != nil {
			return 0, nil, err
		}
		d := s.Def
		if by == "player" && d.ClaimableBy != "player" {
			return 0, nil, platform.Errf(http.StatusForbidden, "CLAIM_NOT_ALLOWED", "This quest can only be claimed by the game server.")
		}
		if !s.Claimable() {
			switch {
			case s.CompletionClaimed:
				return 0, nil, platform.Errf(http.StatusConflict, "ALREADY_CLAIMED", "This quest's rewards were already claimed in this period.")
			case s.Status == StatusLocked:
				return 0, nil, platform.Errf(http.StatusUnprocessableEntity, "QUEST_LOCKED", "Complete this quest's prerequisites first.")
			case !s.Engaged:
				return 0, nil, platform.Errf(http.StatusUnprocessableEntity, "QUEST_NOT_ACTIVE", "Accept the quest before claiming its rewards.")
			default:
				e := platform.Errf(http.StatusUnprocessableEntity, "NOT_COMPLETED", "No stage or completion reward has been earned since the last claim.")
				e.Details = gin.H{"current_stage": s.CurrentStage}
				return 0, nil, e
			}
		}
		type paid struct {
			StageIndex int           `json:"stage_index"`
			StageKey   *string       `json:"stage_key"`
			Rewards    goals.Rewards `json:"rewards"`
		}
		var out []paid
		pay := func(stage int, rewards goals.Rewards, reason string, stageKey *string) error {
			snapshot, _ := json.Marshal(rewards)
			var claimID string
			if err := tx.QueryRow(c, `INSERT INTO quest_claims(player_id,project_id,environment_id,quest_id,period_key,stage_index,rewards,claimed_by,claimed_at)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id::text`,
				actor.ID, actor.ProjectID, actor.EnvironmentID, d.ID, s.Period.Key, stage, snapshot, by, now).Scan(&claimID); err != nil {
				if platform.IsUniqueViolation(err) {
					return platform.Errf(http.StatusConflict, "ALREADY_CLAIMED", "This reward was already claimed in this period.")
				}
				return err
			}
			if err := goals.ApplyRewards(c, tx, actor, actor.ID, rewards, goals.SourceQuest, reason, "quest-claim:"+claimID, now); err != nil {
				return err
			}
			out = append(out, paid{StageIndex: stage, StageKey: stageKey, Rewards: rewards})
			return nil
		}
		for _, i := range s.ClaimableStages {
			st := d.Stages[i]
			if err = pay(i, st.Rewards, "quest:"+d.Key+":"+st.Key, &st.Key); err != nil {
				return 0, nil, err
			}
		}
		if s.ClaimableComplete {
			if err = pay(CompletionStage, d.Rewards, "quest:"+d.Key, nil); err != nil {
				return 0, nil, err
			}
		}
		quest, err := fresh(c, tx, actor, key, now)
		if err != nil {
			return 0, nil, err
		}
		return http.StatusOK, gin.H{"key": d.Key, "period_key": s.Period.Key, "claimed_by": by, "claimed_at": now, "paid": out, "quest": quest}, nil
	})
}

// Stats returns acceptance and claim counts per published quest for the dashboard,
// summed over all periods (a daily quest accepted on three days counts three times).
// Auto-accepted quests have no acceptance record until abandoned, so "accepted"
// counts explicit acceptances. It aggregates only; no player identifiers leave it.
func (h Handler) Stats(c *gin.Context) {
	projectID, envID := platform.Owned(c)
	rows, err := h.DB.Query(c, `SELECT d.key,d.name,COALESCE(d.chain,''),d.repeat,
		(SELECT COUNT(*) FROM player_quests pq WHERE pq.quest_id=d.id),
		(SELECT COUNT(*) FROM player_quests pq WHERE pq.quest_id=d.id AND pq.status='abandoned'),
		(SELECT COUNT(*) FROM quest_claims qc WHERE qc.quest_id=d.id AND qc.stage_index=-1),
		(SELECT COUNT(*) FROM quest_claims qc WHERE qc.quest_id=d.id AND qc.stage_index>=0)
		FROM quest_definitions d WHERE d.project_id=$1 AND d.environment_id=$2 AND d.active ORDER BY d.sort_order,d.key`, projectID, envID)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer rows.Close()
	out := []gin.H{}
	for rows.Next() {
		var key, name, chain, repeat string
		var accepted, abandoned, completions, stageClaims int64
		if err = rows.Scan(&key, &name, &chain, &repeat, &accepted, &abandoned, &completions, &stageClaims); err != nil {
			platform.Internal(c, err)
			return
		}
		var ch any
		if chain != "" {
			ch = chain
		}
		out = append(out, gin.H{"key": key, "name": name, "chain": ch, "repeat": repeat, "accepted": accepted,
			"abandoned": abandoned, "completions_claimed": completions, "stage_rewards_claimed": stageClaims})
	}
	if err = rows.Err(); err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, out)
}
