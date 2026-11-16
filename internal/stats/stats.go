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

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/platform"
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
		id, err := definition(c, tx, actor, key, "counter")
		if err != nil {
			return 0, nil, err
		}
		var value int64
		err = tx.QueryRow(c, `INSERT INTO player_stats(player_id,project_id,environment_id,stat_definition_id,value) VALUES($1,$2,$3,$4,$5)
			ON CONFLICT(player_id,stat_definition_id) DO UPDATE SET value=player_stats.value+EXCLUDED.value,updated_at=now() RETURNING value`,
			actor.ID, actor.ProjectID, actor.EnvironmentID, id, in.Amount).Scan(&value)
		if err != nil {
			return 0, nil, err
		}
		_, err = tx.Exec(c, `INSERT INTO stat_transactions(player_id,project_id,environment_id,stat_definition_id,operation,amount,reason,reference) VALUES($1,$2,$3,$4,'increment',$5,$6,$7)`,
			actor.ID, actor.ProjectID, actor.EnvironmentID, id, in.Amount, in.Reason, in.Reference)
		return http.StatusOK, gin.H{"stat_key": key, "incremented": in.Amount, "value": value}, err
	})
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
		id, err := definition(c, tx, actor, key, "flag")
		if err != nil {
			return 0, nil, err
		}
		tag, err := tx.Exec(c, `INSERT INTO player_stats(player_id,project_id,environment_id,stat_definition_id,value) VALUES($1,$2,$3,$4,1) ON CONFLICT(player_id,stat_definition_id) DO NOTHING`,
			actor.ID, actor.ProjectID, actor.EnvironmentID, id)
		if err != nil {
			return 0, nil, err
		}
		newlySet := tag.RowsAffected() == 1
		if newlySet {
			if _, err = tx.Exec(c, `INSERT INTO stat_transactions(player_id,project_id,environment_id,stat_definition_id,operation,amount,reason,reference) VALUES($1,$2,$3,$4,'set_flag',1,$5,$6)`,
				actor.ID, actor.ProjectID, actor.EnvironmentID, id, in.Reason, in.Reference); err != nil {
				return 0, nil, err
			}
		}
		return http.StatusOK, gin.H{"flag_key": key, "set": true, "newly_set": newlySet}, nil
	})
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
