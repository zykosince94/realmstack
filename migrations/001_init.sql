CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE IF NOT EXISTS users (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), email text NOT NULL UNIQUE,
 password_hash text NOT NULL, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS projects (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), owner_user_id uuid NOT NULL REFERENCES users(id),
 name text NOT NULL, slug text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(owner_user_id, slug), UNIQUE(id, owner_user_id)
);
CREATE TABLE IF NOT EXISTS environments (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 name text NOT NULL CHECK(name IN ('development','production')), public_id text NOT NULL UNIQUE,
 created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(project_id,name), UNIQUE(id,project_id)
);
CREATE TABLE IF NOT EXISTS server_credentials (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), project_id uuid NOT NULL, environment_id uuid NOT NULL,
 key_prefix text NOT NULL, secret_hash text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(), revoked_at timestamptz,
 UNIQUE(key_prefix),
 FOREIGN KEY(environment_id,project_id) REFERENCES environments(id,project_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS players (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), project_id uuid NOT NULL, environment_id uuid NOT NULL,
 external_player_key text, email text, password_hash text, display_name text NOT NULL,
 metadata jsonb NOT NULL DEFAULT '{}'::jsonb, created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(environment_id,project_id) REFERENCES environments(id,project_id) ON DELETE CASCADE,
 UNIQUE(id,project_id,environment_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS players_external_key_uq ON players(project_id,environment_id,external_player_key) WHERE external_player_key IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS players_email_uq ON players(project_id,environment_id,email) WHERE email IS NOT NULL;
CREATE TABLE IF NOT EXISTS refresh_tokens (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), player_id uuid NOT NULL REFERENCES players(id) ON DELETE CASCADE,
 family_id uuid NOT NULL, token_hash text NOT NULL UNIQUE, expires_at timestamptz NOT NULL,
 revoked_at timestamptz, replaced_by uuid REFERENCES refresh_tokens(id), created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS refresh_family_idx ON refresh_tokens(family_id);
CREATE TABLE IF NOT EXISTS configuration_drafts (
 project_id uuid NOT NULL, environment_id uuid NOT NULL,  config jsonb NOT NULL DEFAULT '{"items":[],"currencies":[],"progression":[100,300],"leaderboards":[]}'::jsonb,
 updated_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(project_id,environment_id),
 FOREIGN KEY(environment_id,project_id) REFERENCES environments(id,project_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS configuration_versions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), project_id uuid NOT NULL, environment_id uuid NOT NULL,
 version integer NOT NULL, config jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(project_id,environment_id,version), UNIQUE(id,project_id,environment_id),
 FOREIGN KEY(environment_id,project_id) REFERENCES environments(id,project_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS item_definitions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), project_id uuid NOT NULL, environment_id uuid NOT NULL,
 item_key text NOT NULL, name text NOT NULL, category text NOT NULL DEFAULT 'general',
 stackable boolean NOT NULL DEFAULT true, max_stack integer NOT NULL DEFAULT 999, metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
 active boolean NOT NULL DEFAULT true, UNIQUE(project_id,environment_id,item_key), UNIQUE(id,project_id,environment_id),
 FOREIGN KEY(environment_id,project_id) REFERENCES environments(id,project_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS currency_definitions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), project_id uuid NOT NULL, environment_id uuid NOT NULL,
 currency_key text NOT NULL, name text NOT NULL, active boolean NOT NULL DEFAULT true,
 UNIQUE(project_id,environment_id,currency_key), UNIQUE(id,project_id,environment_id),
 FOREIGN KEY(environment_id,project_id) REFERENCES environments(id,project_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS inventory (
 player_id uuid NOT NULL, project_id uuid NOT NULL, environment_id uuid NOT NULL, item_id uuid NOT NULL,
 quantity bigint NOT NULL CHECK(quantity >= 0), updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(player_id,item_id), FOREIGN KEY(player_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id) ON DELETE CASCADE,
 FOREIGN KEY(item_id,project_id,environment_id) REFERENCES item_definitions(id,project_id,environment_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS equipment (
 player_id uuid NOT NULL, project_id uuid NOT NULL, environment_id uuid NOT NULL, slot_key text NOT NULL,
 item_id uuid NOT NULL, PRIMARY KEY(player_id,slot_key),
 FOREIGN KEY(player_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id) ON DELETE CASCADE,
 FOREIGN KEY(player_id,item_id) REFERENCES inventory(player_id,item_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS balances (
 player_id uuid NOT NULL, project_id uuid NOT NULL, environment_id uuid NOT NULL, currency_id uuid NOT NULL,
 amount bigint NOT NULL CHECK(amount >= 0), updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(player_id,currency_id), FOREIGN KEY(player_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id) ON DELETE CASCADE,
 FOREIGN KEY(currency_id,project_id,environment_id) REFERENCES currency_definitions(id,project_id,environment_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS idempotency_records (
 project_id uuid NOT NULL, environment_id uuid NOT NULL, actor_id uuid NOT NULL, key text NOT NULL,
 request_hash text NOT NULL, result jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(project_id,environment_id,actor_id,key),
 FOREIGN KEY(actor_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS economy_transactions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), player_id uuid NOT NULL, project_id uuid NOT NULL, environment_id uuid NOT NULL,
 currency_id uuid, item_id uuid, operation text NOT NULL, quantity bigint NOT NULL, balance_after bigint,
 reason text NOT NULL DEFAULT '', reference text NOT NULL DEFAULT '', created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(player_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id) ON DELETE CASCADE
 ,FOREIGN KEY(currency_id,project_id,environment_id) REFERENCES currency_definitions(id,project_id,environment_id) ON DELETE CASCADE,
 FOREIGN KEY(item_id,project_id,environment_id) REFERENCES item_definitions(id,project_id,environment_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS economy_player_history_idx ON economy_transactions(player_id,created_at DESC);
CREATE TABLE IF NOT EXISTS progression_versions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), project_id uuid NOT NULL, environment_id uuid NOT NULL,
 version integer NOT NULL, thresholds jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(project_id,environment_id,version), UNIQUE(id,project_id,environment_id),
 FOREIGN KEY(environment_id,project_id) REFERENCES environments(id,project_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS player_progression (
 player_id uuid PRIMARY KEY, project_id uuid NOT NULL, environment_id uuid NOT NULL,
 progression_version_id uuid NOT NULL,
 total_xp bigint NOT NULL DEFAULT 0 CHECK(total_xp>=0), updated_at timestamptz NOT NULL DEFAULT now()
 ,FOREIGN KEY(player_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id) ON DELETE CASCADE,
 FOREIGN KEY(progression_version_id,project_id,environment_id) REFERENCES progression_versions(id,project_id,environment_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS xp_transactions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), player_id uuid NOT NULL, project_id uuid NOT NULL, environment_id uuid NOT NULL,
 amount bigint NOT NULL, reason text NOT NULL DEFAULT '', reference text NOT NULL DEFAULT '',
 idempotency_key text NOT NULL, request_hash text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(player_id,idempotency_key),
 FOREIGN KEY(player_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS leaderboard_definitions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), project_id uuid NOT NULL, environment_id uuid NOT NULL,
 key text NOT NULL, score_direction text NOT NULL CHECK(score_direction IN ('higher','lower')), active boolean NOT NULL DEFAULT true,
 season_start timestamptz, season_end timestamptz, UNIQUE(project_id,environment_id,key),
 UNIQUE(id,project_id,environment_id), FOREIGN KEY(environment_id,project_id) REFERENCES environments(id,project_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS score_submissions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(), leaderboard_id uuid NOT NULL, player_id uuid NOT NULL,
 project_id uuid NOT NULL, environment_id uuid NOT NULL, score bigint NOT NULL,
 idempotency_key text NOT NULL, request_hash text NOT NULL, submitted_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(leaderboard_id,project_id,environment_id) REFERENCES leaderboard_definitions(id,project_id,environment_id) ON DELETE CASCADE,
 FOREIGN KEY(player_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id) ON DELETE CASCADE,
 UNIQUE(player_id,leaderboard_id,idempotency_key)
);
CREATE INDEX IF NOT EXISTS scores_board_player_idx ON score_submissions(leaderboard_id,player_id,submitted_at);
