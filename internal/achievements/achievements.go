// Package achievements serves permanent, passive goals: they complete as soon as their
// conditions are met (no acceptance), and each can be claimed once per player.
// Evaluation and reward payment come from the shared goals engine.
package achievements

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/goals"
	"realmstack-backend/internal/platform"
)

// Handler serves achievement routes.
type Handler struct{ *platform.Env }

// Statuses, in precedence order (spec 6.4).
const (
	StatusClaimed    = "claimed"
	StatusCompleted  = "completed"
	StatusInProgress = "in_progress"
)

// Definition is a published, active achievement.
type Definition struct {
	ID          string
	Key         string
	Name        string
	Description string
	Hidden      bool
	Conditions  []goals.Condition
	Rewards     goals.Rewards
	ClaimableBy string
}

const definitionColumns = `id::text,key,name,description,hidden,conditions,rewards,claimable_by`

func scanDefinition(row pgx.Row) (Definition, error) {
	var d Definition
	var conds, rewards []byte
	if err := row.Scan(&d.ID, &d.Key, &d.Name, &d.Description, &d.Hidden, &conds, &rewards, &d.ClaimableBy); err != nil {
		return d, err
	}
	if err := json.Unmarshal(conds, &d.Conditions); err != nil {
		return d, err
	}
	return d, json.Unmarshal(rewards, &d.Rewards)
}

// Definitions loads the active achievements in their configured order.
func Definitions(ctx context.Context, q platform.Querier, scope platform.Principal) ([]Definition, error) {
	rows, err := q.Query(ctx, `SELECT `+definitionColumns+` FROM achievement_definitions WHERE project_id=$1 AND environment_id=$2 AND active ORDER BY sort_order,key`, scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Definition
	for rows.Next() {
		d, err := scanDefinition(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

type claim struct {
	At time.Time
	By string
}

func claims(ctx context.Context, q platform.Querier, playerID string) (map[string]claim, error) {
	rows, err := q.Query(ctx, `SELECT achievement_id::text,claimed_at,claimed_by FROM achievement_claims WHERE player_id=$1`, playerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]claim{}
	for rows.Next() {
		var id string
		var cl claim
		if err = rows.Scan(&id, &cl.At, &cl.By); err != nil {
			return nil, err
		}
		out[id] = cl
	}
	return out, rows.Err()
}

// Completion is whether a player has completed an achievement, and when they claimed
// it. Quests read it for requires_achievements.
type Completion struct {
	Completed bool       // conditions met or already claimed
	ClaimedAt *time.Time // nil until claimed
}

// Completions evaluates every active achievement for a player (all-time window).
// It only reads.
func Completions(ctx context.Context, q platform.Querier, scope platform.Principal, playerID string) (map[string]Completion, error) {
	defs, err := Definitions(ctx, q, scope)
	if err != nil {
		return nil, err
	}
	facts, err := goals.LoadFacts(ctx, q, scope, playerID, goals.Window{})
	if err != nil {
		return nil, err
	}
	claimed, err := claims(ctx, q, playerID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Completion, len(defs))
	for _, d := range defs {
		c := Completion{Completed: goals.AllMet(goals.Evaluate(d.Conditions, facts))}
		if cl, ok := claimed[d.ID]; ok {
			at := cl.At
			c.Completed, c.ClaimedAt = true, &at
		}
		out[d.Key] = c
	}
	return out, nil
}

// view renders one achievement for a player. When mask is set, a hidden achievement
// that is not yet completed shows only its key and status.
func view(d Definition, progress []goals.Progress, cl *claim, mask bool) gin.H {
	status := StatusInProgress
	switch {
	case cl != nil:
		status = StatusClaimed
	case goals.AllMet(progress):
		status = StatusCompleted
	}
	out := gin.H{"key": d.Key, "hidden": d.Hidden, "status": status, "claimable_by": d.ClaimableBy, "claimed_at": nil}
	if cl != nil {
		out["claimed_at"] = cl.At
	}
	if mask && d.Hidden && status == StatusInProgress {
		out["name"], out["description"], out["conditions"], out["rewards"], out["progress"] = nil, nil, nil, nil, nil
		return out
	}
	out["name"], out["description"], out["conditions"], out["rewards"], out["progress"] = d.Name, d.Description, d.Conditions, d.Rewards, progress
	return out
}

// list reads from the replica when one is configured: progress a moment stale is
// acceptable for display. Claims evaluate on the primary, inside their transaction.
func (h Handler) list(c *gin.Context, scope platform.Principal, playerID string, mask bool) {
	db := h.Reader()
	defs, err := Definitions(c, db, scope)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	facts, err := goals.LoadFacts(c, db, scope, playerID, goals.Window{})
	if err != nil {
		platform.Internal(c, err)
		return
	}
	claimed, err := claims(c, db, playerID)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	out := make([]gin.H, 0, len(defs))
	for _, d := range defs {
		var cl *claim
		if x, ok := claimed[d.ID]; ok {
			cl = &x
		}
		out = append(out, view(d, goals.Evaluate(d.Conditions, facts), cl, mask))
	}
	platform.Success(c, http.StatusOK, out)
}

// Mine lists the player's achievements, masking hidden incomplete ones.
func (h Handler) Mine(c *gin.Context) {
	p := platform.GetPrincipal(c)
	h.list(c, p, p.ID, true)
}

// ForPlayer lists a player's achievements unmasked (trusted server).
func (h Handler) ForPlayer(c *gin.Context) {
	actor, ok := platform.ServerTarget(c, h.DB)
	if !ok {
		return
	}
	h.list(c, actor, actor.ID, false)
}

// ClaimMine claims an achievement with claim policy "player".
func (h Handler) ClaimMine(c *gin.Context) {
	h.claim(c, platform.GetPrincipal(c), "player")
}

// ClaimForPlayer claims any completed achievement on a player's behalf.
func (h Handler) ClaimForPlayer(c *gin.Context) {
	actor, ok := platform.ServerTarget(c, h.DB)
	if !ok {
		return
	}
	h.claim(c, actor, "server")
}

// claim follows spec 6.4: idempotency → lock the player → evaluate → reject if already
// claimed (409) or not complete (422) → pay rewards tagged with the achievement source
// → record the claim → commit. A failing reward rolls back everything.
func (h Handler) claim(c *gin.Context, actor platform.Principal, by string) {
	key := c.Param("key")
	h.Mutate(c, actor, gin.H{"achievement": key}, func(tx pgx.Tx) (int, any, error) {
		if err := platform.LockPlayer(c, tx, actor, actor.ID); err != nil {
			return 0, nil, err
		}
		d, err := scanDefinition(tx.QueryRow(c, `SELECT `+definitionColumns+` FROM achievement_definitions WHERE project_id=$1 AND environment_id=$2 AND key=$3 AND active`,
			actor.ProjectID, actor.EnvironmentID, key))
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil, platform.Errf(http.StatusNotFound, "NOT_FOUND", "No published achievement is named %q.", key)
		}
		if err != nil {
			return 0, nil, err
		}
		if by == "player" && d.ClaimableBy != "player" {
			return 0, nil, platform.Errf(http.StatusForbidden, "CLAIM_NOT_ALLOWED", "This achievement can only be claimed by the game server.")
		}
		var already bool
		if err = tx.QueryRow(c, `SELECT EXISTS(SELECT 1 FROM achievement_claims WHERE player_id=$1 AND achievement_id=$2)`, actor.ID, d.ID).Scan(&already); err != nil {
			return 0, nil, err
		}
		if already {
			return 0, nil, platform.Errf(http.StatusConflict, "ALREADY_CLAIMED", "This achievement has already been claimed.")
		}
		facts, err := goals.LoadFacts(c, tx, actor, actor.ID, goals.Window{})
		if err != nil {
			return 0, nil, err
		}
		progress := goals.Evaluate(d.Conditions, facts)
		if !goals.AllMet(progress) {
			e := platform.Errf(http.StatusUnprocessableEntity, "NOT_COMPLETED", "The achievement's conditions are not all met yet.")
			if !d.Hidden {
				e.Details = gin.H{"progress": progress}
			}
			return 0, nil, e
		}
		rewards, _ := json.Marshal(d.Rewards)
		var claimID string
		var claimedAt time.Time
		if err = tx.QueryRow(c, `INSERT INTO achievement_claims(player_id,project_id,environment_id,achievement_id,rewards,claimed_by,claimed_at) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id::text,claimed_at`,
			actor.ID, actor.ProjectID, actor.EnvironmentID, d.ID, rewards, by, h.Now()).Scan(&claimID, &claimedAt); err != nil {
			return 0, nil, err
		}
		if err = goals.ApplyRewards(c, tx, actor, actor.ID, d.Rewards, goals.SourceAchievement, "achievement:"+d.Key, "achievement-claim:"+claimID, claimedAt); err != nil {
			return 0, nil, err
		}
		return http.StatusOK, gin.H{"key": d.Key, "status": StatusClaimed, "claimed_by": by, "claimed_at": claimedAt, "rewards": d.Rewards}, nil
	})
}

// Stats returns claim counts per published achievement for the dashboard. It
// aggregates only; no player identifiers or PII leave the query.
func (h Handler) Stats(c *gin.Context) {
	projectID, envID := platform.Owned(c)
	rows, err := h.DB.Query(c, `SELECT d.key,d.name,d.hidden,COUNT(cl.id),COUNT(cl.id) FILTER (WHERE cl.claimed_by='player'),COUNT(cl.id) FILTER (WHERE cl.claimed_by='server')
		FROM achievement_definitions d LEFT JOIN achievement_claims cl ON cl.achievement_id=d.id
		WHERE d.project_id=$1 AND d.environment_id=$2 AND d.active GROUP BY d.id ORDER BY d.sort_order,d.key`, projectID, envID)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer rows.Close()
	out := []gin.H{}
	for rows.Next() {
		var key, name string
		var hidden bool
		var total, byPlayer, byServer int64
		if err = rows.Scan(&key, &name, &hidden, &total, &byPlayer, &byServer); err != nil {
			platform.Internal(c, err)
			return
		}
		out = append(out, gin.H{"key": key, "name": name, "hidden": hidden, "claims": total, "claimed_by_player": byPlayer, "claimed_by_server": byServer})
	}
	if err = rows.Err(); err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, out)
}
