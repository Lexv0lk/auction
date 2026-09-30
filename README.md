# auction

Educational artifact auction (MVP): administrators prepare lots and publish
immutable conditions, participants place competing bids, a background worker
inside the server finishes auctions and records the result. Built as a single
Go 1.27 binary on `net/http` with PostgreSQL.

The implementation plan lives in `realisation_steps/` — steps 01-15 are done
and verified, step 16 (final acceptance) is dated 2026-09-30; the design
documents live in `docs/`. Both directories are kept locally and are not
committed.

## Roles and rules

Two roles only; there is no self-registration — accounts are created by the
seed script.

| | Participant | Administrator |
|---|---|---|
| Browse catalog, lot pages and results | yes | yes |
| Place bids | yes (on running auctions) | no |
| Manage categories, create/edit/delete drafts, publish | no | yes |
| Edit or delete an accepted bid | no | no |

Auction rules enforced by the application and the database:

- A lot goes `draft → active → finished` and never back. Publishing is a
  one-way operation of the administrator; the published conditions (title,
  description, category, start price, deadline) are immutable afterwards.
- Amounts are positive integers in the `int64` range (decimal strings on the
  wire). The first bid must be ≥ the start price, every next bid > the current
  one; at the maximum `int64` no higher bid is possible.
- The deadline is judged by the PostgreSQL clock: a bid is accepted only while
  the transaction reads `now() < ends_at`, so "exactly at the deadline" is
  refused. The decision happens after the row lock, immediately before the
  insert.
- Each participant+lot pair carries a unique request key: replaying an
  accepted bid (same key, same amount) returns the stored bid, the same key
  with a different amount is a conflict, refused attempts reserve nothing.
  Replays keep working after the deadline and after the finish.
- When the deadline passes, a background worker inside every `server` process
  finishes the auction: the maximal accepted bid becomes the winner, a lot
  without bids finishes without a winner. Overdue-but-not-yet-finished lots
  display «Торги завершены, определяется результат» and accept no bids.

Out of MVP scope: wallets and real payments, registration, image uploads,
WebSocket, message queues, Kubernetes, external log/metrics systems and HA
PostgreSQL. Scaling model: identical replicas scale HTTP and the background
loop together; independent scaling of the two is not claimed.

## Stack and repository layout

Go 1.27.1 (`go.mod`), `net/http` and `html/template` (no web framework), a
small vanilla-JS page refresher, `pgx/v5` v5.11.0, `gorilla/csrf` v1.7.3,
Prometheus client v1.24.1, `golang.org/x/crypto` v0.57.0 (bcrypt),
PostgreSQL 18.6, `migrate` v4.20.1 in the migration image. Development
tooling: GNU make, Docker with Compose, golangci-lint 2.14.0, k6 v2.2.0
(load runs on the host, never in the images). All versions are pinned.

```
cmd/server/          the single binary: HTTP + background auction loop
internal/            config, postgres, auth, category, lot, bid, worker,
                     observability, http handlers; multiproc test scenarios
migrations/          plain SQL migrations (applied by the migration container)
scripts/seed/        the demo-data command (its own image)
web/                 templates and static assets, embedded into the binary
load/                k6 scenarios + the Go DB verifier (host-side only)
compose.yaml         db, server, one-off migrate/seed, db-test for integration
compose.replicas.yaml  two identical server replicas (make replicas)
Dockerfile           server/seed/migrations images, pinned bases by digest
Makefile             every command below
docs/, realisation_steps/, releases/, load/results/   local, not committed
```

## Quick start

Requirements: Go 1.27.1, Docker with Compose, GNU make, golangci-lint 2.14.0
(for `make check`); on Windows `make run` additionally uses PowerShell
(`scripts/dev.ps1`). Every command in this README is a Make target and works
identically in PowerShell and POSIX shells.

1. Copy `.env.example` to `.env` and review the values. Local port overrides
   (`POSTGRES_PORT`, `TEST_POSTGRES_PORT`, `HTTP_PORT`) and all passwords live
   there. `CSRF_SECRET` must be an independently generated value of at least
   32 characters (for example `python -c "import secrets; print(secrets.token_urlsafe(48))"`).
2. `make migrate` — apply the SQL migrations to the `db` compose service.
3. `make seed` — fill the database with the demo data set (see below).
4. `make run` — start the web server from the working tree on `HTTP_ADDR`;
   `GET /livez` and `GET /readyz` return 200, `GET /metrics` exports
   Prometheus metrics.

`make check` runs the full verification: build, formatting, linter, vet and
tests; `make test-integration` additionally runs the integration-tagged tests
against a real PostgreSQL (`db-test`).

## Configuration

One `config.Load()` (`internal/config/config.go`) reads every setting; there
are no flags, profiles or subcommands. Values arrive as environment variables;
`.env` (copied from `.env.example`, never committed) feeds local runs and
Compose interpolation. Secret variables have **no defaults** — Compose refuses
to start without them (`:?` syntax). The `.env.example` values themselves are
local educational placeholders, not secrets to reuse anywhere real.

Deployment-level variables (used by Compose and the Makefile, not the server):

| Variable | Meaning |
|---|---|
| `POSTGRES_PASSWORD` | secret; password of the compose PostgreSQL containers |
| `POSTGRES_PORT`, `TEST_POSTGRES_PORT` | host ports of `db` (5432) and `db-test` (5433) |
| `HTTP_PORT`, `REPLICA1_PORT`, `REPLICA2_PORT` | published host ports of the server containers |
| `DATABASE_URL` | connection string for `make seed` and dev runs |
| `SEED_ADMIN_PASSWORD`, `SEED_PARTICIPANT_PASSWORD` | secrets; demo account passwords |

Application variables (all reach the containers as independent env vars):

| Variable | Default | Meaning |
|---|---|---|
| `HTTP_ADDR` | `:8080` in containers | the only listener: app, probes, metrics |
| `CSRF_SECRET` | — (required) | secret; shared CSRF signing key of all replicas |
| `SESSION_TTL` | `24h` | login session lifetime |
| `COOKIE_SECURE` | `false` | set `true` behind HTTPS |
| `LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `METRICS_ENABLED` | `true` | `false` hides `GET /metrics` |
| `DB_MAX_CONNS`, `DB_TIMEOUT` | `10`, `5s` | pool size and single-operation deadline |
| `HTTP_READ_TIMEOUT`, `HTTP_WRITE_TIMEOUT`, `HTTP_IDLE_TIMEOUT` | `10s`/`15s`/`60s` | request timeouts |
| `BID_TX_TIMEOUT` | `5s` | bid transaction deadline |
| `SHUTDOWN_TIMEOUT` | `10s` | shared HTTP+worker shutdown budget (keep < `stop_grace_period`) |
| `WORKER_POLL_INTERVAL`, `WORKER_BATCH_SIZE` | `1s`, `100` | background auction loop cadence |

Inside the containers `HTTP_ADDR=:8080` is fixed by Compose so the container
healthcheck stays bound to a known port; the published host port is the
configuration point — moving the service needs no rebuild.

## Container release

The release artifacts are built by the multi-stage `Dockerfile`: the
application image (`auction/server`), the one-off seed command image
(`auction/seed`) and the migration container (`auction/migrations`, the
pinned `migrate` tool plus the SQL of the same commit). Base images, Go and
the PostgreSQL/migrate versions are pinned by exact versions and digests.
The server runs unprivileged, without subcommands, and handles SIGTERM
itself (the Go process is PID 1); templates, static assets and time zone
data are embedded in the binary.

The release flow separates build, release and run (12-factor V):

```sh
make release       # build the images and record releases/<RELEASE_ID>.json
make migrate       # apply the SQL of the same commit (migration image)
make seed          # optional demo data (seed image)
make up            # start PostgreSQL and the server with --no-build
make down          # stop the services; the database volume is kept
```

`make release` binds the commit, both image ids, the seed artifact checksum
and its Go version, the schema version and a fingerprint of the deployment
configuration into one unique `RELEASE_ID`; changing any of them — the
configuration included — produces a new release id. Secret values stay
outside Git and images: only the checksum of the env file is recorded.
Nothing compiles or downloads at run time; a failed migration exits non-zero
and the server then refuses to start against the wrong schema version
(`/readyz` 503, exit 1).

Two identical replicas (each with HTTP and its own background auction loop,
sharing the database and the CSRF configuration) are started with:

```sh
make replicas      # db + server-1 + server-2 on unique host ports
make replicas-down # remove the replicas, keep the database
```

Every application setting reaches the containers as an independent
environment variable (`compose.yaml`); inside the containers the application
always listens on port 8080, so the published host port (`HTTP_PORT`,
`REPLICA1_PORT`, `REPLICA2_PORT`) is the configuration point. See
`releases/README.md` for the manifest format and the full 12-factor audit in
`docs/12factor.md` (both local working notes).

### Rolling out a new version

Build, release and run stay separate (12-factor V): a new version of the code
is a new release, never an in-place rebuild.

1. Check out the new commit (a dirty working tree is recorded as such in the
   manifest) and run `make release` — it builds the three images and writes a
   new `releases/<RELEASE_ID>.json` (a new commit always yields a new id).
2. `make migrate` — applies the SQL of the same commit through the migration
   image, one-off, while the old server may still be running.
3. `make seed` — optional demo data of the same commit.
4. `make up` — starts the server from the new images with `--no-build`;
   `GET /readyz` must answer 200 (the server refuses to start against a
   schema version other than its own, so a missed migration is visible
   immediately, not half-served).

Two consecutive `make release` runs on the same commit produce identical
image ids (reproducible builds); only the timestamp and the id in the manifest
name differ.

### Rollback conditions

Migrations run forward only — there are no down migrations, and no automatic
data-safe rollback is promised:

- **Schema unchanged** (the previous and the new image expect the same
  `ExpectedSchemaVersion`): rolling the application back is safe — start the
  previous release's images against the same database with the same
  environment. Sessions, lots, bids and results all live in PostgreSQL, so
  nothing else has to move.
- **Schema advanced**: the previous image refuses to start against the newer
  schema (`/readyz` 503, exit 1) — the mismatch is caught by the version
  check, never silently served. Returning to the older application version
  therefore requires resolving the schema explicitly: restore the database
  from a backup taken before the migration, or rebuild the data via
  migrations plus the seed. This is a deliberate, documented limitation.

## Logging in

Open `http://<HTTP_ADDR>/login` and sign in with a demo account, for example
`demo-admin` with the `SEED_ADMIN_PASSWORD` value from `.env`. The session
lives in PostgreSQL (`SESSION_TTL`), so the login survives a server restart;
`POST /logout` in the page navigation ends it. Passwords are verified against
the bcrypt hashes created by the seed; the browser receives only an opaque
random token in an `HttpOnly` cookie, while the database stores its SHA-256
hash. Changing requests (login, logout, and later all forms) are protected by
CSRF tokens signed with the shared `CSRF_SECRET`.

## Managing categories

Administrators manage the category reference data at `/admin/categories`
(link in the navigation): create, rename through the per-category edit form,
and delete unused categories. Names are trimmed, must be 1-120 characters,
and are unique case-insensitively; duplicates and invalid names come back as
form errors with the entered value preserved. A category referenced by at
least one lot is never deleted — the request is refused with a conflict
message, and the foreign key on `lots.category_id` is the final guard even
against a lot created concurrently with the deletion. All category operations
require the admin session and a CSRF token.

## Preparing and publishing lots

Administrators manage auction lots at `/admin/lots`: the list shows every
lot of every status (draft, running, finished) with its category, start
price and deadline, and the edit/publish/delete actions are offered for
drafts only. «Новый лот» opens the form with a title (≤200 characters), a
description (≤5000 characters), a category picked from the reference data, a
positive integer start price and a deadline. New lots are always drafts: the
form never carries `status`, `finished_at` or `winning_bid_id`.

The deadline is a `datetime-local` input plus an explicit time-zone select
(UTC by default and a fixed list of IANA zones): the browser submits a
zone-less wall time, so the server interprets it only in the explicitly
chosen zone and stores the absolute instant as `TIMESTAMPTZ`. Every page
displays deadlines in UTC and says so.

Publishing (`POST /admin/lots/{id}/publish`) opens the bidding: inside one
transaction the lot row is locked (`SELECT … FOR UPDATE`), the draft status
and the stored fields are re-checked, the deadline must be in the future by
the database clock (`now()` of the same transaction), and only then the
status becomes `active`. A past deadline is refused (422) and the draft stays
editable. After publication the conditions are immutable: editing, deleting
and republishing an active or finished lot are conflicts (409), and a
republished lot can never extend its deadline. Concurrent edit and publish
resolve into one consistent version through the row lock: the edit either
lands entirely in the published conditions or is refused with 409. Missing
lots answer 404, invalid fields 422 with the entered values preserved; all
lot operations require the admin session and a CSRF token, and successful
changes redirect (303) so a page reload never resubmits the form.

## Browsing the catalog

Every signed-in user (admin or participant) sees the participant catalog at
`/lots` (link «Каталог» in the navigation). It lists published lots only —
drafts never appear there and answer 404 on direct access, exactly like a
missing lot. The list shows the category, the current price (start price
until the first bid), the display state and the UTC deadline, ordered by
urgency: running auctions with the nearest deadlines first, then lots whose
deadline has passed while the result is being determined, then finished lots.
Filters (`category`, `state`) and pagination (`page`, 20 lots per page)
survive in the address, and unknown filter values fall back to "no filter".

The lot page `/lots/{id}` shows the full conditions, the state, the bid form
for participants on a running auction, the bid history (10 per page) and the
result of a finished auction: the stored winning bid with its participant, or
"no winner" for a finished lot without bids. An active lot whose deadline has
passed shows «Торги завершены, определяется результат» and no bid form — a
bid after the deadline is refused (`auction_closed`) even while the result is
not recorded yet.

Every server process runs the auction-completion loop next to its HTTP
handler (no separate worker binary): each pass takes the overdue lots one by
one — `FOR UPDATE SKIP LOCKED` over the lot rows — and finishes each in its
own short transaction, storing `status = finished`, `finished_at` by the
PostgreSQL clock and the maximal accepted bid as the winner (NULL for a lot
without bids). Concurrent replicas skip lots locked by each other and pick
them up on a later pass, so every lot gets exactly one recorded result no
matter how many instances run or restart. The loop makes its first pass
immediately at startup and repeats every `WORKER_POLL_INTERVAL`, at most
`WORKER_BATCH_SIZE` lots per pass; a temporary database failure is logged and
the pass retries after the same pause, and the interval also bounds the
shutdown of both the HTTP handler and the loop.

The page keeps itself fresh: a small script polls `GET /api/lots/{id}` every
few seconds and updates the state, the current price, the minimum next bid
and the countdown without reloading. When the poller learns that the auction
has been finished, it reloads the page once so the server renders the
recorded result — the winner is never assembled in the browser. The countdown
and every displayed state are computed from the database clock, and money
values travel as decimal strings. On a network failure the page says the
shown data may be stale and resumes refreshing on its own after the
connection returns. Without JavaScript the page is fully readable — every
value is server-rendered.

## Health checks, metrics and logs

The single `server` process serves its probes and the metrics exporter on the
same `HTTP_ADDR` as the application, without a session: `GET /livez` proves
the process answers HTTP (no database work), `GET /readyz` runs a short
database check (pool ping plus the supported schema version) and answers
`503` while the database is unreachable, the schema version differs, or the
graceful shutdown has started — it returns to `200` on its own once the
database is back. `GET /metrics` exports Prometheus metrics (`auction_*`
prefix for HTTP requests, bid outcomes, auction completions, the worker
progress and the connection pool, plus the standard `go_*`/`process_*`
metrics); the environment keeps the exporter off with `METRICS_ENABLED=false`
and restricts access by binding `HTTP_ADDR`. See `docs/observability.md`
(local working notes, not part of the repository) for the metric meanings and
the log format; logs are JSON events on stdout with a `component` field
(`http` or `worker`), request IDs, operation names, outcomes and durations,
and passwords never reach them even in driver errors.

## Demo accounts and demo data

`make seed` runs the one-off script in `scripts/seed` against the database
named by `DATABASE_URL`. It works with the current schema version only, must
not run concurrently with other seed runs, and runs without the server. The
script is a plain Go program, so it behaves identically in PowerShell and
Linux shells; its dependencies are pinned in `go.mod`/`go.sum` (`pgx/v5`
v5.11.0, `golang.org/x/crypto` v0.57.0 for bcrypt).

Fixed demo logins (passwords come from the environment, never from code):

| Login                 | Role         | Password variable           |
|-----------------------|--------------|-----------------------------|
| `demo-admin`          | admin        | `SEED_ADMIN_PASSWORD`       |
| `demo-participant-1`  | participant  | `SEED_PARTICIPANT_PASSWORD` |
| `demo-participant-2`  | participant  | `SEED_PARTICIPANT_PASSWORD` |
| `demo-participant-3`  | participant  | `SEED_PARTICIPANT_PASSWORD` |

The example values in `.env.example` are local educational values only; set
your own before pointing the seed at a database you do not fully control.
Passwords are stored exclusively as bcrypt hashes (cost 10, one random salt
per account) and are never printed by the script.

Demo records are recognized by stable keys and never duplicated or modified on
a repeated run:

- demo users — by login (`demo-*`);
- categories — by case-insensitive name (`Нумизматика`, `Филателия`,
  `Антикварные книги`, `Живопись и графика`);
- lots — by title plus category (four drafts with deadlines a week or more
  ahead of the database clock).

The seed does not publish anything: the first demo lot becomes active through
the manual publication operation of step 07, and draft deadlines can be
changed before publication as usual. If a demo login already belongs to an
account with another role, the seed fails with an explicit conflict error and
leaves that account untouched.

The script prints one summary line with created/skipped counts and exits with
a non-zero code on any error, rolling back the whole run (a partial fill never
stays in the database).

## Testing

| Command | What it does |
|---|---|
| `make test` | unit tests of all packages (no Docker needed) |
| `make check` | build + format-check + lint + vet + unit tests |
| `make test-race` | unit tests with the race detector |
| `make test-integration` | integration tests (`-tags=integration`) against the real `db-test` PostgreSQL |
| `make test-integration-repeat` | the same, 10 consecutive runs of the concurrent scenarios |
| `make test-integration-race` | integration tests with the race detector (reliable on Linux/CI) |

Integration tests start `db-test` and the migration container themselves and
require `TEST_DATABASE_URL` (the Makefile builds it from `.env`); without it
they fail fast instead of touching the dev database. The multi-process suite
(`internal/multiproc`) runs real `server` processes against a shared database
and proves the replica properties: session sharing, exactly-once auction
finishing, kill/replacement, database outage and recovery. Windows notes:
`scripts/test-integration.ps1` is the PowerShell equivalent of the three
integration targets, and the SIGTERM test is skipped there (the signal is not
deliverable to a child process on Windows; covered on Linux/CI). Details and
the scenario list live in `docs/testing.md`.

## Load scenarios

Two reproducible k6 profiles (step 15) run on the host against the release
container; k6 v2.2.0 is a host tool and never enters the images.

```sh
make load-catalog                                   # catalog reading, small: 3 clients
make load-catalog LOAD_VUS=15 LOAD_PROFILE=heavy    # heavy reading
make load-bids                                      # competing bids on one lot, small
make load-bids LOAD_VUS=12 LOAD_PAUSE=0.05 LOAD_PROFILE=heavy  # heavy competition
make load-verify VERIFY_MODE=bids VERIFY_LOT_TITLE=… VERIFY_EXPECTED_NEW=…  # DB check after
```

Each run authenticates through the normal forms (sessions + CSRF, no test
backdoors), counts `201`/`200`/`409`-by-reason/`422`/`5xx`/timeouts separately,
treats `409 bid_too_low` as an expected competitive outcome, replays accepted
bids with the same request key, and writes a JSON report (conditions, versions,
machine, thresholds — no secrets) into `load/results/`. After a bids run the
verifier waits for the worker and checks the database: unique keys, strictly
rising accepted amounts, the winner equals the maximal accepted bid. Baseline
results and their limits are recorded in `docs/load-testing.md`; no invented
RPS targets — the goal is reproducibility and data correctness under load.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Compose fails with `POSTGRES_PASSWORD is required` | `.env` missing or incomplete: copy `.env.example` to `.env` and fill the three secrets (`POSTGRES_PASSWORD`, `SEED_*_PASSWORD`, `CSRF_SECRET`). |
| Port already allocated (5432/5433/18080…) | Another PostgreSQL or service holds the port — override `POSTGRES_PORT`, `TEST_POSTGRES_PORT`, `HTTP_PORT` in `.env` (this development machine uses 15432/15433 because 5432 is taken). |
| Server exits with `database schema is not initialized` | Migrations were not applied — run `make migrate` first; the server never migrates on start. |
| `GET /readyz` returns 503 | The database is unreachable or its schema version differs from the binary's (`docker compose ps db`, re-run `make migrate`). It returns to 200 on its own once the database is back. |
| Bid or form answers `503 service_unavailable` | The database is temporarily unavailable; the request can be retried — accepted bids are idempotent by request key. |
| `422` on publish | The deadline is not in the future by the database clock; edit the draft and publish again. |
| Login or forms answer `403 csrf_invalid` | The `CSRF_SECRET` changed (invalidating signed cookies) or the cookie was dropped — open `/login` again and log in anew. |
| Seed fails with a role conflict | A demo login already exists with another role; point the seed at a fresh database or drop the conflicting row manually — the seed never modifies foreign accounts. |
| `make run` prints PowerShell errors on Windows | `make run` uses `pwsh`/`scripts/dev.ps1`; run `make up` (containers) instead, or install PowerShell. |
| `make test-integration` fails to connect | `db-test` is not up or `TEST_POSTGRES_PORT` differs from `.env`; the target starts the database itself, so check Docker is running. |
| Load report missing | k6 is not installed or not in `PATH` (`k6 version`); it is a host tool — `winget install GrafanaLabs.k6` or the equivalent. |

## 12-factor summary

The methodology (https://12factor.net) is enforced across the whole project;
the detailed audit with per-factor evidence, verification commands, statuses
and deviations lives in `docs/12factor.md` (local working notes).

Scope of the verdict: everything below was verified on local development
conditions (Windows + Docker, Linux-only tests noted); there is no production
environment in the MVP, so no claim extends beyond those conditions — what a
real deployment still has to prove is listed under factors X and V and in the
audit.

| Factor | Status | How this project complies |
|---|---|---|
| I. Codebase | complies | One Git repository, one Go module; the images, SQL and seed script are built from one commit recorded in the release manifest. |
| II. Dependencies | complies | `go.mod`/`go.sum` pin everything; the image build downloads dependencies in an isolated stage; tools and base images are pinned by exact versions and digests. |
| III. Config | complies | Every deployment-specific value (including secrets) arrives as an independent environment variable; no values in code, no secret defaults, `.env` is local only. |
| IV. Backing services | complies | PostgreSQL is an attachable resource addressed by `DATABASE_URL`; the same image serves any prepared compatible database. |
| V. Build, release, run | partial | `make release` builds immutable artifacts and binds them with the configuration into a unique release id; `make up` runs the built images with `--no-build`. Partial: the manifest records the local immutable image id; a registry digest requires an image registry that the MVP does not have. |
| VI. Processes | complies | Sessions, lots, bids and results live only in PostgreSQL; processes are replaceable and share nothing. |
| VII. Port binding | complies | The built-in `net/http` server listens on the configured port (8080 in containers); no external web server. |
| VIII. Concurrency | complies | Identical `server` replicas scale horizontally; each runs HTTP plus the auction-completion loop; coordination happens only in the database. |
| IX. Disposability | complies | Fast startup, SIGTERM handled with a bounded budget, `/readyz` turns 503 when draining, SIGKILL loses nothing. |
| X. Dev/prod parity | partial | Development, tests and the container release run the same PostgreSQL version and the same application image. Partial: there is no production environment to compare against; TLS, a managed database, a secret manager and a registry are the pre-deployment checklist. |
| XI. Logs | complies | JSON events on stdout only; no log files in the process, routing/storage belong to the environment. |
| XII. Admin processes | complies | Migrations and seed are one-off containers of the same release, run separately with the target database configuration. |

The two partial statuses are deliberate, explained deviations (missing
registry, missing production), not hidden non-compliance; each carries its
cause, consequences and the condition under which it closes in `docs/12factor.md`.

## Make targets

| Target                    | Purpose                                            |
|---------------------------|----------------------------------------------------|
| `make build`              | Build `bin/server`                                 |
| `make run`                | Start the web server from the working tree (loads `.env` on Windows) |
| `make images`             | Build the server, seed and migrations images       |
| `make release`            | Build the images and record the release manifest   |
| `make migrate`            | Apply `migrations/` through the migration image    |
| `make seed`               | Fill the database with demo accounts and drafts (seed image) |
| `make up` / `make down`   | Start/stop the compose services (`--no-build`, data is kept) |
| `make replicas`           | Start two identical server replicas (unique host ports) |
| `make replicas-down`      | Stop and remove the replicas (the database stays up) |
| `make server-logs`        | Follow the server container logs                   |
| `make load-catalog` / `make load-bids` | Run a load scenario (k6 on the host, `make up` first) |
| `make load-verify`        | Verify the database after a load run               |
| `make check`              | Build, format-check, lint, vet, test               |
| `make test` / `make test-race` | Unit tests (optionally with the race detector) |
| `make test-integration`   | Integration tests against `db-test`                |
| `make test-integration-repeat` | 10 consecutive runs of the integration tests  |
| `make test-integration-race` | Integration tests with the race detector        |
| `make fmt` / `make fmt-check` | Format Go code (or only check the formatting)  |

## Documentation map

`docs/` and `realisation_steps/` are local working notes (not committed):
`docs/requirements.md` — MVP scope and rules; `docs/architecture.md` —
layers and lifecycle; `docs/http-api.md` — the full HTTP contract;
`docs/schema.md` — tables and constraints; `docs/testing.md` — test kinds
and the multi-process scenarios; `docs/load-testing.md` — load profiles,
parameters and baseline results; `docs/observability.md` — metrics and log
events; `docs/12factor.md` — the per-factor audit; `docs/acceptance.md` —
the acceptance report of step 16.
