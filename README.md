# RealmStack backend

A Go/Gin modular monolith backed by PostgreSQL. It provides dashboard authentication and tenant-owned projects/environments, hashed/revocable server credentials, player registration and rotating refresh tokens, published environment configuration, inventory/economy, pinned XP progression, PostgreSQL leaderboards, server-reported stats and flags, achievements and quests on a shared goal engine, cloud saves, dashboard player management with bans, an optional AI configuration assistant (bring your own OpenAI key), and the Phase 8 scale layer (best-score and goal-total tables, batch writes, shared rate limits and quotas, partitioned ledgers, optional Redis rank projection). Redis is optional. It needs Go 1.27.

**The API contract is [`api/openapi.yaml`](api/openapi.yaml).** Every route is documented there, `TestOpenAPICoversEveryRoute` fails when routes and the document disagree, and the frontend generates its TypeScript client from it. Update it in the same change as any route.

## Code layout

```text
cmd/api/                 process startup: config, pool, migrations, demo seed, graceful shutdown
internal/platform/       envelopes, typed API errors, principal, tokens, Mutate (idempotency), tenancy helpers
internal/httpapi/        Gin engine, middleware, authentication, the route table; integration tests
internal/persistence/    migration runner (advisory-locked)
internal/auth/           dashboard users
internal/projects/       projects, environments, server credentials, ownership middleware
internal/catalog/        configuration drafts, field-level validation, publishing, definition sync
internal/players/        player identity, refresh-token rotation, profile
internal/playeradmin/    dashboard player management: list/search, profile, and the Target guard for the admin routes (the game's handlers do the work)
internal/bans/           player bans: ban/lift/history routes, and the per-instance cache that playerAuth checks
internal/saves/          cloud saves: versioned JSON slots per player (player, server and dashboard routes share the handlers)
internal/inventory/      item stacks and equipment (Grant is reused by goal rewards)
internal/economy/        balances and history (Credit/Spend are reused by goal rewards)
internal/progression/    XP, levels, version pinning (AwardXP is reused by goal rewards)
internal/leaderboards/   score submissions and deterministic ranking
internal/stats/          server-reported counters and flags
internal/goals/          shared goal engine: conditions, validation, LoadFacts/Evaluate, ApplyRewards
internal/achievements/   achievement progress, hidden masking, claims, claim statistics
internal/quests/         quest periods, pure status evaluation, accept/abandon/claim, quest statistics
internal/ai/             AI assistant: key encryption, OpenAI adapter, strict schemas, prompts, background generations, apply
internal/demo/           local demo seed
migrations/              numbered SQL files
api/openapi.yaml         API contract
```

Packages import `platform` and each other only downward (`ai` → `catalog`; `quests` → `achievements` → `goals` → `inventory`/`economy`/`progression` → `platform`), so there are no cycles; `httpapi` wires everything together.

This folder is an independent Git repository. The integrated Compose setup is owned by the sibling `realmstack-frontend/` repository and builds this API from the sibling path; keep both repositories side-by-side to run the full dashboard + API demo. The Go source walkthrough is in `../study/realmstack-backend-guide.html` when using the original workspace.

## Run a local demo

Requires Docker Desktop with Compose. In this repository folder:

```powershell
docker compose up -d --build --wait
```

This standalone stack is the Compose project `realmstack-backend` (containers `realmstack-backend-db-1` and `realmstack-backend-api-1`, bound to `127.0.0.1`). It shares host port 8080 with the full stack in the frontend repository, so run one at a time.

The API is at `http://localhost:8080`; health check: `GET /healthz`. Compose explicitly sets `APP_ENV=local` and `DEMO_MODE=true`, so it seeds:

| Purpose | Value |
|---|---|
| Dashboard login | `admin@realmstack.local` / `RealmStackLocal!2026` |
| Player login | `player@realmstack.local` / `PlayerLocal!2026` |
| Development public environment ID | `pub_realmstack_demo_dev` |
| Demo server credential | `rs_demo.realmstack-local-server-secret` |
| Published content | Credits, Gems, Health Potion, Iron Sword; progression thresholds 100/300/700/1500 XP; `global` leaderboard |
| Demo player save | slot `slot1` (a small JSON save, inserted once) |

These are demo-only credentials. Seed behavior requires **both** `APP_ENV=local` and `DEMO_MODE=true`. The application defaults to `APP_ENV=production`; the example environment file has `DEMO_MODE=false`. Do not deploy the Compose credentials or demo secrets.

All responses use `{ "data": ..., "meta": { "request_id": "..." } }`; errors use `{ "error": { "code": "...", "message": "...", "details": {} }, "meta": { "request_id": "..." } }`. Validation failures list field-level problems in `details.errors` as `[{ "field": "items[2].max_stack", "message": "..." }]`. Authenticated requests accept bearer tokens. Dashboard and player access tokens are JWTs. Send player registration/login the public environment ID; never ship a server credential in a game client. Use an `Idempotency-Key` header on profile patches and item/currency/XP/score mutations.

### Curl walkthrough

```sh
# Dashboard login
curl -s http://localhost:8080/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"admin@realmstack.local","password":"RealmStackLocal!2026"}'
# Copy access_token to DASHBOARD_TOKEN.

# List demo project and environment (copy the IDs if desired).
curl -s http://localhost:8080/v1/projects -H "Authorization: Bearer $DASHBOARD_TOKEN"

# Player login; use the seeded public environment ID.
curl -s http://localhost:8080/v1/players/login -H 'Content-Type: application/json' \
  -d '{"public_environment_id":"pub_realmstack_demo_dev","email":"player@realmstack.local","password":"PlayerLocal!2026"}'
# Copy access_token to PLAYER_TOKEN.
curl -s http://localhost:8080/v1/players/me -H "Authorization: Bearer $PLAYER_TOKEN"
curl -s http://localhost:8080/v1/players/me/inventory -H "Authorization: Bearer $PLAYER_TOKEN"

# Trusted server grant (local demo credential only).
curl -s http://localhost:8080/v1/server/players/PLAYER_UUID/items/grant \
  -H 'X-Server-Credential: rs_demo.realmstack-local-server-secret' \
  -H 'Idempotency-Key: demo-reward-001' -H 'Content-Type: application/json' \
  -d '{"item_key":"health_potion","quantity":1,"reason":"demo_reward","reference":"match-1"}'

# Read the public leaderboard (ranked by best score, earliest best score breaks ties).
curl -s 'http://localhost:8080/v1/leaderboards/global?environment_id=pub_realmstack_demo_dev'
```

Replace `PLAYER_UUID` with the `player.id` returned at login or registration. For easy interactive testing, the login response returns a player access token; token responses intentionally do not echo secrets other than the newly issued refresh token.

## Route reference

### Dashboard (JWT user token)

* `POST /v1/auth/register`, `POST /v1/auth/login`: return a 15-minute access token and set an httpOnly, `SameSite=Strict` refresh cookie (`rs_dashboard_refresh`, path `/v1/auth`, `Secure` unless `APP_ENV=local`).
* `POST /v1/auth/refresh` rotates that cookie and returns a new access token, so a dashboard reload keeps the session; reusing a rotated cookie revokes the whole sign-in (`401 SESSION_REUSE`). `POST /v1/auth/logout` revokes it and clears the cookie. The refresh token is never in a response body and only its digest is stored.
* `GET|POST /v1/projects` (archived projects are listed only with `?include_archived=true`); `GET|PATCH /v1/projects/{projectId}`
* Projects are never deleted. `PATCH` with `{"status":"archived"}` archives a project: all data is kept, and its server credentials, player registration/login/refresh and public leaderboard reads stop. Already-issued player access tokens run out within their lifetime. `{"status":"active"}` restores it.
* `GET|POST /v1/projects/{projectId}/environments`
* `GET|POST /v1/projects/{projectId}/environments/{envId}/credentials` (lists metadata; shows a new credential once)
* `POST /v1/credentials/{id}/revoke`
* Players of an environment (`players-admin`), for the dashboard: `GET .../environments/{envId}/players?search=&limit=&cursor=` (newest first; `search` matches part of the display name, email or external key, or a whole player ID), `GET .../players/{playerId}` (identity, metadata, last session, active sessions), and `GET .../players/{playerId}/inventory|currency|transactions|progression|stats|achievements|quests`, the same data the game sees. Management, because players never use the dashboard: `POST .../players/{playerId}/items/grant`, `inventory/consume|equip|unequip`, `currency/credit|spend`, `xp/award`, `stats/{statKey}/increment`, `flags/{flagKey}/set`, `achievements/{key}/claim`, `quests/{key}/accept|abandon|claim`, `leaderboards/{key}/scores` and `GET|PUT|DELETE .../saves[/{slot}]`. They run the same handlers and transaction functions as the game, so every rule holds (a balance cannot go below zero, a goal pays once, goals see the writes), with a dashboard token and a required `Idempotency-Key` in the `admin` namespace. Claims ignore the claim policy (an admin can claim either kind). A player outside the owned environment is `404`. Corrections are ordinary ledger rows (`reason` is stored); there is no separate audit log. Bans (`POST .../players/{playerId}/ban`, `.../ban/lift`, `GET .../bans`, `?banned=true` on the list) are described under [Player bans](#player-bans); signing a player out without banning them is not built.
* `GET|PUT /v1/projects/{projectId}/environments/{envId}/configurations/draft`
* `POST /v1/projects/{projectId}/environments/{envId}/configurations/publish`
* `GET /v1/projects/{projectId}/environments/{envId}/configurations/versions`
* `POST /v1/projects/{projectId}/environments/{envId}/configurations/versions/{version}/rollback` republishes an earlier version as a new version (history is never rewritten) and resets the draft to it, keeping `editor_layout`. The old configuration must pass today's rules, including the history-dependent ones.

Definition edits change only the draft:

* `GET|POST /v1/projects/{projectId}/environments/{envId}/items`, `GET|PUT|DELETE .../items/{key}` (likewise `currencies`, `leaderboards`, `stats`, `achievements` and `quests`). Bodies are validated and unknown fields are rejected; a single achievement or quest is shape-checked here and its references (and quest prerequisite cycles) are checked by validate and publish.
* `GET|PUT /v1/projects/{projectId}/environments/{envId}/progression` with `{"thresholds":[100,300,700]}`.
* `POST .../configurations/validate` validates the request body, or the stored draft when the body is empty, and returns `{"valid":bool,"errors":[...]}` without saving.

* `GET /v1/projects/{projectId}/environments/{envId}/achievements/stats` returns claim counts per published achievement (no player identifiers).
* `GET /v1/projects/{projectId}/environments/{envId}/quests/stats` returns acceptance, abandon and claim counts per published quest, summed over all periods (no player identifiers).

Publishing validates the whole draft and answers `422 INVALID_CONFIGURATION` with every field error, such as `achievements[2].conditions[0].stat_key`. It records who published each version. A new progression version is created only when the thresholds change. The direction and season of a leaderboard that already has scores cannot change; start a new season with a new leaderboard key. Likewise a stat cannot switch between counter and flag once values were recorded, and once players have accepted or claimed a quest its `repeat` mode is fixed and its existing stages must stay first and in order (new stages may be appended). Acceptances and claims are stored per period and stage index, so those changes would silently change what the history means; use a new quest key instead.

Draft JSON has `items`, `currencies`, `progression` (increasing XP thresholds to reach levels 2+), `leaderboards`, `stats`, `achievements` and `quests` arrays, plus an optional `editor_layout` object (quest node positions saved by the dashboard; publishing ignores it). Example:

```json
{
  "items":[{"key":"health_potion","name":"Health Potion","category":"consumable","stackable":true,"max_stack":99,"metadata":{"healing":50}}],
  "currencies":[{"key":"credits","name":"Credits"}],
  "progression":[100,300,700],
  "leaderboards":[{"key":"global","direction":"higher"}],
  "stats":[{"key":"trolls_defeated","name":"Trolls defeated","kind":"counter"},{"key":"talked_to_elder","name":"Talked to the elder","kind":"flag"}],
  "achievements":[{"key":"troll_slayer","name":"Troll Slayer","description":"Defeat 100 trolls.","hidden":false,
    "conditions":[{"type":"stat","stat_key":"trolls_defeated","target":100},{"type":"flag","flag_key":"talked_to_elder"}],
    "rewards":{"currencies":[{"currency_key":"credits","amount":10}],"xp":50},"claimable_by":"player"}]
}
```

Quests (`quests` array) reuse the condition set and reward shape:

```json
{"key":"into_the_mine","name":"Into the Mine","chain":"Main Story","requires_quests":["the_awakening"],"requires_achievements":[],
 "repeat":"none","auto_accept":false,"claimable_by":"player","available_from":null,"available_until":null,
 "stages":[{"key":"enter","name":"Enter the old mine","conditions":[{"type":"flag","flag_key":"entered_mine"}],"rewards":{"currencies":[{"currency_key":"credits","amount":50}]}},
           {"key":"ore","name":"Defeat 5 enemies","conditions":[{"type":"stat","stat_key":"enemies_defeated","target":5}]}],
 "rewards":{"xp":200}}
```

* 1–20 ordered stages, each with 1–5 conditions and optional rewards; optional completion rewards. Validation also rejects unknown or self prerequisites, prerequisite cycles (reported on the edge that closes the loop, e.g. `quests[1].requires_quests[0]`), duplicate stage keys, `available_from` not before `available_until`, and unknown `repeat` values.
* Status, first match wins: `claimed` (completion claimed this period) → `locked` (a required quest or achievement is not completed) → `upcoming` → `expired` (window closed, not complete) → `completed` → `active` → `available`. A required quest counts as completed once its completion reward was claimed in any period, or, for a non-repeating quest, as soon as it is complete.
* Progress counts `source='api'` ledger rows from acceptance (or the later start of the current period) until the earlier of the period end and `available_until`. Flags and `level_reached` are state and ignore the window. Stages complete strictly in order, but a counted condition counts everything in the window whichever stage is current; when a step must happen after another, have the game server set a flag.
* `repeat: daily` resets at 00:00 UTC (period key `2026-11-16`), `weekly` at Monday 00:00 UTC (ISO week `2026-W47`). Acceptances and claims are unique per player, quest and period.
* An `auto_accept` quest is active without a call. It counts from when it became available: the latest of its publish time (`activated_at`), the period start, `available_from`, and the recorded claim times of its prerequisites. A prerequisite that completed without a claim has no recorded time, so the window then starts at the latest of the others. Abandoning an auto-accepted quest records the opt-out; the player can accept it again manually.
* Abandoning returns an active quest to `available` and keeps claimed rewards; accepting again sets a new acceptance time, so progress restarts, and stage rewards already claimed this period are not paid again.
* A claim pays every earned, unclaimed stage reward plus the completion reward in one transaction. Ledger rows are tagged `source='quest'`.

Achievement conditions come from a closed set: `xp_total`, `level_reached`, `item_granted`, `item_consumed` (omit `item_key` for any item), `currency_earned`, `currency_spent`, `leaderboard_score`, `stat` and `flag`. Each type accepts only its own fields; 1–5 conditions, all must be met. Progress is computed on read from the immutable ledgers, counting only rows with `source='api'`, so rewards paid by a claim never complete another goal. `level_reached` uses the player's pinned curve and API-awarded XP for the same reason (a deliberate reading of spec 6.4).

Supported leaderboard directions are `higher` and `lower`. Optional `season_start` (inclusive UTC RFC3339 timestamp) and `season_end` (exclusive) may be specified on a leaderboard; omit both for an unseasoned board. Publishing stores an immutable config/progression version and atomically switches active definitions. Existing players retain their originally pinned progression version.

### Player APIs

* `POST /v1/players/register`, `POST /v1/players/login` accept the public environment identifier (as `public_environment_id`, `environment_id`, or the `X-Public-Environment` header), email, and password. Internal environment UUIDs are not accepted. Both return access and opaque refresh tokens; attempts are rate limited per IP and route.
* `POST /v1/players/refresh` accepts `{"refresh_token":"..."}` and rotates the token. Reuse of a revoked token revokes its whole family.
* A banned player gets `403 PLAYER_BANNED` (details `reason`, `expires_at`) from login (after a correct password), refresh and every route that takes a player token. See [Player bans](#player-bans).
* `GET|PATCH /v1/players/me`; `GET /v1/players/me/inventory[/{itemKey}]`
* `POST /v1/players/me/inventory/consume|equip|unequip` (equip, unequip, and consume require an idempotency key)
* `GET /v1/players/me/currency`; `POST /v1/players/me/currency/spend`; `GET /v1/players/me/transactions?limit=50` (newest first; pass `meta.next_cursor` as `?cursor=` for the next page)
* `GET /v1/players/me/progression`
* `GET /v1/players/me/stats` (own counters and flags)
* `GET /v1/players/me/achievements` (status `in_progress` / `completed` / `claimed` with per-condition progress; a hidden achievement still in progress shows only its key and status)
* `POST /v1/players/me/achievements/{key}/claim` (claim policy `player` only; `409 ALREADY_CLAIMED`, `422 NOT_COMPLETED`, `403 CLAIM_NOT_ALLOWED`)
* `GET /v1/players/me/quests` (quest log: status, current stage, per-stage progress, claimable rewards, `resets_at` / `expires_at`)
* `POST /v1/players/me/quests/{key}/accept` (`409 QUEST_ALREADY_ACTIVE` / `ALREADY_CLAIMED`; `422 QUEST_LOCKED`, `QUEST_NOT_AVAILABLE`, `QUEST_EXPIRED`), `.../abandon` (`422 QUEST_NOT_ACTIVE`), `.../claim` (claim policy `player`; `409 ALREADY_CLAIMED`, `422 NOT_COMPLETED`)
* `POST /v1/leaderboards/{key}/scores`; `GET /v1/leaderboards/{key}?environment_id={public_environment_id}&limit=10`
* `GET /v1/players/me/saves`; `GET|PUT|DELETE /v1/players/me/saves/{slot}` (cloud saves; see [Cloud saves](#cloud-saves))

### Trusted server APIs (environment-scoped credential)

Supply `X-Server-Credential: {credential}`:

* `POST /v1/server/players/{playerId}/items/grant`
* `POST /v1/server/players/{playerId}/currency/credit`
* `POST /v1/server/players/{playerId}/xp/award`
* `GET /v1/server/players/{playerId}/stats`; `POST .../stats/{statKey}/increment` (`{"amount":3}`, positive only); `POST .../flags/{flagKey}/set` (body optional; set-only)
* `GET /v1/server/players/{playerId}/achievements` (unmasked); `POST .../achievements/{key}/claim` (any claim policy)
* `GET /v1/server/players/{playerId}/quests`; `POST .../quests/{key}/accept|claim` (claim works for any claim policy)
* `GET /v1/server/players/{playerId}/saves`; `GET|PUT|DELETE .../saves/{slot}` (any player of the credential's environment)
* `POST /v1/server/batch` (`{"entries":[{"type":"grant|credit|xp|stat|flag","idempotency_key":"...","player_id":"...", ...}]}`): up to the project's `max_batch_entries` (default 500). Each entry runs the normal idempotency algorithm in its own savepoint, so a failing entry is reported in the 200 response and does not roll back the others; entries for one player apply in request order. `422 BATCH_SIZE`; `429 QUOTA_EXCEEDED` counts every entry as a write.

The configured item/currency definitions must be published before use. Grant/consume quantities and credit/spend amounts must be positive. Economy, XP, stat, quest and claim writes use one transaction, a scoped idempotency record and an immutable ledger entry (`platform.Env.Mutate`). Ledger rows are stamped with the request clock (`Env.Now`), the same clock that stamps quest acceptances, so quest windows compare like with like (and tests can move time). A claim pays every reward with the normal stack and balance rules; if any reward fails (for example a full stack) the whole claim rolls back. A key replay with the same normalized request returns the saved result; reuse with a different payload yields HTTP 409.

## Cloud saves

Named JSON save slots per player, so a player can continue on another device (post-MVP, 2026-11-22; table `player_saves`, migration `014`).

* `PUT .../saves/{slot}` with `{"data": <any JSON value except null>, "expected_version": 3}` creates (`201`) or replaces (`200`) a slot and returns `{slot, version, size_bytes, created_at, updated_at}`. The version starts at 1 and goes up by one on every write. `GET .../saves` lists slots without data (`meta.max_slots`, `meta.max_bytes`); `GET .../saves/{slot}` adds `data`; `DELETE .../saves/{slot}?expected_version=3` removes one. Every write needs an `Idempotency-Key`.
* **Optimistic concurrency:** send the version you last read as `expected_version` (`0` = the slot must not exist yet). If another device wrote in between, the write is refused with `409 SAVE_VERSION_CONFLICT` and `details.current_version`, and the game decides how to merge. Omit `expected_version` for last-write-wins.
* **Limits:** 10 slots per player (`422 SAVE_SLOT_LIMIT`), 256 KiB per save measured as compacted JSON (`422 SAVE_TOO_LARGE`; the request body limit is 1 MiB). Slot names use the catalog key format (`^[a-z0-9][a-z0-9_-]{0,62}$`). These are constants in `internal/saves`, not per-project settings.
* The same handlers serve the player (`/v1/players/me/saves`), the trusted server (`/v1/server/players/{playerId}/saves`) and the dashboard (`.../environments/{envId}/players/{playerId}/saves`). Saves are client-authoritative when the client writes them; a game that must not trust its client writes saves only from its server. Saves are state, not a ledger: there is no history of earlier versions.

## Player bans

A dashboard admin can ban a player of an environment they own, for a while or permanently (post-MVP, 2026-11-22; table `player_bans`, migration `013`).

* `POST .../players/{playerId}/ban` with `{"reason":"Cheating in ranked","expires_at":"2026-12-01T00:00:00Z"}` (omit `expires_at` for a permanent ban) answers `201 {ban, sessions_revoked}`. The reason is shown to the player, so it is not an internal note. Banning a banned player replaces the ban (to extend, shorten or reword it). `POST .../ban/lift` with an optional `{"note":"..."}` ends it (`422 NOT_BANNED` when none is in force). `GET .../bans` lists the history with a derived `status` (`active`, `expired`, `lifted`, `replaced`) and who issued and ended each ban. The profile carries the ban in force (`ban`), and the player list has a `banned` flag and a `?banned=true` filter.
* **Enforcement:** a banned player gets `403 PLAYER_BANNED` (details `reason`, `expires_at`) at login (only after a correct password, so a ban is never revealed to someone who only knows the email), at refresh (before the reuse check, so a revoked token says "banned" rather than "reuse") and on every route that takes a player access token. Banning revokes every refresh token in the same transaction. After a lift the player signs in again (a revoked refresh token stays revoked).
* **Not affected:** the trusted game server and the dashboard can still act on a banned player (to settle a match or correct a balance), and existing leaderboard scores stay on the boards. Hiding a banned player's scores is deferred: the Redis projection would have to drop and restore them too.
* **How the per-request check stays cheap:** access tokens are stateless and valid for up to 15 minutes, so `playerAuth` must check bans on every request. Each instance keeps every ban in force in memory (`bans.Cache`): the check is a map lookup. Every `BAN_SYNC_INTERVAL` (default 5 s) the first request that finds the copy stale reads the rows changed since the last sync (`player_bans.updated_at`, re-reading the last minute so a late commit is not missed), and every 10 minutes it reloads the whole set. A ban or lift updates the issuing instance at once; other instances apply it within the interval. An expired ban needs no job: expiry is compared on every check.

## Scale and operations (Phase 8)

The runtime is built for many API instances sharing one PostgreSQL database; PostgreSQL stays the source of truth and Redis is optional.

* **Leaderboards:** `leaderboard_best_scores` holds one row per player and board, updated in the score-submission transaction. Top-N is an index scan and a rank is one indexed count. With `REDIS_URL` set, ranks come from a Redis sorted set fed through a transactional outbox (`leaderboard_outbox`); a board is served from Redis only after a full replay, and any mismatch falls back to PostgreSQL.
* **Redis rules:** Redis is trusted once a board is ready, so set `REDIS_URL` on every instance or none (after running without it, run `realmstack-admin rebuild-projection`), and never let Redis evict (`maxmemory-policy noeviction`, the default without a memory limit). Scores are bounded to ±(2^53−1) at submit (`422 VALIDATION_ERROR`) and by a CHECK constraint, because sorted-set scores are float64.
* **Goal totals:** `player_goal_totals` holds all-time sums of `source='api'` ledger rows, written in the same transaction as each ledger row. Achievements read totals; windowed quest conditions still read the ledgers.
* **Quotas and rate limits:** per-project `project_quotas` (defaults `QUOTA_REQUESTS_PER_MINUTE`, `QUOTA_WRITES_PER_MINUTE`, `QUOTA_MAX_BATCH_ENTRIES`) return `429 QUOTA_EXCEEDED` with `Retry-After`. Instances count locally and fold into a shared row every 250 ms, so the overshoot across instances is bounded by what they admit in one interval. Login and registration limits are exact, shared counters in PostgreSQL.
* **Retention:** the four ledgers are range-partitioned by month; a maintenance loop (`MAINTENANCE_INTERVAL`, default 1 h, one instance per run) creates partitions three months ahead and purges idempotency records after 30 days (a key replayed after expiry is treated as new, so clients must not retry a write after 30 days).
* **Connections:** `DATABASE_URL` may point at PgBouncer in transaction mode (the default pgx mode works with PgBouncer 1.21+ and `max_prepared_statements`). `DATABASE_DIRECT_URL` is used for migrations and maintenance, which need session state. `DATABASE_REPLICA_URL` (optional) serves lag-tolerant reads: leaderboard rankings and goal progress. Claims, accepts and every write use the primary.
* **Credential cache:** successful server-credential lookups are cached for `CREDENTIAL_CACHE_TTL` (default 30 s, at most 60 s); the secret is checked on every request, and revoking a credential or archiving a project drops its entries on that instance at once. Other instances notice within the TTL.
* **Ban cache:** each instance holds every ban in force and syncs changes every `BAN_SYNC_INTERVAL` (default 5 s), so checking a player token costs no query (see [Player bans](#player-bans)).
* **Write pipelining:** ledger and total writes whose result nobody reads are queued (`platform.DeferExec`) and sent in the same round trip as the transaction's next statement. Any later statement flushes the queue first, so reads see the rows and a failure surfaces on the statement it was sent with.

Operator commands (`realmstack-admin`, using `DATABASE_DIRECT_URL`):

```sh
realmstack-admin migrate
realmstack-admin maintenance            # partitions, idempotency purge, old rate-limit windows
realmstack-admin rebuild-leaderboards   # best scores from submissions (and replay Redis)
realmstack-admin rebuild-projection     # replay every board into Redis
realmstack-admin rebuild-goal-totals    # totals from the ledgers
realmstack-admin verify                 # compare all derived data with a recomputation; exit 1 on a difference
```

The load test (traffic profile, how to run it, how to read and profile results) is in [loadtest/README.md](loadtest/README.md). Its target is a tenant of 500,000 players, below the spec's 1M; the full-profile exit criterion has not been verified on target hardware. Migrations `007` and `008` recreate the ledgers, so an existing local demo database needs `docker compose down -v`.

## AI configuration assistant (optional)

Users bring their own OpenAI key; the assistant proposes definitions, and people decide. It is off unless the deployment sets `AI_ENCRYPTION_KEY` (a base64-encoded 32-byte key, for example `openssl rand -base64 32`); without it the AI routes answer `503 AI_UNAVAILABLE` and everything else works unchanged. A malformed key stops startup.

* `GET|PUT|DELETE /v1/ai/settings` (per dashboard user): `{"provider":"openai","model":"gpt-5.5","api_key":"sk-..."}`. The key is encrypted at rest with AES-256-GCM (bound to the user ID) and is write-only: responses show only `key_last4`. Omit `api_key` to change only the model.
* **Providers:** `openai` (OpenAI Responses API) or `openai_compatible`: any service that implements Chat Completions (OpenRouter, Together, Groq, vLLM, Ollama, ...) at a `base_url` you choose, e.g. `{"provider":"openai_compatible","base_url":"https://openrouter.ai/api/v1","model":"nvidia/nemotron-3.5-super-vl:free","api_key":"..."}`. The compatible adapter sends the same strict JSON Schema as a `response_format`; a model that does not enforce it yields an `invalid` proposal (rejected, never applied), so use models that support structured output. Saving settings **checks the key and model with the provider first and stores nothing if that fails** (the same approach as Query Pulse): the key via `GET /key` where the service has one, the model against `GET /models`, at no token cost, because many services have no model-retrieval route. Error messages say what to do but never repeat the service's own error text, which can echo part of a key. `POST /v1/ai/settings/test` re-checks what is saved.
* **Why the base URL is restricted:** users choose it, so the server would otherwise call any address they type. It must be `https`, with no credentials, query or fragment, and must not be a private, loopback, link-local or carrier-grade-NAT address (`localhost`, `*.localhost` and `*.internal` are refused too). The check repeats on the address actually dialled (after DNS), and redirects are never followed. A local model server needs the operator to set `AI_ALLOW_PRIVATE_URLS=true`, which also allows `http`. A stored key is only ever sent to the provider and base URL it was saved with, so changing either requires entering the key again (`422 api_key`). `POST /v1/ai/settings/test` re-checks the saved key and model (`422 AI_KEY_REJECTED` / `AI_MODEL_NOT_FOUND`); `PUT` runs the same check before saving.
* `POST /v1/projects/{projectId}/environments/{envId}/ai/generations` with `{"operation":"quests","params":{"brief":"...","count":3,"quest_type":"story","stage_count":3},"merge_policy":"add"}` starts a generation and answers `202`; poll `GET .../ai/generations/{id}`. Operations: `items`, `economy` (currencies plus advisory prices: the configuration has no price field), `progression`, `achievements`, `quests`, `explain` (`params.question`) and `tests` (candidate test cases).
* The model receives an allow-listed view of the **draft only** (keys, names, kinds, conditions, rewards; item metadata reduced to `slot`) plus the brief. No player, ledger, credential or user data is read. The OpenAI request uses a strict JSON Schema per operation, `store: false`, an output token cap and a reasoning effort.
* The output is decoded strictly (unknown fields rejected), merged into a copy of the draft, and checked with `catalog.CheckConfig`, the same rules publish uses. A generation ends `valid`, `invalid` (with field errors), `answered` (explain/tests) or `failed` (with a code such as `AI_RATE_LIMITED`, `AI_OUTPUT_TRUNCATED`, `AI_REFUSED`, `AI_TIMEOUT`). `merge_policy: add` rejects keys that already exist; `replace` overwrites them.
* `POST .../ai/generations/{id}/apply` is the human approval: it merges a `valid` proposal into the **draft** after re-validating it against the current draft (`409 DRAFT_CHANGED` if it no longer fits, `409 ALREADY_APPLIED`, `422 NOT_APPLICABLE`). It never publishes; the normal publish is the second, separate step. `POST .../discard` closes a finished generation.
* Generations run in the background (the HTTP write timeout is 30 s; a generation can take minutes). Ones left running by a restart are marked `failed` (`ABANDONED_BY_RESTART`) on startup; shutdown waits for them within the grace period.
* Limits: `AI_ALLOW_PRIVATE_URLS` (false), `AI_TIMEOUT` (3m), `AI_MAX_OUTPUT_TOKENS` (32000), `AI_GENERATIONS_PER_MINUTE` (6 per user, in process), `AI_MAX_RUNNING_PER_USER` (2). Logs record operation, status, token usage and duration; never keys, prompts or outputs.

Deviation from spec 7.3: AI output is stored as proposals (`ai_generations`) that are merged into the single editable draft, rather than as extra draft configuration versions. Version numbering and the published configuration stay untouched until a person publishes.

## Configuration and checks

Copy `.env.example` to `.env` for direct local runs and set a random JWT secret with at least 32 characters. `docker-compose.yml` is an intentionally local demo setup. SQL files in `migrations/` are applied in order on API startup, under an advisory lock so several instances can start together. Economy, XP, score, stat and claim ledgers (including `quest_claims`) are append-only: the database rejects `UPDATE` and `DELETE` on them. `player_quests` is state, not a ledger, so it changes on abandon and re-accept. Request bodies default to a 1 MiB maximum. Logs are structured JSON and include request IDs; secrets/passwords are not included.

Run tests and static analysis from the repository root:

```sh
go test ./...
go vet ./...
```

PostgreSQL integration tests run when `TEST_DATABASE_URL` is set. To run gofmt, vet, unit and integration tests in containers:

```sh
docker compose -f docker-compose.test.yml run --rm test
```

CI (`.github/workflows/ci.yml`) runs the same checks against a PostgreSQL service.
