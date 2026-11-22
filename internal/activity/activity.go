// Package activity reports how many players are active in an environment, for the
// dashboard: concurrent players (CCU) over the last 24 hours and daily active players
// (DAU) over the last 30 days.
//
// Activity is derived from sessions, with no separate presence tracking. Every
// login, registration and token refresh inserts a refresh_tokens row, and a running game
// refreshes about every 14 minutes (access tokens live 15), so:
//
//   - a player counts toward CCU in a 15-minute bucket if a session was issued for them in
//     that bucket;
//   - a player counts toward DAU on a UTC day if a session was issued for them that day.
//
// This is an approximation. It depends on the client refreshing on schedule, a player
// whose game stops refreshing drops out within 15 minutes, and history only goes back as
// far as refresh tokens are kept (maintenance deletes them a day after they expire, so
// REFRESH_TOKEN_TTL bounds the DAU window; the default 30 days covers it). The queries
// scan refresh-token rows in the window, so on a very large tenant this read is heavy;
// it runs on the read replica when one is configured.
package activity

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"realmstack-backend/internal/platform"
)

// Handler serves the dashboard's activity route.
type Handler struct{ *platform.Env }

// BucketMinutes is the CCU resolution, matching the access-token lifetime.
const BucketMinutes = 15

type ccuPoint struct {
	At      time.Time `json:"at"`
	Players int64     `json:"players"`
}

type dauPoint struct {
	Date    string `json:"date"`
	Players int64  `json:"players"`
}

// The same scope predicate for every query: refresh_tokens has no tenant columns, so the
// player row carries the project and environment.
const scoped = `FROM refresh_tokens rt JOIN players p ON p.id=rt.player_id WHERE p.project_id=$1 AND p.environment_id=$2`

// Get returns CCU per complete 15-minute bucket for the last 24 hours (96 points, oldest
// first, zero-filled), DAU per UTC day for the last 30 days, and the current values.
func (h Handler) Get(c *gin.Context) {
	projectID, envID := platform.Owned(c)
	now := h.Now().UTC()
	b := &pgx.Batch{}
	// date_bin aligns buckets to quarter hours, so a bucket means the same thing on every
	// call. The series ends at the last complete bucket: the current one is still filling,
	// and drawing it would show a false drop at the end of the line ("now" is ccu_now).
	b.Queue(`WITH bounds AS (
			SELECT date_bin('15 minutes', $3::timestamptz, TIMESTAMPTZ '2000-01-01') - interval '15 minutes' AS hi),
		buckets AS (
			SELECT generate_series(hi - interval '1425 minutes', hi, interval '15 minutes') AS at FROM bounds),
		sessions AS (
			SELECT DISTINCT rt.player_id, date_bin('15 minutes', rt.created_at, TIMESTAMPTZ '2000-01-01') AS at `+scoped+`
			  AND rt.created_at >= (SELECT hi - interval '1425 minutes' FROM bounds) AND rt.created_at < (SELECT hi + interval '15 minutes' FROM bounds))
		SELECT b.at, count(s.player_id) FROM buckets b LEFT JOIN sessions s ON s.at=b.at GROUP BY b.at ORDER BY b.at`, projectID, envID, now)
	b.Queue(`WITH days AS (SELECT ($3::timestamptz AT TIME ZONE 'UTC')::date - g AS day FROM generate_series(0, 29) g),
		sessions AS (
			SELECT DISTINCT rt.player_id, (rt.created_at AT TIME ZONE 'UTC')::date AS day `+scoped+`
			  AND rt.created_at >= ((($3::timestamptz AT TIME ZONE 'UTC')::date - 29)::timestamp AT TIME ZONE 'UTC') AND rt.created_at <= $3)
		SELECT d.day::text, count(s.player_id) FROM days d LEFT JOIN sessions s ON s.day=d.day GROUP BY d.day ORDER BY d.day`, projectID, envID, now)
	// "Now" is a rolling window rather than the current (partial) bucket.
	b.Queue(`SELECT count(DISTINCT rt.player_id) `+scoped+` AND rt.created_at > $3::timestamptz - interval '15 minutes' AND rt.created_at <= $3`, projectID, envID, now)
	br := h.Reader().SendBatch(c, b)
	defer br.Close()

	ccu := []ccuPoint{}
	rows, err := br.Query()
	if err == nil {
		ccu, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (ccuPoint, error) {
			var p ccuPoint
			return p, r.Scan(&p.At, &p.Players)
		})
	}
	var dau []dauPoint
	if err == nil {
		if rows, err = br.Query(); err == nil {
			dau, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (dauPoint, error) {
				var p dauPoint
				return p, r.Scan(&p.Date, &p.Players)
			})
		}
	}
	var ccuNow int64
	if err == nil {
		err = br.QueryRow().Scan(&ccuNow)
	}
	if err != nil {
		platform.Internal(c, err)
		return
	}
	peak := ccuNow // the current window belongs to the last 24 hours too
	for _, p := range ccu {
		peak = max(peak, p.Players)
	}
	var today int64
	if len(dau) > 0 {
		today = dau[len(dau)-1].Players
	}
	platform.Success(c, http.StatusOK, gin.H{
		"generated_at": now, "bucket_minutes": BucketMinutes,
		"ccu_now": ccuNow, "ccu_peak_24h": peak, "dau_today": today,
		"ccu": ccu, "dau": dau,
	})
}
