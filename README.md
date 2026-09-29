# auction

Educational artifact auction (MVP): administrators prepare lots and publish
immutable conditions, participants place competing bids, a background worker
inside the server finishes auctions and records the result. Built as a single
Go 1.27 binary on `net/http` with PostgreSQL.

Development status and the implementation plan live in `realisation_steps/`
(steps 01-11 are done); design documents live in `docs/`. Both directories are
kept locally and are not committed.

## Quick start

Requirements: Go 1.27.1, Docker with Compose, GNU make, golangci-lint 2.14.0
(for `make check`).

1. Copy `.env.example` to `.env` and review the values. Local port overrides
   (`POSTGRES_PORT`, `TEST_POSTGRES_PORT`) and all passwords live there.
   `CSRF_SECRET` must be an independently generated value of at least 32
   characters (for example `python -c "import secrets; print(secrets.token_urlsafe(48))"`).
2. `make migrate` — apply the SQL migrations to the `db` compose service.
3. `make seed` — fill the database with the demo data set (see below).
4. `make run` — start the web server on `HTTP_ADDR`; `GET /livez` returns 200.

`make check` runs the full verification: build, formatting, linter, vet and
tests; `make test-integration` additionally runs the integration-tagged tests
against a real PostgreSQL (`db-test`).

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
