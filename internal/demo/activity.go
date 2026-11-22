package demo

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// crowd are demo players that exist only to give the dashboard's activity chart (CCU and
// DAU) something to show. They have no password, so nobody can sign in as them.
var crowd = []string{
	"Aldric Thornvale", "Brynn Ashcombe", "Caius Emberlight", "Delphine Moorwood", "Eamon Ravensworth",
	"Freya Stormhollow", "Gideon Blackbriar", "Hestia Dawnmere", "Ivor Coldbrook", "Juno Silverfen",
	"Kael Duskwarden", "Lyra Wintermantle", "Magnus Oakenshield", "Nerys Brightwater", "Orin Frostgale",
	"Petra Hollowmere", "Quentin Ashfall", "Rowena Starfield", "Soren Grimwald", "Talia Moonbrook",
}

// SeedActivity gives the demo environment a believable activity history. Activity is
// derived from when sessions were issued (internal/activity), so the history is a set of
// already-revoked refresh-token rows for the crowd players: one per 15-minute bucket in
// which a player is "online".
//
// The pattern is deterministic (hashes of player and time, so the same history comes back
// on every run): each player plays on a share of days that depends on the player, and on
// a playing day is online more in the evening (UTC) and on weekends. The rows' token_hash
// starts with "demo-activity:", which a real SHA-256 digest never does: that marks them,
// and no presented token can ever match one. Each startup fills only the gap since the
// last seeded row, up to 30 days back, so the chart stays current without duplicates.
func SeedActivity(ctx context.Context, db *pgxpool.Pool, projectID, envID string) error {
	for i, name := range crowd {
		email := fmt.Sprintf("crowd%02d@realmstack.local", i+1)
		if _, err := db.Exec(ctx, `INSERT INTO players(project_id,environment_id,email,display_name) VALUES($1,$2,$3,$4)
			ON CONFLICT(project_id,environment_id,email) WHERE email IS NOT NULL DO NOTHING`, projectID, envID, email, name); err != nil {
			return err
		}
	}
	_, err := db.Exec(ctx, `
	WITH bounds AS (
		SELECT date_bin('15 minutes', GREATEST(now() - interval '30 days',
		         COALESCE((SELECT max(rt.created_at) FROM refresh_tokens rt JOIN players p ON p.id=rt.player_id
		                   WHERE p.project_id=$1 AND p.environment_id=$2 AND rt.token_hash LIKE 'demo-activity:%') + interval '15 minutes',
		                  now() - interval '30 days')), TIMESTAMPTZ '2000-01-01') AS lo,
		       date_bin('15 minutes', now(), TIMESTAMPTZ '2000-01-01') - interval '15 minutes' AS hi),
	slots AS (
		SELECT p.id AS player_id, b.t,
		       -- 0..1 pseudo-random numbers: does the player play that day, are they online in this bucket
		       abs(hashtext(p.id::text || (b.t AT TIME ZONE 'UTC')::date::text)) % 1000 / 1000.0 AS day_roll,
		       abs(hashtext(p.id::text || b.t::text)) % 1000 / 1000.0 AS slot_roll,
		       0.35 + abs(hashtext(p.id::text)) % 600 / 1000.0 AS plays_share,
		       extract(hour FROM b.t AT TIME ZONE 'UTC') + extract(minute FROM b.t) / 60.0 AS hour,
		       extract(isodow FROM b.t AT TIME ZONE 'UTC') AS dow
		FROM players p, bounds, generate_series(bounds.lo, bounds.hi, interval '15 minutes') AS b(t)
		WHERE p.project_id=$1 AND p.environment_id=$2 AND p.email LIKE 'crowd%@realmstack.local')
	INSERT INTO refresh_tokens(player_id,family_id,token_hash,expires_at,revoked_at,created_at)
	SELECT player_id, gen_random_uuid(), 'demo-activity:' || gen_random_uuid(),
	       t + interval '30 days', t + interval '14 minutes',
	       t + (abs(hashtext(player_id::text || t::text || 's')) % 840) * interval '1 second'
	FROM slots
	WHERE day_roll < plays_share
	  -- evening peak around 20:00 UTC, quietest around 08:00; weekends a little busier
	  AND slot_roll < 0.08 + 0.42 * (0.5 + 0.5 * cos(2 * pi() * (hour - 20) / 24)) + CASE WHEN dow >= 6 THEN 0.08 ELSE 0 END`, projectID, envID)
	return err
}
