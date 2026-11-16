// Package progression owns XP and levels: versioned cumulative thresholds, per-player
// version pinning at the first award, and trusted XP awards.
package progression

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/platform"
	"realmstack-backend/internal/totals"
)

// Handler serves progression routes.
type Handler struct{ *platform.Env }

// LevelFor returns the level for cumulative xp: level 1 at 0 XP, plus one for every
// threshold (levels 2+) reached.
func LevelFor(thresholds []int64, xp int64) int {
	level := 1
	for _, threshold := range thresholds {
		if xp < threshold {
			break
		}
		level++
	}
	return level
}

// Award is the result of an XP award.
type Award struct {
	TotalXP            int64  `json:"total_xp"`
	Awarded            int64  `json:"awarded"`
	Level              int    `json:"level"`
	LeveledUp          bool   `json:"leveled_up"`
	ProgressionVersion int    `json:"progression_version"`
	VersionID          string `json:"progression_version_id"`
}

// AwardXP adds amount to the player's XP inside tx, tagged with source. A new player
// pins to the latest progression version; an existing player keeps their pinned one,
// which RETURNING reports. Callers must hold the player row lock (platform.LockPlayer).
func AwardXP(ctx context.Context, tx pgx.Tx, scope platform.Principal, playerID string, amount int64, source, reason, reference, key string, at time.Time) (Award, error) {
	if amount <= 0 || amount > 1e9 {
		return Award{}, platform.Errf(http.StatusUnprocessableEntity, "VALIDATION_ERROR", "XP amount must be positive and within limits.")
	}
	var latestID string
	err := tx.QueryRow(ctx, `SELECT id::text FROM progression_versions WHERE project_id=$1 AND environment_id=$2 ORDER BY version DESC LIMIT 1`, scope.ProjectID, scope.EnvironmentID).Scan(&latestID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Award{}, platform.Errf(http.StatusUnprocessableEntity, "PROGRESSION_NOT_PUBLISHED", "Publish progression thresholds before awarding XP.")
	}
	if err != nil {
		return Award{}, err
	}
	a := Award{Awarded: amount}
	err = tx.QueryRow(ctx, `INSERT INTO player_progression(player_id,project_id,environment_id,progression_version_id,total_xp) VALUES($1,$2,$3,$4,$5)
		ON CONFLICT(player_id) DO UPDATE SET total_xp=player_progression.total_xp+EXCLUDED.total_xp,updated_at=now()
		RETURNING total_xp,progression_version_id::text`, playerID, scope.ProjectID, scope.EnvironmentID, latestID, amount).Scan(&a.TotalXP, &a.VersionID)
	if err != nil {
		return Award{}, err
	}
	var raw []byte
	if err = tx.QueryRow(ctx, `SELECT thresholds,version FROM progression_versions WHERE id=$1`, a.VersionID).Scan(&raw, &a.ProgressionVersion); err != nil {
		return Award{}, err
	}
	var thresholds []int64
	_ = json.Unmarshal(raw, &thresholds)
	a.Level = LevelFor(thresholds, a.TotalXP)
	a.LeveledUp = a.Level > LevelFor(thresholds, a.TotalXP-amount)
	_, err = tx.Exec(ctx, `INSERT INTO xp_transactions(player_id,project_id,environment_id,progression_version_id,amount,reason,reference,idempotency_key,request_hash,source,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		playerID, scope.ProjectID, scope.EnvironmentID, a.VersionID, amount, reason, reference, key, platform.Digest(fmt.Sprint(amount, reason, reference)), source, at)
	if err != nil || source != "api" {
		return a, err
	}
	return a, totals.Add(ctx, tx, scope, playerID, at, totals.Delta{Metric: totals.XP, Amount: amount})
}

// AwardHandler is the trusted-server XP award route.
func (h Handler) AwardHandler(c *gin.Context) {
	var in struct {
		Amount    int64  `json:"amount"`
		Reason    string `json:"reason"`
		Reference string `json:"reference"`
	}
	if !platform.Bind(c, &in) {
		return
	}
	if in.Amount <= 0 || in.Amount > 1e9 {
		platform.Fail(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "XP amount must be positive and within limits.")
		return
	}
	actor, ok := platform.ServerTarget(c, h.DB)
	if !ok {
		return
	}
	h.Mutate(c, actor, in, func(tx pgx.Tx) (int, any, error) {
		if err := platform.LockPlayer(c, tx, actor, actor.ID); err != nil {
			return 0, nil, err
		}
		a, err := AwardXP(c, tx, actor, actor.ID, in.Amount, "api", in.Reason, in.Reference, platform.IdempotencyKey(c), h.Now())
		return http.StatusOK, a, err
	})
}

// Get returns the player's XP, level and pinned version.
func (h Handler) Get(c *gin.Context) {
	p := platform.GetPrincipal(c)
	var xp int64
	var raw []byte
	var version int
	err := h.DB.QueryRow(c, `SELECT pp.total_xp,pv.thresholds,pv.version FROM player_progression pp JOIN progression_versions pv ON pv.id=pp.progression_version_id
		WHERE pp.player_id=$1 AND pp.project_id=$2 AND pp.environment_id=$3`, p.ID, p.ProjectID, p.EnvironmentID).Scan(&xp, &raw, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		platform.Success(c, http.StatusOK, gin.H{"total_xp": int64(0), "level": 1, "version": nil})
		return
	}
	if err != nil {
		platform.Internal(c, err)
		return
	}
	var thresholds []int64
	_ = json.Unmarshal(raw, &thresholds)
	platform.Success(c, http.StatusOK, gin.H{"total_xp": xp, "level": LevelFor(thresholds, xp), "version": version, "thresholds": thresholds})
}
