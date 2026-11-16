// Package stats records game-world facts the platform cannot observe itself: named
// counters (trolls_defeated) and set-only flags (talked_to_elder). Only a trusted
// server credential can write them, so game clients cannot fake progress.
package stats

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/platform"
	"realmstack-backend/internal/totals"
)

// Handler serves stat and flag routes.
type Handler struct{ *platform.Env }

type write struct {
	Amount    int64  `json:"amount"`
	Reason    string `json:"reason"`
	Reference string `json:"reference"`
}

// definition loads an active stat definition of the expected kind.
func definition(ctx context.Context, tx pgx.Tx, scope platform.Principal, key, kind string) (string, error) {
	var id, got string
	err := tx.QueryRow(ctx, `SELECT id::text,kind FROM stat_definitions WHERE project_id=$1 AND environment_id=$2 AND key=$3 AND active`, scope.ProjectID, scope.EnvironmentID, key).Scan(&id, &got)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", platform.Errf(http.StatusNotFound, "NOT_FOUND", "No published %s is named %q.", kind, key)
	}
	if err != nil {
		return "", err
	}
	if got != kind {
		return "", platform.Errf(http.StatusUnprocessableEntity, "WRONG_STAT_KIND", "%q is a %s, not a %s.", key, got, kind)
	}
	return id, nil
}

// Increment adds a positive amount to a counter (trusted server only).
func (h Handler) Increment(c *gin.Context) {
	var in write
	if !platform.Bind(c, &in) {
		return
	}
	if in.Amount <= 0 || in.Amount > 1e9 {
		platform.Fail(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Amount must be a positive integer (counters never decrease).")
		return
	}
	actor, ok := platform.ServerTarget(c, h.DB)
	if !ok {
		return
	}
	key := c.Param("statKey")
	h.Mutate(c, actor, gin.H{"stat_key": key, "body": in}, func(tx pgx.Tx) (int, any, error) {
		value, err := IncrementTx(c, tx, actor, actor.ID, key, in.Amount, in.Reason, in.Reference, h.Now())
		return http.StatusOK, gin.H{"stat_key": key, "incremented": in.Amount, "value": value}, err
	})
}

// IncrementTx adds amount to a player's counter inside tx and writes the ledger row and
// goal total. It returns the new value. Used by the route and by batch writes.
func IncrementTx(ctx context.Context, tx pgx.Tx, scope platform.Principal, playerID, key string, amount int64, reason, reference string, at time.Time) (int64, error) {
	if amount <= 0 || amount > 1e9 {
		return 0, platform.Errf(http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Amount must be a positive integer (counters never decrease).")
	}
	id, err := definition(ctx, tx, scope, key, "counter")
	if err != nil {
		return 0, err
	}
	var value int64
	err = tx.QueryRow(ctx, `INSERT INTO player_stats(player_id,project_id,environment_id,stat_definition_id,value) VALUES($1,$2,$3,$4,$5)
		ON CONFLICT(player_id,stat_definition_id) DO UPDATE SET value=player_stats.value+EXCLUDED.value,updated_at=now() RETURNING value`,
		playerID, scope.ProjectID, scope.EnvironmentID, id, amount).Scan(&value)
	if err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO stat_transactions(player_id,project_id,environment_id,stat_definition_id,operation,amount,reason,reference,created_at) VALUES($1,$2,$3,$4,'increment',$5,$6,$7,$8)`,
		playerID, scope.ProjectID, scope.EnvironmentID, id, amount, reason, reference, at); err != nil {
		return 0, err
	}
	return value, totals.Add(ctx, tx, scope, playerID, at, totals.Delta{Metric: totals.Stat(key), Amount: amount})
}

// SetFlag sets a flag (trusted server only). Flags are set-only: setting an already
// set flag succeeds without writing a second ledger row, and nothing clears a flag.
func (h Handler) SetFlag(c *gin.Context) {
	var in write
	// The body is optional: a bare POST sets the flag.
	raw, err := c.GetRawData()
	if err != nil || (len(bytes.TrimSpace(raw)) > 0 && json.Unmarshal(raw, &in) != nil) {
		platform.BadRequest(c)
		return
	}
	actor, ok := platform.ServerTarget(c, h.DB)
	if !ok {
		return
	}
	key := c.Param("flagKey")
	h.Mutate(c, actor, gin.H{"flag_key": key, "reason": in.Reason, "reference": in.Reference}, func(tx pgx.Tx) (int, any, error) {
		newlySet, err := SetFlagTx(c, tx, actor, actor.ID, key, in.Reason, in.Reference, h.Now())
		return http.StatusOK, gin.H{"flag_key": key, "set": true, "newly_set": newlySet}, err
	})
}

// SetFlagTx sets a player's flag inside tx. Setting an already set flag succeeds
// without a second ledger row; it reports whether this call set it.
func SetFlagTx(ctx context.Context, tx pgx.Tx, scope platform.Principal, playerID, key, reason, reference string, at time.Time) (bool, error) {
	id, err := definition(ctx, tx, scope, key, "flag")
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO player_stats(player_id,project_id,environment_id,stat_definition_id,value) VALUES($1,$2,$3,$4,1) ON CONFLICT(player_id,stat_definition_id) DO NOTHING`,
		playerID, scope.ProjectID, scope.EnvironmentID, id)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO stat_transactions(player_id,project_id,environment_id,stat_definition_id,operation,amount,reason,reference,created_at) VALUES($1,$2,$3,$4,'set_flag',1,$5,$6,$7)`,
		playerID, scope.ProjectID, scope.EnvironmentID, id, reason, reference, at)
	return err == nil, err
}

func (h Handler) list(c *gin.Context, scope platform.Principal, playerID string) {
	rows, err := h.DB.Query(c, `SELECT d.key,d.name,d.kind,COALESCE(ps.value,0) FROM stat_definitions d
		LEFT JOIN player_stats ps ON ps.stat_definition_id=d.id AND ps.player_id=$1
		WHERE d.project_id=$2 AND d.environment_id=$3 AND d.active ORDER BY d.kind,d.key`, playerID, scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer rows.Close()
	counters, flags := []gin.H{}, []gin.H{}
	for rows.Next() {
		var key, name, kind string
		var value int64
		if err = rows.Scan(&key, &name, &kind, &value); err != nil {
			platform.Internal(c, err)
			return
		}
		if kind == "flag" {
			flags = append(flags, gin.H{"key": key, "name": name, "set": value > 0})
		} else {
			counters = append(counters, gin.H{"key": key, "name": name, "value": value})
		}
	}
	if err = rows.Err(); err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, gin.H{"counters": counters, "flags": flags})
}

// Mine returns the signed-in player's counters and flags.
func (h Handler) Mine(c *gin.Context) {
	p := platform.GetPrincipal(c)
	h.list(c, p, p.ID)
}

// ForPlayer returns any in-scope player's counters and flags (trusted server).
func (h Handler) ForPlayer(c *gin.Context) {
	actor, ok := platform.ServerTarget(c, h.DB)
	if !ok {
		return
	}
	h.list(c, actor, actor.ID)
}
