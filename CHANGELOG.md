# Changelog

All notable changes to the RealmStack API are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project follows
[Semantic Versioning](https://semver.org/).

## 1.0.0

First public release.

- Multi-tenant projects and environments, revocable server credentials, archive instead of delete.
- Player identity: registration, login, short-lived access tokens, rotating refresh-token families.
- Inventory, currencies and an append-only economy ledger with atomic credit and spend.
- Versioned XP progression with per-player pinning.
- Leaderboards with seasons, deterministic ties and rank, backed by PostgreSQL with an optional
  Redis projection.
- Stats, flags, achievements and quests on one shared goal engine; derived progress, one-time
  rewards.
- Cloud saves with optimistic version checks and player bans with history.
- Dashboard activity (CCU and DAU) derived from session issuance.
- Draft, validate, publish and rollback configuration with immutable versions.
- Optional AI configuration assistant (bring your own OpenAI or OpenAI-compatible key).
- Idempotent mutations, per-project quotas, batch writes, monthly ledger partitions.
- `realmstack-admin` for migrations, maintenance, rebuilds and verification; k6 load-test suite.
- OpenAPI 3 contract in `api/openapi.yaml`.
