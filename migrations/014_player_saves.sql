-- Cloud saves: named JSON save slots per player. Saves are
-- mutable state, not a ledger: each write replaces the slot and bumps its version, which
-- clients send back as expected_version so two devices cannot silently overwrite each
-- other (optimistic concurrency).
CREATE TABLE IF NOT EXISTS player_saves (
 player_id uuid NOT NULL, project_id uuid NOT NULL, environment_id uuid NOT NULL,
 slot text NOT NULL CHECK (slot ~ '^[a-z0-9][a-z0-9_-]{0,62}$'),
 data jsonb NOT NULL,
 version bigint NOT NULL CHECK (version >= 1),
 -- Size of the compacted JSON as sent; the limit is enforced in internal/saves.
 size_bytes integer NOT NULL CHECK (size_bytes > 0),
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL,
 PRIMARY KEY (player_id, slot),
 FOREIGN KEY(player_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id) ON DELETE CASCADE
);
