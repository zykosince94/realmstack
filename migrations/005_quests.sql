-- Quests. Active, staged goals on the shared goal engine.

-- Quest definitions are synced from the published configuration's "quests" array.
-- Removed quests are deactivated, never deleted, so acceptances and claims keep their
-- target. activated_at is when the quest last went live: an auto_accept quest counts
-- activity from then at the earliest, so publishing a new quest does not hand players
-- credit for everything they did before it existed.
CREATE TABLE IF NOT EXISTS quest_definitions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), project_id uuid NOT NULL, environment_id uuid NOT NULL,
 key text NOT NULL, name text NOT NULL, description text NOT NULL DEFAULT '', chain text,
 requires_quests jsonb NOT NULL DEFAULT '[]'::jsonb, requires_achievements jsonb NOT NULL DEFAULT '[]'::jsonb,
 stages jsonb NOT NULL, rewards jsonb NOT NULL DEFAULT '{}'::jsonb,
 claimable_by text NOT NULL DEFAULT 'player' CHECK (claimable_by IN ('player','server')),
 auto_accept boolean NOT NULL DEFAULT false,
 repeat text NOT NULL DEFAULT 'none' CHECK (repeat IN ('none','daily','weekly')),
 available_from timestamptz, available_until timestamptz,
 sort_order integer NOT NULL DEFAULT 0, active boolean NOT NULL DEFAULT true,
 activated_at timestamptz NOT NULL DEFAULT now(),
 CHECK (available_from IS NULL OR available_until IS NULL OR available_from < available_until),
 UNIQUE(project_id,environment_id,key), UNIQUE(id,project_id,environment_id),
 FOREIGN KEY(environment_id,project_id) REFERENCES environments(id,project_id) ON DELETE CASCADE
);

-- One acceptance record per player, quest and period ('' for non-repeating quests,
-- YYYY-MM-DD for daily, YYYY-Www for weekly). Abandoning flips the status; accepting
-- again reactivates the row with a new accepted_at, which restarts progress. This
-- table is state, not a ledger, so it is deliberately mutable.
CREATE TABLE IF NOT EXISTS player_quests (
 player_id uuid NOT NULL, project_id uuid NOT NULL, environment_id uuid NOT NULL, quest_id uuid NOT NULL,
 period_key text NOT NULL, status text NOT NULL CHECK (status IN ('active','abandoned')),
 accepted_at timestamptz NOT NULL, abandoned_at timestamptz,
 PRIMARY KEY(player_id,quest_id,period_key),
 FOREIGN KEY(player_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id),
 FOREIGN KEY(quest_id,project_id,environment_id) REFERENCES quest_definitions(id,project_id,environment_id)
);
CREATE INDEX IF NOT EXISTS player_quests_by_quest_idx ON player_quests(quest_id);

-- Immutable claim records: one per player, quest, period and stage. stage_index -1 is
-- the completion reward. rewards is a snapshot of what was paid.
CREATE TABLE IF NOT EXISTS quest_claims (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), player_id uuid NOT NULL, project_id uuid NOT NULL, environment_id uuid NOT NULL,
 quest_id uuid NOT NULL, period_key text NOT NULL, stage_index integer NOT NULL CHECK (stage_index >= -1),
 rewards jsonb NOT NULL, claimed_by text NOT NULL CHECK (claimed_by IN ('player','server')),
 claimed_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(player_id,quest_id,period_key,stage_index),
 FOREIGN KEY(player_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id),
 FOREIGN KEY(quest_id,project_id,environment_id) REFERENCES quest_definitions(id,project_id,environment_id)
);
CREATE INDEX IF NOT EXISTS quest_claims_by_quest_idx ON quest_claims(quest_id);
DROP TRIGGER IF EXISTS quest_claims_append_only ON quest_claims;
CREATE TRIGGER quest_claims_append_only BEFORE UPDATE OR DELETE ON quest_claims
  FOR EACH ROW EXECUTE FUNCTION realmstack_reject_ledger_change();

