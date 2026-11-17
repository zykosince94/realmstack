-- Outbox for the optional Redis rank projection (spec 6.6). A score submission that
-- improves a player's best inserts a row here in the same transaction (only when Redis
-- is configured); API workers copy the player's current best into a Redis sorted set
-- and delete the row. A row with player_id NULL marks the end of a full replay: once
-- every earlier row for that board is applied, the board is served from Redis.
--
-- The table has no project_id/environment_id columns: leaderboard_id already fixes the
-- tenant (a board belongs to one environment), and both foreign keys cascade.
CREATE TABLE IF NOT EXISTS leaderboard_outbox (
 id bigserial PRIMARY KEY,
 leaderboard_id uuid NOT NULL REFERENCES leaderboard_definitions(id) ON DELETE CASCADE,
 player_id uuid REFERENCES players(id) ON DELETE CASCADE,
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS leaderboard_outbox_board_idx ON leaderboard_outbox(leaderboard_id, id);

-- Scores are bounded to +-(2^53 - 1) so every rank score is exactly representable as the
-- float64 a Redis sorted set stores (see internal/leaderboards MaxScore).
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'score_submissions_score_range') THEN
    ALTER TABLE score_submissions ADD CONSTRAINT score_submissions_score_range
      CHECK (score BETWEEN -9007199254740991 AND 9007199254740991);
  END IF;
END $$;
