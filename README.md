# auction

Educational artifact auction (MVP): administrators prepare lots and publish
immutable conditions, participants place competing bids, a background worker
inside the server finishes auctions and records the result. Built as a single
Go 1.27 binary on `net/http` with PostgreSQL.

Development status and the implementation plan live in `realisation_steps/`
(steps 01-04 are done); design documents live in `docs/`. Both directories are
kept locally and are not committed.

## Quick start

Requirements: Go 1.27.1, Docker with Compose, GNU make, golangci-lint 2.14.0
(for `make check`).

1. Copy `.env.example` to `.env` and review the values. Local port overrides
   (`POSTGRES_PORT`, `TEST_POSTGRES_PORT`) and all passwords live there.
2. `make migrate` — apply the SQL migrations to the `db` compose service.
3. `make seed` — fill the database with the demo data set (see below).
4. `make run` — start the web server on `HTTP_ADDR`; `GET /livez` returns 200.

`make check` runs the full verification: build, formatting, linter, vet and
tests; `make test-integration` additionally runs the integration-tagged tests
against a real PostgreSQL (`db-test`).

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

## Make targets

| Target                    | Purpose                                            |
|---------------------------|----------------------------------------------------|
| `make build`              | Build `bin/server`                                 |
| `make run`                | Start the web server (loads `.env` on Windows)     |
| `make migrate`            | Apply `migrations/` through the migration container|
| `make seed`               | Fill the database with demo accounts and drafts    |
| `make check`              | Build, format-check, lint, vet, test               |
| `make test-integration`   | Integration tests against `db-test`                |
