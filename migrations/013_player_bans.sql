-- Player bans. A dashboard admin bans a player of an environment
-- they own, for a while or permanently; a banned player cannot sign in, refresh a session
-- or use a player token. One row per ban: lifting or replacing a ban ends the row instead
-- of deleting it, so the table is also the ban history.
CREATE TABLE IF NOT EXISTS player_bans (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 player_id uuid NOT NULL, project_id uuid NOT NULL, environment_id uuid NOT NULL,
 -- Shown to the player in the 403 PLAYER_BANNED response, so it is not an internal note.
 reason text NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 500),
 expires_at timestamptz, -- NULL means permanent
 created_by uuid NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL,
 ended_at timestamptz,
 ended_by uuid REFERENCES users(id),
 end_kind text CHECK (end_kind IN ('lifted','replaced')),
 end_note text NOT NULL DEFAULT '' CHECK (char_length(end_note) <= 500),
 -- Database clock, not the API clock: each API instance polls rows changed since its
 -- last sync (bans.Cache), and that watermark must come from one clock.
 updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK ((ended_at IS NULL) = (end_kind IS NULL)),
 CHECK (expires_at IS NULL OR expires_at > created_at),
 FOREIGN KEY(player_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id) ON DELETE CASCADE
);
-- At most one open ban per player; a new ban ends the previous one ('replaced').
CREATE UNIQUE INDEX IF NOT EXISTS player_bans_open_uq ON player_bans(player_id) WHERE ended_at IS NULL;
CREATE INDEX IF NOT EXISTS player_bans_history_idx ON player_bans(player_id, created_at DESC);
CREATE INDEX IF NOT EXISTS player_bans_changed_idx ON player_bans(updated_at);
