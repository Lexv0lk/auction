GO ?= go
GOLANGCI_LINT ?= golangci-lint
GOLANGCI_LINT_VERSION ?= 2.14.0
GO_TARGET_VERSION := $(shell $(GO) list -m -f "{{.GoVersion}}")
ENV_FILE ?= .env
# Local overrides (ports, password) are read from .env when it exists; see .env.example.
sinclude $(ENV_FILE)

POSTGRES_PORT ?= 5432
TEST_POSTGRES_PORT ?= 5433
# Integration tests use the db-test compose service by default; override with
# your own TEST_DATABASE_URL to run them against another PostgreSQL instance.
export TEST_DATABASE_URL ?= postgres://auction:$(POSTGRES_PASSWORD)@localhost:$(TEST_POSTGRES_PORT)/auction_test?sslmode=disable

# make seed reads the same DATABASE_URL as the server plus the demo account
# passwords; values from .env (included above) or the environment are passed
# through to the script.
export DATABASE_URL
export SEED_ADMIN_PASSWORD
export SEED_PARTICIPANT_PASSWORD

# Load scenarios (step 15): k6 runs on the host, separate from the release
# images. The load tool reads the same .env (ports, demo passwords) and is
# configured only through the environment, like the application itself.
HTTP_PORT ?= 18080
LOAD_BASE_URL ?= http://127.0.0.1:$(HTTP_PORT)
LOAD_RESULTS_DIR ?= load/results
LOAD_APP_VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null)
# Deferred: evaluated only when a load target runs, so plain make check
# never pays for a Docker/k6 probe.
LOAD_DB_VERSION = $(shell docker compose exec -T db postgres --version 2>/dev/null | cut -d' ' -f1-2)
LOAD_K6_VERSION = $(shell k6 version 2>/dev/null)
LOAD_HOST_OS = $(shell uname -srm 2>/dev/null)
export LOAD_BASE_URL LOAD_RESULTS_DIR LOAD_APP_VERSION LOAD_DB_VERSION LOAD_K6_VERSION LOAD_HOST_OS
export LOAD_ADMIN_LOGIN LOAD_ADMIN_PASSWORD LOAD_PARTICIPANT_PASSWORD LOAD_PARTICIPANT_LOGINS
export LOAD_VUS LOAD_WARMUP LOAD_DURATION LOAD_PAUSE LOAD_PROFILE LOAD_RUN_ID
export LOAD_CATEGORIES LOAD_LOTS LOAD_CATALOG_DEADLINE_HOURS
export LOAD_BID_STEP LOAD_BID_START_PRICE LOAD_REPEATS LOAD_BID_MARGIN
export VERIFY_MODE VERIFY_LOT_TITLE VERIFY_TITLE_PREFIX VERIFY_EXPECTED_NEW
export VERIFY_UNCERTAIN VERIFY_EXPECTED_LOTS VERIFY_WAIT VERIFY_OUTPUT

ifeq ($(OS),Windows_NT)
EXE := .exe
RUN := pwsh -NoProfile -File ./scripts/dev.ps1 -EnvFile "$(ENV_FILE)"
else
EXE :=
RUN := $(GO) run ./cmd/server
endif

.PHONY: help go-version build app-help run migrate seed images release up down replicas replicas-down server-logs load-catalog load-bids load-verify lint-version lint fmt fmt-check test vet test-race test-integration test-integration-race test-integration-repeat check

help:
	@echo build             Build bin/server
	@echo app-help          Show server usage
	@echo run               Start the web server from the working tree (host Go)
	@echo images            Build the server, seed and migrations images
	@echo release           Build the images and record releases/<RELEASE_ID>.json
	@echo migrate           Apply SQL from migrations/ with the migration image (make migrate)
	@echo seed              Fill the database with demo accounts and drafts (seed image; make migrate first)
	@echo up                Start PostgreSQL and the server from the built images (--no-build)
	@echo down              Stop the compose services (database data is kept)
	@echo replicas          Start two identical server replicas with unique host ports
	@echo replicas-down     Stop and remove the two replicas (the database stays up)
	@echo server-logs       Follow the server container logs
	@echo load-catalog      Run the catalog-reading load scenario (k6; make up first)
	@echo load-bids         Run the competing-bids load scenario (k6; make up first)
	@echo load-verify       Check the loaded database after a scenario (go run ./load/verify)
	@echo lint              Check Go code with golangci-lint
	@echo fmt               Format Go code with golangci-lint
	@echo fmt-check         Check Go formatting without changing files
	@echo test              Run unit tests
	@echo vet               Run go vet
	@echo test-race         Run tests with race detection
	@echo test-integration  Run integration-tagged tests
	@echo test-integration-race  Run integration tests with race detection
	@echo test-integration-repeat  Repeat integration tests 10 times
	@echo check             Build, format-check, lint, vet and test

go-version:
	$(if $(findstring go$(GO_TARGET_VERSION),$(shell $(GO) version)),@echo go$(GO_TARGET_VERSION),$(error Go $(GO_TARGET_VERSION) is required))

build:
	$(GO) build -o bin/server$(EXE) ./cmd/server

app-help:
	$(GO) run ./cmd/server --help

run:
	$(RUN)

# Release flow (12-factor V): build the artifacts once, then run everything
# else from the built images. `make up` passes --no-build to Compose: no
# compilation and no dependency download happen at run time.
images:
	docker build --target server -t auction/server:local .
	docker build --target seed -t auction/seed:local .
	docker build --target migrations -t auction/migrations:local .

release:
	ENV_FILE="$(ENV_FILE)" bash scripts/release.sh

up:
	docker compose up -d --no-build server

down:
	docker compose down

# Two identical replicas with unique published host ports (12-factor VIII);
# the compose server service is not started here.
replicas:
	docker compose -f compose.yaml -f compose.replicas.yaml up -d --no-build db server-1 server-2

replicas-down:
	docker compose -f compose.yaml -f compose.replicas.yaml stop server-1 server-2
	docker compose -f compose.yaml -f compose.replicas.yaml rm -f server-1 server-2

server-logs:
	docker compose logs -f server

# The load tool writes its JSON reports into load/results; the k6 scripts
# need that directory to exist when the run finishes.
load-catalog:
	@mkdir -p $(LOAD_RESULTS_DIR)
	k6 run load/catalog_read.js

load-bids:
	@mkdir -p $(LOAD_RESULTS_DIR)
	k6 run load/bidding.js

load-verify:
	@mkdir -p $(LOAD_RESULTS_DIR)
	$(GO) run ./load/verify

migrate:
	docker compose run --rm --build migrate

seed:
	docker compose run --rm --build seed

lint-version: go-version
	$(if $(filter $(GOLANGCI_LINT_VERSION),$(shell $(GOLANGCI_LINT) version --short)),@echo golangci-lint $(GOLANGCI_LINT_VERSION),$(error golangci-lint $(GOLANGCI_LINT_VERSION) is required))
	$(if $(findstring built with go$(GO_TARGET_VERSION),$(shell $(GOLANGCI_LINT) version)),@echo golangci-lint built with go$(GO_TARGET_VERSION),$(error golangci-lint must be built with go$(GO_TARGET_VERSION)))

lint: lint-version
	$(GOLANGCI_LINT) config verify
	$(GOLANGCI_LINT) run ./...

fmt: lint-version
	$(GOLANGCI_LINT) fmt

fmt-check: lint-version
	$(GOLANGCI_LINT) fmt --diff

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

test-race:
	$(GO) test -race ./...

# Integration packages share one test database, so -p 1 keeps the packages
# (each of which resets the data) from interfering with each other.
test-integration:
	docker compose up -d --wait db-test
	docker compose run --rm --build migrate-test
	$(GO) test -p 1 -tags=integration ./...

test-integration-race:
	docker compose up -d --wait db-test
	docker compose run --rm --build migrate-test
	$(GO) test -p 1 -race -tags=integration ./...

test-integration-repeat:
	docker compose up -d --wait db-test
	docker compose run --rm --build migrate-test
	$(GO) test -p 1 -tags=integration -count=10 ./...

check: go-version build fmt-check lint vet test
