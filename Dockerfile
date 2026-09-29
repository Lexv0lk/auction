# Multi-stage build of the release artifacts (12-factor V): the application
# server, the one-off seed command and the migration container. Build the
# final stages with:
#
#   docker build --target server      -t auction/server:local      .
#   docker build --target seed        -t auction/seed:local        .
#   docker build --target migrations  -t auction/migrations:local  .
#
# Every stage is pinned to exact version tags plus the repository digests
# recorded when the versions were chosen (12-factor II); bump them
# deliberately and re-run the release validation afterwards.

ARG GO_IMAGE=golang:1.27.1-alpine3.24@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414
ARG RUNTIME_IMAGE=alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
ARG MIGRATE_IMAGE=migrate/migrate:v4.20.1@sha256:76cc2074cb6642631f34a898ced71e6aeaa6b1a4d78c4daa743275a22e0c5be7

# ---------------------------------------------------------------- build ------
# The build stage downloads the dependencies pinned in go.mod/go.sum and
# compiles both commands statically; nothing from the host toolchain leaks
# into the binaries.
FROM ${GO_IMAGE} AS build

ENV CGO_ENABLED=0
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/
COPY web/ web/
COPY scripts/seed/ scripts/seed/

RUN go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
    && go build -trimpath -ldflags="-s -w" -o /out/seed ./scripts/seed

# --------------------------------------------------------------- server ------
# The application image: one ready binary and the runtime resources it needs
# (ca-certificates for TLS connections to a managed PostgreSQL; templates and
# static assets are embedded in the binary via web/embed.go, time zone data
# is embedded via time/tzdata).
#
# The app runs as an unprivileged user without subcommands: the exec-form
# entrypoint keeps the Go process as PID 1, so SIGTERM reaches it directly
# and its own signal handler stops HTTP and the background loop.
FROM ${RUNTIME_IMAGE} AS server

RUN apk add --no-cache ca-certificates \
    && addgroup -g 10001 app \
    && adduser -D -H -u 10001 -G app app

COPY --from=build /out/server /server

USER app
EXPOSE 8080
STOPSIGNAL SIGTERM
ENTRYPOINT ["/server"]

# ----------------------------------------------------------------- seed ------
# The one-off administrative seed command as its own image (12-factor XII);
# it expects DATABASE_URL and the SEED_*_PASSWORD variables in the
# environment and exits when done.
FROM ${RUNTIME_IMAGE} AS seed

RUN apk add --no-cache ca-certificates \
    && addgroup -g 10001 app \
    && adduser -D -H -u 10001 -G app app

COPY --from=build /out/seed /seed

USER app
ENTRYPOINT ["/seed"]

# ------------------------------------------------------------ migrations ------
# The migration container: the pinned migrate tool plus the SQL of the same
# commit, so the target environment never mounts SQL from a working checkout.
FROM ${MIGRATE_IMAGE} AS migrations

COPY migrations/ /migrations/

USER nobody
