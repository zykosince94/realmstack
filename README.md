# RealmStack backend

A Go/Gin modular monolith backed by PostgreSQL. It provides dashboard authentication and tenant-owned projects/environments, hashed/revocable server credentials, player registration and rotating refresh tokens, published environment configuration, inventory/economy, pinned XP progression, PostgreSQL leaderboards, server-reported stats and flags, and achievements on a shared goal engine. Redis, quests and AI features are not included yet.

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
internal/inventory/      item stacks and equipment (Grant is reused by goal rewards)
internal/economy/        balances and history (Credit/Spend are reused by goal rewards)
internal/progression/    XP, levels, version pinning (AwardXP is reused by goal rewards)
internal/leaderboards/   score submissions and deterministic ranking
internal/stats/          server-reported counters and flags
internal/goals/          shared goal engine: conditions, validation, LoadFacts/Evaluate, ApplyRewards
internal/achievements/   achievement progress, hidden masking, claims, claim statistics
internal/demo/           local demo seed
migrations/              numbered SQL files
api/openapi.yaml         API contract
```

Packages import `platform` and each other only downward (`achievements` → `goals` → `inventory`/`economy`/`progression` → `platform`), so there are no cycles; `httpapi` wires everything together.

This folder is an independent Git repository. The integrated Compose setup is owned by the sibling `realmstack-frontend/` repository and builds this API from the sibling path; keep both repositories side-by-side to run the full dashboard + API demo. The Go source walkthrough is in `../study/realmstack-backend-guide.html` when using the original workspace.

## Run a local demo

Requires Docker Desktop with Compose. In this repository folder:

```powershell
docker compose up --build
```

The API is at `http://localhost:8080`; health check: `GET /healthz`. Compose explicitly sets `APP_ENV=local` and `DEMO_MODE=true`, so it seeds:

| Purpose | Value |
|---|---|
| Dashboard login | `admin@realmstack.local` / `RealmStackLocal!2026` |
| Player login | `player@realmstack.local` / `PlayerLocal!2026` |
| Development public environment ID | `pub_realmstack_demo_dev` |
| Demo server credential | `rs_demo.realmstack-local-server-secret` |
| Published content | Credits, Gems, Health Potion, Iron Sword; progression thresholds 100/300/700/1500 XP; `global` leaderboard |

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

* `POST /v1/auth/register`, `POST /v1/auth/login`
* `GET|POST /v1/projects` (archived projects are listed only with `?include_archived=true`); `GET|PATCH /v1/projects/{projectId}`
* Projects are never deleted. `PATCH` with `{"status":"archived"}` archives a project: all data is kept, and its server credentials, player registration/login/refresh and public leaderboard reads stop. Already-issued player access tokens run out within their lifetime. `{"status":"active"}` restores it.
* `GET|POST /v1/projects/{projectId}/environments`
* `GET|POST /v1/projects/{projectId}/environments/{envId}/credentials` (lists metadata; shows a new credential once)
* `POST /v1/credentials/{id}/revoke`
* `GET|PUT /v1/projects/{projectId}/environments/{envId}/configurations/draft`
* `POST /v1/projects/{projectId}/environments/{envId}/configurations/publish`
* `GET /v1/projects/{projectId}/environments/{envId}/configurations/versions`

Definition edits change only the draft:

* `GET|POST /v1/projects/{projectId}/environments/{envId}/items`, `GET|PUT|DELETE .../items/{key}` (likewise `currencies`, `leaderboards`, `stats` and `achievements`). Bodies are validated and unknown fields are rejected; a single achievement is shape-checked here and its references are checked by validate and publish.
* `GET|PUT /v1/projects/{projectId}/environments/{envId}/progression` with `{"thresholds":[100,300,700]}`.
* `POST .../configurations/validate` validates the request body, or the stored draft when the body is empty, and returns `{"valid":bool,"errors":[...]}` without saving.

* `GET /v1/projects/{projectId}/environments/{envId}/achievements/stats` returns claim counts per published achievement (no player identifiers).

Publishing validates the whole draft and answers `422 INVALID_CONFIGURATION` with every field error, such as `achievements[2].conditions[0].stat_key`. It records who published each version. A new progression version is created only when the thresholds change. The direction and season of a leaderboard that already has scores cannot change; start a new season with a new leaderboard key. Likewise a stat cannot switch between counter and flag once values were recorded.

Draft JSON has `items`, `currencies`, `progression` (increasing XP thresholds to reach levels 2+), `leaderboards`, `stats` and `achievements` arrays. Example:

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

Achievement conditions come from a closed set: `xp_total`, `level_reached`, `item_granted`, `item_consumed` (omit `item_key` for any item), `currency_earned`, `currency_spent`, `leaderboard_score`, `stat` and `flag`. Each type accepts only its own fields; 1–5 conditions, all must be met. Progress is computed on read from the immutable ledgers, counting only rows with `source='api'`, so rewards paid by a claim never complete another goal. `level_reached` uses the player's pinned curve and API-awarded XP for the same reason (a deliberate reading of spec 6.4).

Supported leaderboard directions are `higher` and `lower`. Optional `season_start` (inclusive UTC RFC3339 timestamp) and `season_end` (exclusive) may be specified on a leaderboard; omit both for an unseasoned board. Publishing stores an immutable config/progression version and atomically switches active definitions. Existing players retain their originally pinned progression version.

### Player APIs

* `POST /v1/players/register`, `POST /v1/players/login` accept the public environment identifier (as `public_environment_id`, `environment_id`, or the `X-Public-Environment` header), email, and password. Internal environment UUIDs are not accepted. Both return access and opaque refresh tokens; attempts are rate limited per IP and route.
* `POST /v1/players/refresh` accepts `{"refresh_token":"..."}` and rotates the token. Reuse of a revoked token revokes its whole family.
* `GET|PATCH /v1/players/me`; `GET /v1/players/me/inventory[/{itemKey}]`
* `POST /v1/players/me/inventory/consume|equip|unequip` (equip, unequip, and consume require an idempotency key)
* `GET /v1/players/me/currency`; `POST /v1/players/me/currency/spend`; `GET /v1/players/me/transactions?limit=50` (newest first; pass `meta.next_cursor` as `?cursor=` for the next page)
* `GET /v1/players/me/progression`
* `GET /v1/players/me/stats` (own counters and flags)
* `GET /v1/players/me/achievements` (status `in_progress` / `completed` / `claimed` with per-condition progress; a hidden achievement still in progress shows only its key and status)
* `POST /v1/players/me/achievements/{key}/claim` (claim policy `player` only; `409 ALREADY_CLAIMED`, `422 NOT_COMPLETED`, `403 CLAIM_NOT_ALLOWED`)
* `POST /v1/leaderboards/{key}/scores`; `GET /v1/leaderboards/{key}?environment_id={public_environment_id}&limit=10`

### Trusted server APIs (environment-scoped credential)

Supply `X-Server-Credential: {credential}`:

* `POST /v1/server/players/{playerId}/items/grant`
* `POST /v1/server/players/{playerId}/currency/credit`
* `POST /v1/server/players/{playerId}/xp/award`
* `GET /v1/server/players/{playerId}/stats`; `POST .../stats/{statKey}/increment` (`{"amount":3}`, positive only); `POST .../flags/{flagKey}/set` (body optional; set-only)
* `GET /v1/server/players/{playerId}/achievements` (unmasked); `POST .../achievements/{key}/claim` (any claim policy)

The configured item/currency definitions must be published before use. Grant/consume quantities and credit/spend amounts must be positive. Economy, XP, stat and claim writes use one transaction, a scoped idempotency record and an immutable ledger entry (`platform.Env.Mutate`). A claim pays every reward with the normal stack and balance rules; if any reward fails (for example a full stack) the whole claim rolls back. A key replay with the same normalized request returns the saved result; reuse with a different payload yields HTTP 409.

## Configuration and checks

Copy `.env.example` to `.env` for direct local runs and set a random JWT secret with at least 32 characters. `docker-compose.yml` is an intentionally local demo setup. SQL files in `migrations/` are applied in order on API startup, under an advisory lock so several instances can start together. Economy, XP and score ledgers are append-only: the database rejects `UPDATE` and `DELETE` on them. Request bodies default to a 1 MiB maximum. Logs are structured JSON and include request IDs; secrets/passwords are not included.

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
