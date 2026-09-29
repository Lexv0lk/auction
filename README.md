# auction

Educational artifact auction (MVP): administrators prepare lots and publish
immutable conditions, participants place competing bids, a background worker
inside the server finishes auctions and records the result. Built as a single
Go 1.27 binary on `net/http` with PostgreSQL.

Development status and the implementation plan live in `realisation_steps/`
(steps 01-13 are done); design documents live in `docs/`. Both directories are
kept locally and are not committed.

## Quick start

Requirements: Go 1.27.1, Docker with Compose, GNU make, golangci-lint 2.14.0
(for `make check`).

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

## 12-factor summary

The methodology (https://12factor.net) is enforced across the whole project;
the detailed audit with per-factor evidence, verification commands, statuses
and deviations lives in `docs/12factor.md` (local working notes):

| Factor | How this project complies |
|---|---|
| I. Codebase | One Git repository, one Go module; the images, SQL and seed script are built from one commit recorded in the release manifest. |
| II. Dependencies | `go.mod`/`go.sum` pin everything; the image build downloads dependencies in an isolated stage; tools and base images are pinned by exact versions and digests. |
| III. Config | Every deployment-specific value (including secrets) arrives as an independent environment variable; no values in code, no secret defaults, `.env` is local only. |
| IV. Backing services | PostgreSQL is an attachable resource addressed by `DATABASE_URL`; the same image serves any prepared compatible database. |
| V. Build, release, run | `make release` builds immutable artifacts and binds them with the configuration into a unique release id; `make up` runs the built images with `--no-build`. |
| VI. Processes | Sessions, lots, bids and results live only in PostgreSQL; processes are replaceable and share nothing. |
| VII. Port binding | The built-in `net/http` server listens on the configured port (8080 in containers); no external web server. |
| VIII. Concurrency | Identical `server` replicas scale horizontally; each runs HTTP plus the auction-completion loop; coordination happens only in the database. |
| IX. Disposability | Fast startup, SIGTERM handled with a bounded budget, `/readyz` turns 503 when draining, SIGKILL loses nothing. |
| X. Dev/prod parity | Development, tests and the container release run the same PostgreSQL version and the same application image. |
| XI. Logs | JSON events on stdout only; no log files in the process, routing/storage belong to the environment. |
| XII. Admin processes | Migrations and seed are one-off containers of the same release, run separately with the target database configuration. |

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
| `make check`              | Build, format-check, lint, vet, test               |
| `make test-integration`   | Integration tests against `db-test`                |
