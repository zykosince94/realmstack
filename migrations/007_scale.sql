-- Phase 8: scale, retention and tenant fairness (spec 6.6).
--
-- Before v1 there is no production data to preserve, so the four ledgers are dropped
-- and recreated as monthly range-partitioned tables. A local demo database needs a
-- reset after this migration (docker compose down -v) so its balances and the new
-- ledgers start out consistent.

DROP TABLE IF EXISTS economy_transactions CASCADE;
DROP TABLE IF EXISTS xp_transactions CASCADE;
DROP TABLE IF EXISTS stat_transactions CASCADE;
DROP TABLE IF EXISTS score_submissions CASCADE;

-- A partitioned table's primary key must contain the partition column, hence
-- (id, created_at). Unique keys must too, so the old secondary uniques
-- (player_id, idempotency_key) on XP and scores are gone: idempotency_records was
-- already the guard against duplicate writes, and claim tables keep their own.
CREATE TABLE economy_transactions (
 id uuid NOT NULL DEFAULT gen_random_uuid(), player_id uuid NOT NULL, project_id uuid NOT NULL, environment_id uuid NOT NULL,
 currency_id uuid, item_id uuid, operation text NOT NULL, quantity bigint NOT NULL, balance_after bigint,
 reason text NOT NULL DEFAULT '', reference text NOT NULL DEFAULT '',
 source text NOT NULL DEFAULT 'api' CHECK (source IN ('api','achievement','quest')),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (id, created_at),
 FOREIGN KEY(player_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id) ON DELETE CASCADE,
 FOREIGN KEY(currency_id,project_id,environment_id) REFERENCES currency_definitions(id,project_id,environment_id) ON DELETE CASCADE,
 FOREIGN KEY(item_id,project_id,environment_id) REFERENCES item_definitions(id,project_id,environment_id) ON DELETE CASCADE
) PARTITION BY RANGE (created_at);
CREATE INDEX economy_player_history_idx ON economy_transactions(player_id, created_at DESC, id DESC);
CREATE INDEX economy_goal_idx ON economy_transactions(player_id, operation, created_at) INCLUDE (item_id, currency_id, quantity) WHERE source = 'api';

CREATE TABLE xp_transactions (
 id uuid NOT NULL DEFAULT gen_random_uuid(), player_id uuid NOT NULL, project_id uuid NOT NULL, environment_id uuid NOT NULL,
 progression_version_id uuid NOT NULL, amount bigint NOT NULL, reason text NOT NULL DEFAULT '', reference text NOT NULL DEFAULT '',
 idempotency_key text NOT NULL, request_hash text NOT NULL,
 source text NOT NULL DEFAULT 'api' CHECK (source IN ('api','achievement','quest')),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (id, created_at),
 FOREIGN KEY(player_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id) ON DELETE CASCADE,
 FOREIGN KEY(progression_version_id,project_id,environment_id) REFERENCES progression_versions(id,project_id,environment_id)
) PARTITION BY RANGE (created_at);
CREATE INDEX xp_goal_idx ON xp_transactions(player_id, created_at) INCLUDE (amount) WHERE source = 'api';

CREATE TABLE stat_transactions (
 id uuid NOT NULL DEFAULT gen_random_uuid(), player_id uuid NOT NULL, project_id uuid NOT NULL, environment_id uuid NOT NULL,
 stat_definition_id uuid NOT NULL, operation text NOT NULL CHECK (operation IN ('increment','set_flag')),
 amount bigint NOT NULL CHECK (amount > 0), reason text NOT NULL DEFAULT '', reference text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (id, created_at),
 FOREIGN KEY(player_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id),
 FOREIGN KEY(stat_definition_id,project_id,environment_id) REFERENCES stat_definitions(id,project_id,environment_id)
) PARTITION BY RANGE (created_at);
CREATE INDEX stat_goal_idx ON stat_transactions(player_id, created_at) INCLUDE (stat_definition_id, operation, amount);

CREATE TABLE score_submissions (
 id uuid NOT NULL DEFAULT gen_random_uuid(), leaderboard_id uuid NOT NULL, player_id uuid NOT NULL,
 project_id uuid NOT NULL, environment_id uuid NOT NULL, score bigint NOT NULL,
 idempotency_key text NOT NULL, request_hash text NOT NULL, submitted_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (id, submitted_at),
 FOREIGN KEY(leaderboard_id,project_id,environment_id) REFERENCES leaderboard_definitions(id,project_id,environment_id) ON DELETE CASCADE,
 FOREIGN KEY(player_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id) ON DELETE CASCADE
) PARTITION BY RANGE (submitted_at);
CREATE INDEX scores_board_player_idx ON score_submissions(leaderboard_id, player_id, submitted_at);
CREATE INDEX scores_player_idx ON score_submissions(player_id, leaderboard_id, submitted_at);

-- Row triggers on a partitioned table are cloned onto every partition, including ones
-- created later, so the ledgers stay append-only. Old partitions may be detached and
-- archived, but never edited.
CREATE TRIGGER economy_transactions_append_only BEFORE UPDATE OR DELETE ON economy_transactions FOR EACH ROW EXECUTE FUNCTION realmstack_reject_ledger_change();
CREATE TRIGGER xp_transactions_append_only BEFORE UPDATE OR DELETE ON xp_transactions FOR EACH ROW EXECUTE FUNCTION realmstack_reject_ledger_change();
CREATE TRIGGER stat_transactions_append_only BEFORE UPDATE OR DELETE ON stat_transactions FOR EACH ROW EXECUTE FUNCTION realmstack_reject_ledger_change();
CREATE TRIGGER score_submissions_append_only BEFORE UPDATE OR DELETE ON score_submissions FOR EACH ROW EXECUTE FUNCTION realmstack_reject_ledger_change();

-- A default partition catches any timestamp outside the monthly partitions (clock
-- skew, tests that move the clock), so a write never fails for lack of a partition.
CREATE TABLE economy_transactions_default PARTITION OF economy_transactions DEFAULT;
CREATE TABLE xp_transactions_default PARTITION OF xp_transactions DEFAULT;
CREATE TABLE stat_transactions_default PARTITION OF stat_transactions DEFAULT;
CREATE TABLE score_submissions_default PARTITION OF score_submissions DEFAULT;

-- realmstack_ensure_partitions creates the monthly partitions from start_month through
-- months_ahead months after the current one. The maintenance job calls it daily, so
-- partitions always exist ahead of time. A month the default partition already holds
-- rows for cannot get its own partition; it is skipped with a warning and those rows
-- stay in the default partition.
CREATE OR REPLACE FUNCTION realmstack_ensure_partitions(start_month date, months_ahead integer) RETURNS integer
LANGUAGE plpgsql AS $$
DECLARE
  t text;
  m date;
  last_month date := (date_trunc('month', now()) + make_interval(months => months_ahead))::date;
  part text;
  created integer := 0;
BEGIN
  FOREACH t IN ARRAY ARRAY['economy_transactions','xp_transactions','stat_transactions','score_submissions'] LOOP
    m := date_trunc('month', start_month)::date;
    WHILE m <= last_month LOOP
      part := t || '_' || to_char(m, 'YYYY_MM');
      IF to_regclass(part) IS NULL THEN
        BEGIN
          EXECUTE format('CREATE TABLE %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)', part, t, m, (m + interval '1 month')::date);
          created := created + 1;
        EXCEPTION WHEN others THEN
          RAISE WARNING 'partition % not created: %', part, SQLERRM;
        END;
      END IF;
      m := (m + interval '1 month')::date;
    END LOOP;
  END LOOP;
  RETURN created;
END $$;
SELECT realmstack_ensure_partitions((date_trunc('month', now()) - interval '1 month')::date, 3);

-- Leaderboards: one row per player and board, maintained in the score-submission
-- transaction (spec 6.3, 6.6). rank_score is the score negated on higher-is-better
-- boards, so "better" is always "smaller (rank_score, achieved_at, player_id)" and one
-- ascending index serves top-N and rank for both directions. A board's direction
-- cannot change once it has scores, so rank_score never goes stale.
CREATE TABLE IF NOT EXISTS leaderboard_best_scores (
 leaderboard_id uuid NOT NULL, player_id uuid NOT NULL, project_id uuid NOT NULL, environment_id uuid NOT NULL,
 best_score bigint NOT NULL, rank_score bigint NOT NULL, achieved_at timestamptz NOT NULL, submission_id uuid NOT NULL,
 PRIMARY KEY (leaderboard_id, player_id),
 FOREIGN KEY(leaderboard_id,project_id,environment_id) REFERENCES leaderboard_definitions(id,project_id,environment_id) ON DELETE CASCADE,
 FOREIGN KEY(player_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS leaderboard_rank_idx ON leaderboard_best_scores(leaderboard_id, rank_score, achieved_at, player_id);

-- All-time goal totals for source='api' rows (spec 6.6), maintained in the same
-- transaction as each ledger insert and rebuildable from the ledgers. Metrics:
-- grant:<item>, grant: (any item), consume:<item>, consume:, credit:<currency>,
-- spend:<currency>, xp, stat:<counter>.
CREATE TABLE IF NOT EXISTS player_goal_totals (
 player_id uuid NOT NULL, project_id uuid NOT NULL, environment_id uuid NOT NULL,
 metric text NOT NULL, total bigint NOT NULL, updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (player_id, metric),
 FOREIGN KEY(player_id,project_id,environment_id) REFERENCES players(id,project_id,environment_id) ON DELETE CASCADE
);

-- Per-tenant quotas; defaults apply when a project has no row.
CREATE TABLE IF NOT EXISTS project_quotas (
 project_id uuid PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 requests_per_minute integer NOT NULL CHECK (requests_per_minute > 0),
 writes_per_minute integer NOT NULL CHECK (writes_per_minute > 0),
 max_batch_entries integer NOT NULL CHECK (max_batch_entries BETWEEN 1 AND 5000),
 updated_at timestamptz NOT NULL DEFAULT now()
);

-- Shared rate-limit counters, so limits hold across API instances. UNLOGGED: losing
-- counters in a crash only resets the current minute, and skipping the WAL keeps this
-- hot table cheap. The maintenance job deletes old windows.
CREATE UNLOGGED TABLE IF NOT EXISTS rate_limit_windows (
 bucket text NOT NULL, window_start timestamptz NOT NULL, count bigint NOT NULL,
 PRIMARY KEY (bucket, window_start)
);

-- Idempotency records expire after 30 days and are purged in batches. After expiry a
-- replayed key is treated as new, so clients must not retry a write after 30 days.
ALTER TABLE idempotency_records ADD COLUMN IF NOT EXISTS expires_at timestamptz NOT NULL DEFAULT now() + interval '30 days';
CREATE INDEX IF NOT EXISTS idempotency_expiry_idx ON idempotency_records(expires_at);
