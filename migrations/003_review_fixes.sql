-- Review fixes and goal-engine preparation.

-- Projects are archived, never hard-deleted. Deleting a project would
-- cascade into the immutable ledgers.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS status text NOT NULL DEFAULT 'active';
ALTER TABLE projects ADD COLUMN IF NOT EXISTS archived_at timestamptz;
ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_status_check;
ALTER TABLE projects ADD CONSTRAINT projects_status_check CHECK (status IN ('active','archived'));

-- Idempotency keys are namespaced by actor kind. A player's self-service keys and the
-- trusted server's keys for that player no longer share one namespace, so a player
-- cannot occupy a key the game server will use later. Records written before this
-- migration are treated as player records.
ALTER TABLE idempotency_records ADD COLUMN IF NOT EXISTS actor_kind text NOT NULL DEFAULT 'player';
ALTER TABLE idempotency_records DROP CONSTRAINT IF EXISTS idempotency_records_actor_kind_check;
ALTER TABLE idempotency_records ADD CONSTRAINT idempotency_records_actor_kind_check CHECK (actor_kind IN ('player','server'));
ALTER TABLE idempotency_records DROP CONSTRAINT IF EXISTS idempotency_records_pkey;
ALTER TABLE idempotency_records ADD PRIMARY KEY (project_id, environment_id, actor_kind, actor_id, key);

-- Ledger source: goal evaluation counts only source='api' rows, so rewards
-- paid by achievement/quest claims never complete another goal.
ALTER TABLE economy_transactions ADD COLUMN IF NOT EXISTS source text NOT NULL DEFAULT 'api';
ALTER TABLE economy_transactions DROP CONSTRAINT IF EXISTS economy_transactions_source_check;
ALTER TABLE economy_transactions ADD CONSTRAINT economy_transactions_source_check CHECK (source IN ('api','achievement','quest'));
ALTER TABLE xp_transactions ADD COLUMN IF NOT EXISTS source text NOT NULL DEFAULT 'api';
ALTER TABLE xp_transactions DROP CONSTRAINT IF EXISTS xp_transactions_source_check;
ALTER TABLE xp_transactions ADD CONSTRAINT xp_transactions_source_check CHECK (source IN ('api','achievement','quest'));

-- Each XP award records the progression version it was applied under.
ALTER TABLE xp_transactions ADD COLUMN IF NOT EXISTS progression_version_id uuid;
UPDATE xp_transactions x SET progression_version_id = pp.progression_version_id
  FROM player_progression pp WHERE pp.player_id = x.player_id AND x.progression_version_id IS NULL;
ALTER TABLE xp_transactions ALTER COLUMN progression_version_id SET NOT NULL;
ALTER TABLE xp_transactions DROP CONSTRAINT IF EXISTS xp_transactions_progression_fk;
ALTER TABLE xp_transactions ADD CONSTRAINT xp_transactions_progression_fk
  FOREIGN KEY (progression_version_id, project_id, environment_id) REFERENCES progression_versions(id, project_id, environment_id);

-- Indexes for windowed goal evaluation and cursor-paginated history.
CREATE INDEX IF NOT EXISTS economy_goal_idx ON economy_transactions(player_id, operation, created_at)
  INCLUDE (item_id, currency_id, quantity) WHERE source = 'api';
CREATE INDEX IF NOT EXISTS xp_goal_idx ON xp_transactions(player_id, created_at) INCLUDE (amount) WHERE source = 'api';
DROP INDEX IF EXISTS economy_player_history_idx;
CREATE INDEX IF NOT EXISTS economy_player_history_idx ON economy_transactions(player_id, created_at DESC, id DESC);

-- Ledgers are append-only. Corrections are compensating rows.
CREATE OR REPLACE FUNCTION realmstack_reject_ledger_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'ledger table % is append-only', TG_TABLE_NAME USING ERRCODE = 'P0001';
END
$$;
DROP TRIGGER IF EXISTS economy_transactions_append_only ON economy_transactions;
CREATE TRIGGER economy_transactions_append_only BEFORE UPDATE OR DELETE ON economy_transactions
  FOR EACH ROW EXECUTE FUNCTION realmstack_reject_ledger_change();
DROP TRIGGER IF EXISTS xp_transactions_append_only ON xp_transactions;
CREATE TRIGGER xp_transactions_append_only BEFORE UPDATE OR DELETE ON xp_transactions
  FOR EACH ROW EXECUTE FUNCTION realmstack_reject_ledger_change();
DROP TRIGGER IF EXISTS score_submissions_append_only ON score_submissions;
CREATE TRIGGER score_submissions_append_only BEFORE UPDATE OR DELETE ON score_submissions
  FOR EACH ROW EXECUTE FUNCTION realmstack_reject_ledger_change();

-- Configuration versions record who published them and when.
ALTER TABLE configuration_versions ADD COLUMN IF NOT EXISTS created_by uuid REFERENCES users(id);
ALTER TABLE configuration_versions ADD COLUMN IF NOT EXISTS published_at timestamptz;
UPDATE configuration_versions SET published_at = created_at WHERE published_at IS NULL;
ALTER TABLE configuration_versions ALTER COLUMN published_at SET DEFAULT now();
ALTER TABLE configuration_versions ALTER COLUMN published_at SET NOT NULL;

-- Progression versions get their own sequence and link to the configuration version
-- that introduced them; a publish that leaves thresholds unchanged creates none.
ALTER TABLE progression_versions ADD COLUMN IF NOT EXISTS configuration_version_id uuid;
UPDATE progression_versions pv SET configuration_version_id = cv.id
  FROM configuration_versions cv
  WHERE cv.project_id = pv.project_id AND cv.environment_id = pv.environment_id
    AND cv.version = pv.version AND pv.configuration_version_id IS NULL;
ALTER TABLE progression_versions DROP CONSTRAINT IF EXISTS progression_versions_configuration_fk;
ALTER TABLE progression_versions ADD CONSTRAINT progression_versions_configuration_fk
  FOREIGN KEY (configuration_version_id, project_id, environment_id) REFERENCES configuration_versions(id, project_id, environment_id);
