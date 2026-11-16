// Package leaderboards records score submissions and computes deterministic rankings
// from PostgreSQL: best score per player, earliest best breaks ties, seasons are
// start-inclusive and end-exclusive.
package leaderboards

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/platform"
)

// Handler serves leaderboard routes.
type Handler struct{ *platform.Env }

// Submit records a player's score.
func (h Handler) Submit(c *gin.Context) {
	var in struct {
		Score int64 `json:"score"`
	}
	if !platform.Bind(c, &in) {
		return
	}
	p := platform.GetPrincipal(c)
	key := c.Param("key")
	h.Mutate(c, p, gin.H{"key": key, "score": in.Score}, func(tx pgx.Tx) (int, any, error) {
		var id string
		var start, end *time.Time
		err := tx.QueryRow(c, `SELECT id::text,season_start,season_end FROM leaderboard_definitions WHERE project_id=$1 AND environment_id=$2 AND key=$3 AND active`,
			p.ProjectID, p.EnvironmentID, key).Scan(&id, &start, &end)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil, platform.Errf(http.StatusNotFound, "NOT_FOUND", "The leaderboard was not found.")
		}
		if err != nil {
			return 0, nil, err
		}
		// The same timestamp decides the season check and is stored, so an accepted
		// score always falls inside the season it was accepted for.
		now := h.Now().UTC()
		if start != nil && now.Before(*start) || end != nil && !now.Before(*end) {
			return 0, nil, platform.Errf(http.StatusUnprocessableEntity, "LEADERBOARD_CLOSED", "The leaderboard is not accepting scores.")
		}
		_, err = tx.Exec(c, `INSERT INTO score_submissions(leaderboard_id,player_id,project_id,environment_id,score,idempotency_key,request_hash,submitted_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			id, p.ID, p.ProjectID, p.EnvironmentID, in.Score, platform.IdempotencyKey(c), platform.Digest(fmt.Sprint(id, in.Score)), now)
		return http.StatusCreated, gin.H{"leaderboard_key": key, "score": in.Score, "submitted_at": now}, err
	})
}

// bestScores ranks one best submission per player inside the board's season.
// order is "DESC" or "ASC" from a fixed allowlist, never user input.
const bestScores = `WITH best AS (
	SELECT DISTINCT ON (ss.player_id) ss.player_id,ss.score,ss.submitted_at FROM score_submissions ss
	WHERE ss.leaderboard_id=$1 AND ($2::timestamptz IS NULL OR ss.submitted_at >= $2) AND ($3::timestamptz IS NULL OR ss.submitted_at < $3)
	ORDER BY ss.player_id,ss.score %[1]s,ss.submitted_at ASC,ss.id ASC),
ranked AS (SELECT player_id,score,submitted_at,ROW_NUMBER() OVER(ORDER BY score %[1]s,submitted_at ASC,player_id ASC) AS rank FROM best)`

// Get returns the top entries and, for a player token, that player's rank.
func (h Handler) Get(c *gin.Context) {
	scope := platform.GetScope(c)
	var boardID, direction string
	var start, end *time.Time
	err := h.DB.QueryRow(c, `SELECT id::text,score_direction,season_start,season_end FROM leaderboard_definitions WHERE project_id=$1 AND environment_id=$2 AND key=$3 AND active`,
		scope.ProjectID, scope.EnvironmentID, c.Param("key")).Scan(&boardID, &direction, &start, &end)
	if errors.Is(err, pgx.ErrNoRows) {
		platform.NotFound(c)
		return
	}
	if err != nil {
		platform.Internal(c, err)
		return
	}
	limit := 10
	if n, e := strconv.Atoi(c.DefaultQuery("limit", "10")); e == nil && n > 0 {
		limit = min(n, 100)
	}
	order := "DESC"
	if direction == "lower" {
		order = "ASC"
	}
	rows, err := h.DB.Query(c, fmt.Sprintf(bestScores, order)+` SELECT r.player_id::text,r.score,r.submitted_at,pl.display_name,r.rank FROM ranked r JOIN players pl ON pl.id=r.player_id ORDER BY r.rank LIMIT $4`,
		boardID, start, end, limit)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	entries := []gin.H{}
	for rows.Next() {
		var id, name string
		var score, rank int64
		var at time.Time
		if err = rows.Scan(&id, &score, &at, &name, &rank); err != nil {
			rows.Close()
			platform.Internal(c, err)
			return
		}
		entries = append(entries, gin.H{"player_id": id, "display_name": name, "score": score, "rank": rank, "submitted_at": at})
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		platform.Internal(c, err)
		return
	}
	result := gin.H{"key": c.Param("key"), "entries": entries}
	if platform.HasPrincipal(c) {
		var rank, score int64
		err = h.DB.QueryRow(c, fmt.Sprintf(bestScores, order)+` SELECT rank,score FROM ranked WHERE player_id=$4`, boardID, start, end, platform.GetPrincipal(c).ID).Scan(&rank, &score)
		switch {
		case err == nil:
			result["player_rank"], result["player_score"] = rank, score
		case !errors.Is(err, pgx.ErrNoRows):
			platform.Internal(c, err)
			return
		}
	}
	platform.Success(c, http.StatusOK, result)
}
