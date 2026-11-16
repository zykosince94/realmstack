-- Phase 7: AI configuration assistant (bring your own key).

-- One provider setting per dashboard user (spec 4). The API key is encrypted with
-- AES-256-GCM under a key from the deployment's environment (AI_ENCRYPTION_KEY),
-- never stored in the database. key_version allows rotating that encryption key;
-- key_last4 is the only part of the key the API ever returns.
CREATE TABLE IF NOT EXISTS ai_provider_settings (
 user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
 provider text NOT NULL CHECK (provider IN ('openai')),
 model text NOT NULL,
 encrypted_api_key bytea NOT NULL, key_nonce bytea NOT NULL, key_version integer NOT NULL DEFAULT 1,
 key_last4 text NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now()
);

-- One row per AI request. A generation is a proposal: it never touches runtime
-- tables. "valid" proposals can be applied to the draft by a person; publishing stays
-- the existing, separate, explicit step. output holds the model's parsed proposal,
-- merged the draft it would produce, errors the field errors that made it invalid.
CREATE TABLE IF NOT EXISTS ai_generations (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), project_id uuid NOT NULL, environment_id uuid NOT NULL,
 user_id uuid NOT NULL REFERENCES users(id),
 operation text NOT NULL CHECK (operation IN ('items','economy','progression','achievements','quests','explain','tests')),
 params jsonb NOT NULL DEFAULT '{}'::jsonb,
 merge_policy text NOT NULL DEFAULT 'add' CHECK (merge_policy IN ('add','replace')),
 provider text NOT NULL, model text NOT NULL,
 status text NOT NULL CHECK (status IN ('pending','running','valid','invalid','answered','failed','applied','discarded')),
 output jsonb, merged jsonb, errors jsonb NOT NULL DEFAULT '[]'::jsonb, notes text NOT NULL DEFAULT '',
 error_code text NOT NULL DEFAULT '', error_message text NOT NULL DEFAULT '',
 input_tokens bigint NOT NULL DEFAULT 0, output_tokens bigint NOT NULL DEFAULT 0,
 base_draft_hash text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(), finished_at timestamptz,
 applied_at timestamptz, applied_by uuid REFERENCES users(id),
 FOREIGN KEY(environment_id,project_id) REFERENCES environments(id,project_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS ai_generations_scope_idx ON ai_generations(project_id, environment_id, created_at DESC);
CREATE INDEX IF NOT EXISTS ai_generations_running_idx ON ai_generations(user_id) WHERE status IN ('pending','running');
