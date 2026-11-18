-- Dashboard sessions survive a page reload: a rotating refresh token, kept by the browser
-- in an httpOnly cookie, mints new short-lived access tokens. Same design as the player
-- refresh tokens (refresh_tokens): one row per token, a family per sign-in, and reuse of a
-- rotated token revokes the whole family. Only a digest of the token is stored.
CREATE TABLE IF NOT EXISTS dashboard_refresh_tokens (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 family_id uuid NOT NULL,
 token_hash text NOT NULL UNIQUE,
 expires_at timestamptz NOT NULL,
 revoked_at timestamptz,
 replaced_by uuid REFERENCES dashboard_refresh_tokens(id),
 created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS dashboard_refresh_family_idx ON dashboard_refresh_tokens(family_id);
CREATE INDEX IF NOT EXISTS dashboard_refresh_user_idx ON dashboard_refresh_tokens(user_id);
