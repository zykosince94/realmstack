// Package leaderboards records score submissions and serves deterministic rankings
// from PostgreSQL: best score per player, earliest best breaks ties, seasons are
// start-inclusive and end-exclusive.
//
// Since Phase 8 rankings read leaderboard_best_scores, a per-player best maintained in
// the submission transaction, instead of recomputing best scores from every
// submission on each read. Top-N is an index scan and a player's rank is one indexed
// count of better entries (spec 6.6). RebuildBestScores recomputes the table from the
// submissions, which remain the source of truth.
package leaderboards

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/platform"
)

// Handler serves leaderboard routes.
type Handler struct{ *platform.Env }

// rankScore orders entries so that smaller is always better: the score itself on
// lower-is-better boards, its negation on higher-is-better ones. One ascending index
// then serves both directions.
func rankScore(direction string, score int64) int64 {
	if direction == "higher" {
		return -score
	}
	return score
}

// Submit records a player's score and updates their best in the same transaction.
func (h Handler) Submit(c *gin.Context) {
	var in struct {
		Score int64 `json:"score"`
	}
	if !platform.Bind(c, &in) {
		return
	}
	if in.Score == math.MinInt64 {
		platform.Fail(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "The score is out of range.")
		return
	}
	p := platform.GetPrincipal(c)
	key := c.Param("key")
	h.Mutate(c, p, gin.H{"key": key, "score": in.Score}, func(tx pgx.Tx) (int, any, error) {
		var id, direction string
		var start, end *time.Time
		err := tx.QueryRow(c, `SELECT id::text,score_direction,season_start,season_end FROM leaderboard_definitions WHERE project_id=$1 AND environment_id=$2 AND key=$3 AND active`,
			p.ProjectID, p.EnvironmentID, key).Scan(&id, &direction, &start, &end)
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
		var submissionID string
		if err = tx.QueryRow(c, `INSERT INTO score_submissions(leaderboard_id,player_id,project_id,environment_id,score,idempotency_key,request_hash,submitted_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id::text`,
			id, p.ID, p.ProjectID, p.EnvironmentID, in.Score, platform.IdempotencyKey(c), platform.Digest(fmt.Sprint(id, in.Score)), now).Scan(&submissionID); err != nil {
			return 0, nil, err
		}
		// Replace the best only with a strictly better entry, in exactly the order the
		// rebuild uses (score, then earlier time, then smaller submission id), so the
		// maintained table and a rebuild from submissions always agree.
		_, err = tx.Exec(c, `INSERT INTO leaderboard_best_scores(leaderboard_id,player_id,project_id,environment_id,best_score,rank_score,achieved_at,submission_id)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (leaderboard_id,player_id) DO UPDATE SET best_score=EXCLUDED.best_score,rank_score=EXCLUDED.rank_score,achieved_at=EXCLUDED.achieved_at,submission_id=EXCLUDED.submission_id
			WHERE (EXCLUDED.rank_score,EXCLUDED.achieved_at,EXCLUDED.submission_id) < (leaderboard_best_scores.rank_score,leaderboard_best_scores.achieved_at,leaderboard_best_scores.submission_id)`,
			id, p.ID, p.ProjectID, p.EnvironmentID, in.Score, rankScore(direction, in.Score), now, submissionID)
		return http.StatusCreated, gin.H{"leaderboard_key": key, "score": in.Score, "submitted_at": now}, err
	})
}

// Get returns the top entries and, for a player token, that player's rank. Reads go
// to the replica when one is configured: a ranking a moment stale is acceptable.
func (h Handler) Get(c *gin.Context) {
	scope := platform.GetScope(c)
	db := h.Reader()
	var boardID string
	err := db.QueryRow(c, `SELECT id::text FROM leaderboard_definitions WHERE project_id=$1 AND environment_id=$2 AND key=$3 AND active`,
		scope.ProjectID, scope.EnvironmentID, c.Param("key")).Scan(&boardID)
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
	// Top-N: an index scan on (leaderboard_id, rank_score, achieved_at, player_id).
	rows, err := db.Query(c, `SELECT b.player_id::text,b.best_score,b.achieved_at,pl.display_name FROM leaderboard_best_scores b JOIN players pl ON pl.id=b.player_id
		WHERE b.leaderboard_id=$1 ORDER BY b.rank_score,b.achieved_at,b.player_id LIMIT $2`, boardID, limit)
	if err != nil {
		platform.Internal(c, err)
		return
	}
	entries := []gin.H{}
	for rank := int64(1); rows.Next(); rank++ {
		var id, name string
		var score int64
		var at time.Time
		if err = rows.Scan(&id, &score, &at, &name); err != nil {
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
		rank, score, found, err := PlayerRank(c, db, boardID, platform.GetPrincipal(c).ID)
		if err != nil {
			platform.Internal(c, err)
			return
		}
		if found {
			result["player_rank"], result["player_score"] = rank, score
		}
	}
	platform.Success(c, http.StatusOK, result)
}

// PlayerRank is one indexed count of the entries that rank ahead of the player: the
// row comparison matches the index order, so PostgreSQL reads only that index range.
func PlayerRank(ctx context.Context, q platform.Querier, boardID, playerID string) (rank, score int64, found bool, err error) {
	err = q.QueryRow(ctx, `SELECT b.best_score,1+(SELECT count(*) FROM leaderboard_best_scores o WHERE o.leaderboard_id=b.leaderboard_id
		AND (o.rank_score,o.achieved_at,o.player_id) < (b.rank_score,b.achieved_at,b.player_id))
		FROM leaderboard_best_scores b WHERE b.leaderboard_id=$1 AND b.player_id=$2`, boardID, playerID).Scan(&score, &rank)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	return rank, score, err == nil, err
}

// RebuildBestScores recomputes leaderboard_best_scores from score_submissions with
// the same ordering the submit path uses. Submissions are locked against writes (reads
// continue) for the duration, so nothing can land between the delete and the insert.
func RebuildBestScores(ctx context.Context, db interface {
	Begin(context.Context) (pgx.Tx, error)
}) (int64, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `LOCK TABLE score_submissions IN SHARE MODE`); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM leaderboard_best_scores`); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO leaderboard_best_scores(leaderboard_id,player_id,project_id,environment_id,best_score,rank_score,achieved_at,submission_id) `+recomputeBest)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), tx.Commit(ctx)
}

// recomputeBest derives every player's best entry from the submissions, in the order
// the submit path uses: rank_score, then earliest time, then smallest submission id.
const recomputeBest = `SELECT DISTINCT ON (s.leaderboard_id,s.player_id) s.leaderboard_id,s.player_id,s.project_id,s.environment_id,s.score AS best_score,
	CASE WHEN d.score_direction='higher' THEN -s.score ELSE s.score END AS rank_score,s.submitted_at AS achieved_at,s.id AS submission_id
FROM score_submissions s JOIN leaderboard_definitions d ON d.id=s.leaderboard_id
ORDER BY s.leaderboard_id,s.player_id,CASE WHEN d.score_direction='higher' THEN -s.score ELSE s.score END,s.submitted_at,s.id`

// VerifyBestScores counts best-score rows that differ from a recomputation from the
// submissions, without changing anything. Zero means a rebuild would change nothing.
func VerifyBestScores(ctx context.Context, q platform.Querier) (int64, error) {
	var n int64
	err := q.QueryRow(ctx, `SELECT count(*) FROM leaderboard_best_scores b FULL OUTER JOIN (`+recomputeBest+`) r
		ON r.leaderboard_id=b.leaderboard_id AND r.player_id=b.player_id
		WHERE b.player_id IS NULL OR r.player_id IS NULL OR (r.best_score,r.rank_score,r.achieved_at,r.submission_id)<>(b.best_score,b.rank_score,b.achieved_at,b.submission_id)`).Scan(&n)
	return n, err
}
