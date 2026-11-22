# Load test

A k6 suite and a disposable multi-instance environment for scale and capacity testing. The environment has its own Compose project (`realmstack-load`) and database volume, so it never touches the local demo stack. This directory holds the method; results are not recorded here, because they depend entirely on the hardware (see "Reading results").

## Traffic profile

The reference profile is a tenant with about 1M registered players, about 100k online, about 5k trusted writes per second at peak, and a login surge of about 100k players in 10 minutes. This suite seeds **500,000 players** and models the rest as request rates.

"100k concurrent" is modelled as request rates (constant-arrival-rate scenarios), not 100k virtual users: an online player's client polls now and then, and the rates below are what those players produce.

| Scenario | Rate at `SCALE=1` | What it does |
|---|---|---|
| `server_writes` | 4,500/s | Trusted grant, credit, XP award and stat increment, spread over a sample of 100,000 players |
| `batch_writes` | 10/s | `POST /v1/server/batch` with 50 entries (500 more writes/s) |
| `score_submits` | 150/s | Player score submissions |
| `leaderboard_top` | 400/s | Top 100 of a 500,000-player board |
| `leaderboard_rank` | 400/s | A player's rank (`limit=1`, so it isolates the rank lookup) |
| `player_reads` | 2,000/s | Inventory, currency, progression, achievements and quests reads |
| `login_surge` | 167/s | Player login at bcrypt cost 10 (`LOGIN_DURATION`, default 2 m) |

The steady mix is about 7,500 requests/s. Pass criteria: p95 under 150 ms for writes, under 50 ms for top-100 and for rank; under 1% failed requests. k6 enforces them as thresholds, so a failing run exits non-zero.

## Run it

```sh
cd loadtest
docker compose up -d --build db pgbouncer1 pgbouncer2 pgbouncer3 redis api1 api2 api3
docker compose run --rm seed                  # 500,000 players, once per database
docker compose run --rm k6                    # SCALE=1 DURATION=5m by default
docker compose run --rm verify                # rebuild check on the data the test wrote
docker compose --profile tools down -v        # remove everything, including the database volume
```

Environment variables for `k6`: `SCALE` multiplies every rate (`SCALE=0.5` for a quick run), `DURATION` is the steady phase, `SCENARIOS=server_writes,player_reads` runs a subset (the steady scenarios and `login_surge` can be run separately), `BASE_URLS` overrides the API instances.

Seeding: `PLAYERS` sets the player count and `seed -sample N` the number of players k6 draws from (default 100,000, matching the online population). `seed -resample` keeps the existing tenant and only replaces the sample in `data/tenant.json`; the credential secret exists only in that file, so keep it. The seed refuses to run twice on one database. The seed output and k6 results (`data/`, `results/`) are git-ignored.

The database volume grows quickly: a run of a few minutes writes millions of ledger rows and a lot of WAL. Remove the volume when you are done (`down -v` above), and on Docker Desktop keep its disk image on a drive with room to spare.

## Topology

PostgreSQL 17, three API instances, one PgBouncer per API instance (transaction mode, `max_prepared_statements=1000`), Redis for the rank projection, and k6. Migrations and maintenance connect to PostgreSQL directly (`DATABASE_DIRECT_URL`): they take a session advisory lock and long transactions, which a transaction-mode pooler cannot keep.

## Reading results

- **Run k6 on separate hardware from the database** when you want a result that says something about the platform. A single host running k6, three APIs, three PgBouncers, Redis and PostgreSQL saturates its CPUs, and every request then queues for CPU at every hop; the latencies measure the host, not the design.
- Compare runs at several scales (`SCALE=0.5`, `0.75`, `1`). A healthy system shows flat latency until a resource saturates and then a sharp rise; the scale where it bends tells you the ceiling, and `docker stats` plus the profiling below tell you which resource it is.
- Run the full profile on the hardware you intend to use and treat the thresholds as the pass criteria for that environment.

## Profiling

The load database enables `pg_stat_statements`:

```sh
docker compose exec -T db psql -U realmstack -d realmstack -c \
  "select round((total_exec_time/1000)::numeric,1) s, calls, left(query,100) from pg_stat_statements order by total_exec_time desc limit 15"
```

Sample `pg_stat_activity` (`wait_event_type`, `wait_event`) and `pg_blocking_pids()` while a run is in progress to tell CPU, WAL flushes and row-lock queues apart. `pg_stat_statements_reset()` between runs.

## Design notes

- **PgBouncer is single-threaded.** Its cost is per network round trip, so one shared instance pins a core while PostgreSQL and the APIs have CPU to spare. Hence one PgBouncer per API instance, and write pipelining (`platform.DeferExec`) to cut round trips per mutation.
- **Concurrent batches could deadlock** if they locked random players in opposite orders. A batch applies entries grouped by (lower-cased) player ID, so locks are taken in one global order.
- **Counting the entries ahead of a player** in PostgreSQL costs about 65 ns each, so a mid-table rank on a large board costs tens of milliseconds of CPU. That is the reason for the optional Redis sorted-set projection (`internal/leaderboards/projection.go`).
- **Leaderboard reads** use one pipelined batch instead of three sequential round trips (board, top-N, rank).
- **A small player sample** makes per-player row locks collide far more often than real traffic would; the default sample is 100,000.
- **`exec`/simple-protocol modes** (the usual advice for transaction-mode pooling) break JSON arguments. PgBouncer 1.21+ tracks prepared statements per server connection, so pgx's default mode works with `max_prepared_statements`.

## Correctness checks

Rebuild and verify commands (`realmstack-admin rebuild-leaderboards`, `rebuild-goal-totals`, `rebuild-projection`, `verify`) are covered by the integration suite (`internal/httpapi/scale_test.go`, `projection_test.go`), which compares best scores, goal totals and the Redis projection with a recomputation from the ledgers. `docker compose run --rm verify` runs the same comparison against the data a load test wrote.
