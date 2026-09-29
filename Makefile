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

ifeq ($(OS),Windows_NT)
EXE := .exe
RUN := pwsh -NoProfile -File ./scripts/dev.ps1 -EnvFile "$(ENV_FILE)"
else
EXE :=
RUN := $(GO) run ./cmd/server
endif

.PHONY: help go-version build app-help run migrate seed lint-version lint fmt fmt-check test vet test-race test-integration test-integration-race test-integration-repeat check

help:
	@echo build             Build bin/server
	@echo app-help          Show server usage
	@echo run               Start the web server
	@echo migrate           Apply SQL from migrations/ with the migration container
	@echo seed              Fill the database with demo accounts and drafts (make migrate first)
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

migrate:
	docker compose run --rm migrate

seed:
	$(GO) run ./scripts/seed

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
	docker compose run --rm migrate-test
	$(GO) test -p 1 -tags=integration ./...

test-integration-race:
	docker compose up -d --wait db-test
	docker compose run --rm migrate-test
	$(GO) test -p 1 -race -tags=integration ./...

test-integration-repeat:
	docker compose up -d --wait db-test
	docker compose run --rm migrate-test
	$(GO) test -p 1 -tags=integration -count=10 ./...

check: go-version build fmt-check lint vet test
