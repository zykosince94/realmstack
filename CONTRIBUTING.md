# Contributing to the RealmStack API

Thanks for helping. Bug reports, documentation fixes and focused pull requests are welcome.
For anything larger than a fix, open an issue first so we can agree on the approach.

## Set up

You need Go 1.27 and Docker (the integration tests run against PostgreSQL 17).

```sh
gofmt -l . && go vet ./... && go test -count=1 ./...     # unit tests; integration tests skip
docker compose -f docker-compose.test.yml run --rm test   # full suite on PostgreSQL 17
docker compose -f docker-compose.test.yml down            # clean up
```

CI also runs `go test -race ./...` against PostgreSQL and Redis. Run the full suite before you
open a pull request, and say in the description what you did not run.

## Conventions

- **Every route is in `api/openapi.yaml`.** `TestOpenAPICoversEveryRoute` fails when the file
  and the router disagree. Change the contract in the same pull request. The dashboard
  repository regenerates its client from it (`npm run generate:api`, `npm run sync:openapi`).
- **Tenant scope comes from the principal, never from request JSON.** New tables get
  composite foreign keys `(x_id, project_id, environment_id)`, plus cross-tenant and
  cross-environment tests.
- **Every mutation goes through `Mutate`** (idempotency, advisory lock, replay, rollback).
  Value-moving writes are append-only ledger rows; corrections are compensating rows.
- **Migrations are new numbered files.** Never edit one that has been applied: the runner
  records the file name and silently skips edits. Keep DDL idempotent and say in the pull
  request when a migration is destructive.
- **Derived tables** (best scores, goal totals, the Redis projection) need a
  same-transaction writer, a `realmstack-admin rebuild-*` command and a `verify` check.
- **Errors** are `*platform.Error` values with a status code and a stable `code`; validation
  errors carry JSON-path field errors.
- **Comments explain why**, not what. Use `h.Now()` for any time you compare or store.
- Keep pull requests small, add tests with the change, and use conventional commit subjects
  (`feat:`, `fix:`, `docs:`, `chore:`) with a short bullet body.

## Layout

See **Project layout** in the README. Imports point downward only
(`httpapi` → `quests` → `achievements` → `goals` → `inventory`/`economy`/`progression`/`stats`
→ `platform`), and `platform` imports no domain package.

## License

By contributing you agree that your contribution is licensed under the Apache License 2.0,
the same license as the project.
