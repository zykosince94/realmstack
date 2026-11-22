// Package playeradmin is the dashboard's player management: a searchable list, a
// profile, the same player data the game sees (inventory, balances, ledger, progression,
// stats, achievements, quests), and the actions an admin performs on a player's behalf.
//
// Players never use the dashboard, so before this package the only way to see or change a
// player was to sign in as them (the test client) or to hold the game server's
// credential: every player route was either self-service (player token) or trusted
// server (server credential). An admin needs to look players up and act on them without
// either.
//
// Nothing here has its own business logic. Target checks that the player belongs to the
// dashboard user's own environment and then narrows the request to that player, which is
// all the existing handlers read. The read routes and the management routes (grants,
// currency, XP, stats, claims, quest accept/abandon/claim, score submission) are those
// handlers, registered under /players/:playerId (see routes.go). So the admin view and the
// game can never disagree, and an admin action follows every rule a game action does:
// balances cannot go negative, claims pay once, idempotency keys replay. Admin keys have
// their own idempotency namespace ("admin"). Corrections are ordinary ledger rows (the
// ledgers are append-only); there is no separate audit log yet.
package playeradmin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"realmstack-backend/internal/bans"
	"realmstack-backend/internal/economy"
	"realmstack-backend/internal/platform"
	"realmstack-backend/internal/progression"
)

// Handler serves the dashboard's player routes.
type Handler struct{ *platform.Env }

// Target runs after the dashboard ownership checks (RequireOwnedEnvironment). It answers
// 404 for a player outside the owned environment, and otherwise makes the player the
// request's principal (kind admin), scoped to that environment, for the read handlers.
func (h Handler) Target() gin.HandlerFunc {
	return func(c *gin.Context) {
		projectID, envID := platform.Owned(c)
		scope := platform.Principal{ProjectID: projectID, EnvironmentID: envID}
		playerID := c.Param("playerId")
		if !platform.PlayerInScope(c, h.DB, scope, playerID) {
			platform.NotFound(c)
			c.Abort()
			return
		}
		platform.SetDashboardUser(c, platform.GetPrincipal(c).ID) // the user principal from userAuth
		platform.SetPrincipal(c, platform.Principal{ID: playerID, ProjectID: projectID, EnvironmentID: envID, Kind: platform.KindAdmin})
		c.Next()
	}
}

// escapeLike makes a search term literal inside a LIKE pattern (backslash is the
// default escape character in PostgreSQL).
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// List returns the environment's players, newest first, cursor-paginated. search
// matches part of the display name, email or external key (case-insensitive), or a
// whole player ID.
func (h Handler) List(c *gin.Context) {
	projectID, envID := platform.Owned(c)
	limit := 25
	if n, err := strconv.Atoi(c.Query("limit")); err == nil && n > 0 && n <= 100 {
		limit = n
	}
	search := strings.TrimSpace(c.Query("search"))
	if len(search) > 100 {
		search = search[:100]
	}
	var beforeAt *time.Time
	var beforeID *string
	if cursor := c.Query("cursor"); cursor != "" {
		at, id, ok := economy.DecodeCursor(cursor)
		if !ok {
			platform.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "cursor is invalid.")
			return
		}
		beforeAt, beforeID = &at, &id
	}
	pattern := "%" + escapeLike(search) + "%"
	bannedOnly := c.Query("banned") == "true"
	// The ban lookup is the same predicate as bans.Active (open, not expired at the API clock).
	rows, err := h.DB.Query(c, `SELECT p.id::text,p.display_name,COALESCE(p.email,''),COALESCE(p.external_player_key,''),p.created_at,
			COALESCE(pp.total_xp,0),COALESCE(pv.thresholds,'[]'::jsonb),b.id IS NOT NULL
		FROM players p
		LEFT JOIN player_progression pp ON pp.player_id=p.id
		LEFT JOIN progression_versions pv ON pv.id=pp.progression_version_id
		LEFT JOIN player_bans b ON b.player_id=p.id AND b.ended_at IS NULL AND (b.expires_at IS NULL OR b.expires_at > $8)
		WHERE p.project_id=$1 AND p.environment_id=$2
		  AND ($3='' OR p.id::text=lower($3) OR p.display_name ILIKE $4 OR p.email ILIKE $4 OR p.external_player_key ILIKE $4)
		  AND ($5::timestamptz IS NULL OR (p.created_at,p.id) < ($5,$6::uuid))
		  AND (NOT $9 OR b.id IS NOT NULL)
		ORDER BY p.created_at DESC,p.id DESC LIMIT $7`, projectID, envID, search, pattern, beforeAt, beforeID, limit+1, h.Now(), bannedOnly)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	defer rows.Close()
	out := []gin.H{}
	var lastAt time.Time
	var lastID string
	more := false
	for rows.Next() {
		var id, name, email, external string
		var created time.Time
		var xp int64
		var raw []byte
		var banned bool
		if err = rows.Scan(&id, &name, &email, &external, &created, &xp, &raw, &banned); err != nil {
			platform.Internal(c, err)
			return
		}
		if len(out) == limit {
			more = true // the extra row only says that another page exists
			break
		}
		var thresholds []int64
		_ = json.Unmarshal(raw, &thresholds)
		out = append(out, gin.H{"id": id, "display_name": name, "email": nullable(email), "external_player_key": nullable(external),
			"created_at": created, "total_xp": xp, "level": progression.LevelFor(thresholds, xp), "banned": banned})
		lastAt, lastID = created, id
	}
	if err = rows.Err(); err != nil {
		platform.Internal(c, err)
		return
	}
	var meta gin.H
	if more {
		meta = gin.H{"next_cursor": economy.EncodeCursor(lastAt, lastID)}
	}
	platform.SuccessMeta(c, http.StatusOK, out, meta)
}

// Profile returns one player's identity and session summary. The password hash and
// refresh tokens are never read.
func (h Handler) Profile(c *gin.Context) {
	p := platform.GetPrincipal(c)
	var name, email, external string
	var metadata []byte
	var created time.Time
	var lastSession *time.Time
	var activeSessions int64
	err := h.DB.QueryRow(c, `SELECT p.display_name,COALESCE(p.email,''),COALESCE(p.external_player_key,''),p.metadata,p.created_at,
			(SELECT max(created_at) FROM refresh_tokens WHERE player_id=p.id),
			(SELECT count(*) FROM refresh_tokens WHERE player_id=p.id AND revoked_at IS NULL AND expires_at > $4)
		FROM players p WHERE p.id=$1 AND p.project_id=$2 AND p.environment_id=$3`, p.ID, p.ProjectID, p.EnvironmentID, h.Now()).
		Scan(&name, &email, &external, &metadata, &created, &lastSession, &activeSessions)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	ban, err := bans.Active(c, h.DB, p.ID, h.Now())
	if err != nil {
		platform.Internal(c, err)
		return
	}
	platform.Success(c, http.StatusOK, gin.H{"id": p.ID, "display_name": name, "email": nullable(email), "external_player_key": nullable(external),
		"metadata": json.RawMessage(metadata), "created_at": created, "last_session_at": lastSession, "active_sessions": activeSessions, "ban": ban})
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
