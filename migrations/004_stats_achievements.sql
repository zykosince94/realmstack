-- Phase 5: stats/flags (server-reported game-world facts) and achievements.

-- Stat and flag definitions are synced from the published configuration's "stats" array.
CREATE TABLE IF NOT EXISTS stat_definitions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), project_id uuid NOT NULL, environment_id uuid NOT NULL,
 key text NOT NULL, name text NOT NULL, kind text NOT NULL CHECK (kind IN ('counter','flag')),
 active boolean NOT NULL DEFAULT true,
 UNIQUE(project_id,environment_id,key), UNIQUE(id,project_id,environment_id),
 FOREIGN KEY(environment_id,project_id) REFERENCES environments(id,project_id) ON DELETE CASCADE
);

-- Current value per player and stat: a counter total, or 1 for a set flag.
CREATE TABLE IF NOT EXISTS player_stats (
 player_id uuid NOT NULL, project_id uuid NOT NULL, environment_id uuid NOT NULL, stat_definition_id uuid NOT NULL,
 value bigint NOT NULL CHECK (value >= 0), updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(player_id,stat_definition_id),
 FOREIGN KEY(player_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id),
 FOREIGN KEY(stat_definition_id,project_id,environment_id) REFERENCES stat_definitions(id,project_id,environment_id)
);

-- Immutable stat ledger. Goal evaluation sums increments inside a time window.
CREATE TABLE IF NOT EXISTS stat_transactions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), player_id uuid NOT NULL, project_id uuid NOT NULL, environment_id uuid NOT NULL,
 stat_definition_id uuid NOT NULL, operation text NOT NULL CHECK (operation IN ('increment','set_flag')),
 amount bigint NOT NULL CHECK (amount > 0), reason text NOT NULL DEFAULT '', reference text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(player_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id),
 FOREIGN KEY(stat_definition_id,project_id,environment_id) REFERENCES stat_definitions(id,project_id,environment_id)
);
CREATE INDEX IF NOT EXISTS stat_goal_idx ON stat_transactions(player_id, created_at) INCLUDE (stat_definition_id, operation, amount);
DROP TRIGGER IF EXISTS stat_transactions_append_only ON stat_transactions;
CREATE TRIGGER stat_transactions_append_only BEFORE UPDATE OR DELETE ON stat_transactions
  FOR EACH ROW EXECUTE FUNCTION realmstack_reject_ledger_change();

-- Achievement definitions are synced from the published "achievements" array.
-- Removed definitions are deactivated, never deleted, so claims keep their target.
CREATE TABLE IF NOT EXISTS achievement_definitions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), project_id uuid NOT NULL, environment_id uuid NOT NULL,
 key text NOT NULL, name text NOT NULL, description text NOT NULL DEFAULT '', hidden boolean NOT NULL DEFAULT false,
 conditions jsonb NOT NULL, rewards jsonb NOT NULL DEFAULT '{}'::jsonb,
 claimable_by text NOT NULL DEFAULT 'player' CHECK (claimable_by IN ('player','server')),
 sort_order integer NOT NULL DEFAULT 0, active boolean NOT NULL DEFAULT true,
 UNIQUE(project_id,environment_id,key), UNIQUE(id,project_id,environment_id),
 FOREIGN KEY(environment_id,project_id) REFERENCES environments(id,project_id) ON DELETE CASCADE
);

-- One immutable claim per player and achievement, with a snapshot of what was paid.
CREATE TABLE IF NOT EXISTS achievement_claims (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), player_id uuid NOT NULL, project_id uuid NOT NULL, environment_id uuid NOT NULL,
 achievement_id uuid NOT NULL, rewards jsonb NOT NULL, claimed_by text NOT NULL CHECK (claimed_by IN ('player','server')),
 claimed_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(player_id,achievement_id),
 FOREIGN KEY(player_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id),
 FOREIGN KEY(achievement_id,project_id,environment_id) REFERENCES achievement_definitions(id,project_id,environment_id)
);
CREATE INDEX IF NOT EXISTS achievement_claims_by_achievement_idx ON achievement_claims(achievement_id);
DROP TRIGGER IF EXISTS achievement_claims_append_only ON achievement_claims;
CREATE TRIGGER achievement_claims_append_only BEFORE UPDATE OR DELETE ON achievement_claims
  FOR EACH ROW EXECUTE FUNCTION realmstack_reject_ledger_change();

-- Score lookups by player for leaderboard_score conditions.
CREATE INDEX IF NOT EXISTS scores_player_idx ON score_submissions(player_id, leaderboard_id, submitted_at);
