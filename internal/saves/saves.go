// Package saves stores cloud saves: a few named JSON slots per player ("slot1",
// "autosave"), so a player can continue on another device.
//
// Saves are mutable state, not a ledger: a write replaces the slot. Each slot carries a
// version that goes up by one on every write. A client sends the version it last read as
// expected_version; if another device wrote in between, the write is refused with 409
// SAVE_VERSION_CONFLICT and the current version, and the game decides how to merge. A
// write without expected_version always wins (last write wins), for games that do not
// need the check.
//
// The same handlers serve the player (their own saves, player token), the trusted game
// server (any player in its environment) and the dashboard (playeradmin.Target), so the
// three can never disagree. Saves written by the game client are client-authoritative:
// a game that must not trust the client writes saves only from its server.
package saves

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/platform"
)

const (
	// MaxSlots is how many save slots one player may have.
	MaxSlots = 10
	// MaxBytes is the largest save, measured as compacted JSON. The 1 MiB body limit
	// leaves room for the envelope; larger saves (or binary data) belong in object storage.
	MaxBytes = 256 << 10
)

// Handler serves the save routes for players, the game server and the dashboard.
type Handler struct{ *platform.Env }

// Summary describes a slot without its data, so listing slots stays cheap.
type Summary struct {
	Slot      string    `json:"slot"`
	Version   int64     `json:"version"`
	SizeBytes int       `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// target is the player whose saves the request reads or writes: the signed-in player,
// the player managed from the dashboard (Target already set them as the principal), or,
// for a server credential, the :playerId in the path after the scope check.
func (h Handler) target(c *gin.Context) (platform.Principal, bool) {
	p := platform.GetPrincipal(c)
	if p.Kind == platform.KindServer {
		return platform.ServerTarget(c, h.DB)
	}
	return p, true
}

// slot validates the :slot parameter (the same format as catalog keys).
func slot(c *gin.Context) (string, bool) {
	s := c.Param("slot")
	return s, platform.KeyPattern.MatchString(s)
}

func limitsMeta() gin.H { return gin.H{"max_slots": MaxSlots, "max_bytes": MaxBytes} }

// List returns the player's slots (without data), ordered by name. meta carries the
// limits so a client can show how much room is left.
func (h Handler) List(c *gin.Context) {
	p, ok := h.target(c)
	if !ok {
		return
	}
	rows, err := h.DB.Query(c, `SELECT slot,version,size_bytes,created_at,updated_at FROM player_saves
		WHERE player_id=$1 AND project_id=$2 AND environment_id=$3 ORDER BY slot`, p.ID, p.ProjectID, p.EnvironmentID)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer rows.Close()
	out := []Summary{}
	for rows.Next() {
		var s Summary
		if err = rows.Scan(&s.Slot, &s.Version, &s.SizeBytes, &s.CreatedAt, &s.UpdatedAt); err != nil {
			platform.Internal(c, err)
			return
		}
		out = append(out, s)
	}
	if err = rows.Err(); err != nil {
		platform.Internal(c, err)
		return
	}
	platform.SuccessMeta(c, http.StatusOK, out, limitsMeta())
}

// Get returns one slot with its data.
func (h Handler) Get(c *gin.Context) {
	p, ok := h.target(c)
	if !ok {
		return
	}
	name, valid := slot(c)
	if !valid {
		notFound(c)
		return
	}
	var s Summary
	var data []byte
	err := h.DB.QueryRow(c, `SELECT slot,version,size_bytes,created_at,updated_at,data FROM player_saves
		WHERE player_id=$1 AND project_id=$2 AND environment_id=$3 AND slot=$4`, p.ID, p.ProjectID, p.EnvironmentID, name).
		Scan(&s.Slot, &s.Version, &s.SizeBytes, &s.CreatedAt, &s.UpdatedAt, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		notFound(c)
		return
	}
	if err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, gin.H{"slot": s.Slot, "version": s.Version, "size_bytes": s.SizeBytes,
		"created_at": s.CreatedAt, "updated_at": s.UpdatedAt, "data": json.RawMessage(data)})
}

func notFound(c *gin.Context) {
	platform.Fail(c, http.StatusNotFound, "NOT_FOUND", "No save is stored in that slot.")
}

func conflict(current int64) error {
	return &platform.Error{Status: http.StatusConflict, Code: "SAVE_VERSION_CONFLICT",
		Message: "The save changed since the version you read; load it again before writing.", Details: gin.H{"current_version": current}}
}

type putInput struct {
	Data            json.RawMessage `json:"data"`
	ExpectedVersion *int64          `json:"expected_version"`
}

// Put creates or replaces a slot (201 when created, 200 when replaced) and returns its
// summary. The data is any JSON value except null.
func (h Handler) Put(c *gin.Context) {
	name, valid := slot(c)
	if !valid {
		platform.Respond(c, platform.Invalid("VALIDATION_ERROR", "The slot name is invalid.", []platform.FieldError{{Field: "slot", Message: "Use 1 to 63 lowercase letters, digits, - or _, starting with a letter or digit."}}))
		return
	}
	var in putInput
	if !platform.Bind(c, &in) {
		return
	}
	// Compacting makes the size independent of the client's formatting.
	var compact bytes.Buffer
	if len(in.Data) == 0 || bytes.Equal(bytes.TrimSpace(in.Data), []byte("null")) || json.Compact(&compact, in.Data) != nil {
		platform.Respond(c, platform.Invalid("VALIDATION_ERROR", "The save is invalid.", []platform.FieldError{{Field: "data", Message: "Send the save as a JSON value (not null)."}}))
		return
	}
	if compact.Len() > MaxBytes {
		platform.Respond(c, &platform.Error{Status: http.StatusUnprocessableEntity, Code: "SAVE_TOO_LARGE",
			Message: "The save is larger than the limit.", Details: gin.H{"size_bytes": compact.Len(), "max_bytes": MaxBytes}})
		return
	}
	if in.ExpectedVersion != nil && *in.ExpectedVersion < 0 {
		platform.Respond(c, platform.Invalid("VALIDATION_ERROR", "The save is invalid.", []platform.FieldError{{Field: "expected_version", Message: "Use the version you last read, or 0 to create the slot."}}))
		return
	}
	p, ok := h.target(c)
	if !ok {
		return
	}
	data, size := compact.Bytes(), compact.Len()
	h.Mutate(c, p, gin.H{"slot": name, "data": json.RawMessage(data), "expected_version": in.ExpectedVersion}, func(tx pgx.Tx) (int, any, error) {
		// The player lock serializes this player's writes, which makes the slot count
		// check below exact (two devices cannot both create the eleventh slot).
		if err := platform.LockPlayer(c, tx, p, p.ID); err != nil {
			return 0, nil, err
		}
		now := h.Now()
		var current int64
		err := tx.QueryRow(c, `SELECT version FROM player_saves WHERE player_id=$1 AND slot=$2`, p.ID, name).Scan(&current)
		var s Summary
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if in.ExpectedVersion != nil && *in.ExpectedVersion != 0 {
				return 0, nil, conflict(0) // the client expected a slot that was deleted
			}
			var count int
			if err = tx.QueryRow(c, `SELECT count(*) FROM player_saves WHERE player_id=$1`, p.ID).Scan(&count); err != nil {
				return 0, nil, err
			}
			if count >= MaxSlots {
				return 0, nil, &platform.Error{Status: http.StatusUnprocessableEntity, Code: "SAVE_SLOT_LIMIT",
					Message: "The player already uses every save slot; delete one first.", Details: gin.H{"max_slots": MaxSlots}}
			}
			err = tx.QueryRow(c, `INSERT INTO player_saves(player_id,project_id,environment_id,slot,data,version,size_bytes,created_at,updated_at)
				VALUES($1,$2,$3,$4,$5,1,$6,$7,$7) RETURNING slot,version,size_bytes,created_at,updated_at`,
				p.ID, p.ProjectID, p.EnvironmentID, name, data, size, now).Scan(&s.Slot, &s.Version, &s.SizeBytes, &s.CreatedAt, &s.UpdatedAt)
			return http.StatusCreated, s, err
		case err != nil:
			return 0, nil, err
		}
		if in.ExpectedVersion != nil && *in.ExpectedVersion != current {
			return 0, nil, conflict(current)
		}
		err = tx.QueryRow(c, `UPDATE player_saves SET data=$3,size_bytes=$4,version=version+1,updated_at=$5
			WHERE player_id=$1 AND slot=$2 RETURNING slot,version,size_bytes,created_at,updated_at`,
			p.ID, name, data, size, now).Scan(&s.Slot, &s.Version, &s.SizeBytes, &s.CreatedAt, &s.UpdatedAt)
		return http.StatusOK, s, err
	})
}

// Delete removes a slot. ?expected_version= guards it like a write.
func (h Handler) Delete(c *gin.Context) {
	name, valid := slot(c)
	if !valid {
		notFound(c)
		return
	}
	var expected *int64
	if raw := c.Query("expected_version"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 1 {
			platform.Respond(c, platform.Invalid("VALIDATION_ERROR", "expected_version is invalid.", []platform.FieldError{{Field: "expected_version", Message: "Use the version you last read."}}))
			return
		}
		expected = &v
	}
	p, ok := h.target(c)
	if !ok {
		return
	}
	h.Mutate(c, p, gin.H{"slot": name, "expected_version": expected}, func(tx pgx.Tx) (int, any, error) {
		var current int64
		err := tx.QueryRow(c, `SELECT version FROM player_saves WHERE player_id=$1 AND slot=$2 FOR UPDATE`, p.ID, name).Scan(&current)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil, platform.Errf(http.StatusNotFound, "NOT_FOUND", "No save is stored in that slot.")
		}
		if err != nil {
			return 0, nil, err
		}
		if expected != nil && *expected != current {
			return 0, nil, conflict(current)
		}
		if _, err = tx.Exec(c, `DELETE FROM player_saves WHERE player_id=$1 AND slot=$2`, p.ID, name); err != nil {
			return 0, nil, err
		}
		return http.StatusOK, gin.H{"slot": name, "deleted": true, "version": current}, nil
	})
}
