// Package bans lets a dashboard admin ban a player of an environment they own, for a
// while or permanently, and lift the ban again.
//
// A ban is enforced where a player proves who they are, not inside each feature:
//   - login and refresh refuse a banned player (players.Login/Refresh call Active);
//   - every route that accepts a player access token refuses it (httpapi's playerAuth
//     asks Cache), because access tokens are stateless and stay valid for up to 15
//     minutes after they were issued;
//   - banning revokes every refresh token, so no session survives the ban.
//
// What a ban does not touch: the trusted game server and the dashboard can still act on
// the player (to settle a match, or to correct a balance), and the player's existing
// leaderboard scores stay on the boards. Hiding them would require the optional Redis
// projection to drop and restore entries.
package bans

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/platform"
)

// Ban is a ban in force, as a player sees it.
type Ban struct {
	ID        string     `json:"id"`
	Reason    string     `json:"reason"`
	ExpiresAt *time.Time `json:"expires_at"` // nil: permanent
	CreatedAt time.Time  `json:"created_at"`
}

// inForce reports whether b still applies at now.
func (b Ban) inForce(now time.Time) bool { return b.ExpiresAt == nil || now.Before(*b.ExpiresAt) }

// Active returns the player's ban in force at now, or nil. An open ban whose expiry has
// passed is not in force; it stays open in the table until a new ban replaces it, which
// is harmless and avoids a cleanup job.
func Active(ctx context.Context, q platform.Querier, playerID string, now time.Time) (*Ban, error) {
	var b Ban
	err := q.QueryRow(ctx, `SELECT id::text,reason,expires_at,created_at FROM player_bans
		WHERE player_id=$1 AND ended_at IS NULL AND (expires_at IS NULL OR expires_at > $2)`, playerID, now).
		Scan(&b.ID, &b.Reason, &b.ExpiresAt, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// Denied answers 403 PLAYER_BANNED. The reason and expiry are part of the response, so a
// game can tell the player why and until when.
func Denied(c *gin.Context, b *Ban) {
	platform.FailDetails(c, http.StatusForbidden, "PLAYER_BANNED", "This player account is banned.", gin.H{"reason": b.Reason, "expires_at": b.ExpiresAt})
}

// Handler serves the dashboard's ban routes under /players/:playerId (behind
// playeradmin.Target, so the principal is the player, kind admin).
type Handler struct {
	*platform.Env
	// Cache is told about every ban change on this instance, so enforcement here is
	// immediate; other instances pick the change up on their next sync.
	Cache *Cache
}

// Record is one row of a player's ban history.
type Record struct {
	ID        string     `json:"id"`
	Reason    string     `json:"reason"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedAt time.Time  `json:"created_at"`
	CreatedBy string     `json:"created_by"` // the dashboard user's email
	EndedAt   *time.Time `json:"ended_at"`
	EndedBy   *string    `json:"ended_by"`
	EndKind   *string    `json:"end_kind"` // lifted, replaced
	EndNote   string     `json:"end_note"`
	// Status is derived at read time: active, expired, lifted or replaced.
	Status string `json:"status"`
}

const recordColumns = `b.id::text,b.reason,b.expires_at,b.created_at,cu.email,b.ended_at,eu.email,b.end_kind,b.end_note`

func scanRecord(row pgx.Row, now time.Time) (Record, error) {
	var r Record
	err := row.Scan(&r.ID, &r.Reason, &r.ExpiresAt, &r.CreatedAt, &r.CreatedBy, &r.EndedAt, &r.EndedBy, &r.EndKind, &r.EndNote)
	switch {
	case r.EndKind != nil:
		r.Status = *r.EndKind
	case r.ExpiresAt != nil && !now.Before(*r.ExpiresAt):
		r.Status = "expired"
	default:
		r.Status = "active"
	}
	return r, err
}

func record(ctx context.Context, q platform.Querier, id string, now time.Time) (Record, error) {
	return scanRecord(q.QueryRow(ctx, `SELECT `+recordColumns+` FROM player_bans b
		JOIN users cu ON cu.id=b.created_by LEFT JOIN users eu ON eu.id=b.ended_by WHERE b.id=$1`, id), now)
}

type banInput struct {
	Reason    string     `json:"reason"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// Ban bans the player. Banning an already banned player replaces the ban (to extend,
// shorten or reword it); the old row is kept as "replaced". Every refresh token of the
// player is revoked in the same transaction.
func (h Handler) Ban(c *gin.Context) {
	var in banInput
	if !platform.Bind(c, &in) {
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	var errs platform.FieldErrors
	if in.Reason == "" || len([]rune(in.Reason)) > 500 {
		errs.Add("reason", "Give a reason of 1 to 500 characters; the player sees it.")
	}
	if len(errs) > 0 {
		platform.Respond(c, platform.Invalid("VALIDATION_ERROR", "The ban is invalid.", errs))
		return
	}
	actor := platform.GetPrincipal(c)
	user := platform.DashboardUser(c)
	h.Mutate(c, actor, in, func(tx pgx.Tx) (int, any, error) {
		now := h.Now()
		if in.ExpiresAt != nil && !in.ExpiresAt.After(now) {
			return 0, nil, platform.Invalid("VALIDATION_ERROR", "The ban is invalid.", []platform.FieldError{{Field: "expires_at", Message: "The expiry must be in the future; omit it for a permanent ban."}})
		}
		if err := platform.LockPlayer(c, tx, actor, actor.ID); err != nil {
			return 0, nil, err
		}
		// End the open ban first: the partial unique index allows one open ban per player.
		if _, err := tx.Exec(c, `UPDATE player_bans SET ended_at=$2,ended_by=$3,end_kind='replaced',updated_at=now() WHERE player_id=$1 AND ended_at IS NULL`, actor.ID, now, user); err != nil {
			return 0, nil, err
		}
		var id string
		if err := tx.QueryRow(c, `INSERT INTO player_bans(player_id,project_id,environment_id,reason,expires_at,created_by,created_at) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id::text`,
			actor.ID, actor.ProjectID, actor.EnvironmentID, in.Reason, in.ExpiresAt, user, now).Scan(&id); err != nil {
			return 0, nil, err
		}
		tag, err := tx.Exec(c, `UPDATE refresh_tokens SET revoked_at=now() WHERE player_id=$1 AND revoked_at IS NULL`, actor.ID)
		if err != nil {
			return 0, nil, err
		}
		r, err := record(c, tx, id, now)
		return http.StatusCreated, gin.H{"ban": r, "sessions_revoked": tag.RowsAffected()}, err
	})
	h.refresh(c, actor.ID)
}

type liftInput struct {
	Note string `json:"note"`
}

// Lift ends the player's ban in force. 422 NOT_BANNED when there is none (including a
// ban that has already expired). Revoked sessions stay revoked: the player signs in again.
func (h Handler) Lift(c *gin.Context) {
	var in liftInput
	if !platform.BindOptional(c, &in) {
		return
	}
	in.Note = strings.TrimSpace(in.Note)
	if len([]rune(in.Note)) > 500 {
		platform.Respond(c, platform.Invalid("VALIDATION_ERROR", "The note is invalid.", []platform.FieldError{{Field: "note", Message: "Keep the note under 500 characters."}}))
		return
	}
	actor := platform.GetPrincipal(c)
	user := platform.DashboardUser(c)
	h.Mutate(c, actor, in, func(tx pgx.Tx) (int, any, error) {
		now := h.Now()
		if err := platform.LockPlayer(c, tx, actor, actor.ID); err != nil {
			return 0, nil, err
		}
		var id string
		err := tx.QueryRow(c, `UPDATE player_bans SET ended_at=$2,ended_by=$3,end_kind='lifted',end_note=$4,updated_at=now()
			WHERE player_id=$1 AND ended_at IS NULL AND (expires_at IS NULL OR expires_at > $2) RETURNING id::text`, actor.ID, now, user, in.Note).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil, platform.Errf(http.StatusUnprocessableEntity, "NOT_BANNED", "The player is not banned.")
		}
		if err != nil {
			return 0, nil, err
		}
		r, err := record(c, tx, id, now)
		return http.StatusOK, gin.H{"ban": r}, err
	})
	h.refresh(c, actor.ID)
}

// refresh re-reads the player's ban into this instance's cache after a ban or lift. It
// reads the database rather than trusting the response, because an idempotent replay
// returns the stored result of an old request, which may no longer be true.
func (h Handler) refresh(c *gin.Context, playerID string) {
	if c.Writer.Status() >= http.StatusMultipleChoices {
		return
	}
	if err := h.Cache.Refresh(c, playerID); err != nil {
		// The ban is committed; other instances and the next sync here will apply it.
		h.Log.Error("ban cache refresh failed", "request_id", c.GetString("request_id"), "error", err)
	}
}

// History lists the player's bans, newest first (at most 100). A replacement is created at
// the instant the ban it replaces ends, so two bans can share created_at; the one still
// open (or that ended later) is the newer one.
func (h Handler) History(c *gin.Context) {
	p := platform.GetPrincipal(c)
	now := h.Now()
	rows, err := h.DB.Query(c, `SELECT `+recordColumns+` FROM player_bans b
		JOIN users cu ON cu.id=b.created_by LEFT JOIN users eu ON eu.id=b.ended_by
		WHERE b.player_id=$1 AND b.project_id=$2 AND b.environment_id=$3
		ORDER BY b.created_at DESC,COALESCE(b.ended_at,'infinity') DESC,b.id DESC LIMIT 100`, p.ID, p.ProjectID, p.EnvironmentID)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		r, err := scanRecord(rows, now)
		if err != nil {
			platform.Internal(c, err)
			return
		}
		out = append(out, r)
	}
	if err = rows.Err(); err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, out)
}
