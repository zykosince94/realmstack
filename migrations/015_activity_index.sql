-- The dashboard's activity chart (CCU and DAU, 2026-11-22) is derived from when sessions
-- were issued (internal/activity). It reads refresh tokens by creation time; this index
-- lets it scan only the window it needs, and INCLUDE avoids visiting the table for the
-- player ID.
CREATE INDEX IF NOT EXISTS refresh_tokens_created_idx ON refresh_tokens(created_at) INCLUDE (player_id);
