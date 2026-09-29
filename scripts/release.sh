#!/usr/bin/env bash
# Build the release artifacts and record the release manifest (12-factor V).
#
# The release binds the application image, the migration image, the seed
# artifact (source checksum + Go version), the schema version and a
# fingerprint of the deployment configuration into one RELEASE_ID; changing
# any of them — the configuration included — produces a new RELEASE_ID even
# when the images stay the same.
#
# Usage:  ENV_FILE=.env scripts/release.sh
# Result: releases/<RELEASE_ID>.json plus the images tagged
#         auction/server|seed|migrations:local (what `make up` runs) and
#         :rel-<RELEASE_ID> (the immutable pointer to this release).
#
# Secrets stay outside Git and images: only the sha256 fingerprint of the
# env file is recorded, never its values.

set -euo pipefail
cd "$(dirname "$0")/.."

error() { printf 'release: %s\n' "$1" >&2; exit 1; }

command -v git >/dev/null 2>&1 || error "git is required"
command -v docker >/dev/null 2>&1 || error "docker is required"
command -v sha256sum >/dev/null 2>&1 || error "sha256sum is required"

ENV_FILE=${ENV_FILE:-.env}
[ -f "$ENV_FILE" ] || error "env file $ENV_FILE not found; copy .env.example and fill it in"

COMMIT=$(git rev-parse HEAD)
SHORT_COMMIT=$(git rev-parse --short HEAD)
if [ -n "$(git status --porcelain)" ]; then
    TREE_CLEAN=false
    echo "release: WARNING - the working tree is dirty; the images may differ from commit $SHORT_COMMIT" >&2
else
    TREE_CLEAN=true
fi

# Schema version: the highest applied-by-this-release migration number.
SCHEMA_VERSION=$(ls migrations/*.up.sql | sed -E 's/.*\/([0-9]{6})_.*/\1/' | sort -n | tail -n 1)
SCHEMA_VERSION=$((10#$SCHEMA_VERSION))

# Configuration reference: a checksum of the deployment env file. The values
# (including secrets) are never recorded.
CONFIG_FINGERPRINT=$(sha256sum "$ENV_FILE" | cut -d' ' -f1)

# Seed artifact version: Go version of the module plus a checksum over the
# seed command sources and the pinned dependency manifests.
SEED_GO_VERSION=$(awk '$1 == "go" {print $2}' go.mod)
SEED_SOURCES=$(find internal/seed scripts/seed -name '*.go' ! -name '*_test.go' | LC_ALL=C sort)
SEED_CHECKSUM=$({ sha256sum $SEED_SOURCES go.mod go.sum; } | sha256sum | cut -d' ' -f1)

echo "release: building the server, seed and migrations images"
# Reproducible artifacts (12-factor V): SOURCE_DATE_EPOCH pins the image
# creation timestamp to the commit time, --provenance=false drops the
# build-time attestation metadata; together they make rebuilding the same
# commit yield the same image id.
SOURCE_DATE_EPOCH=$(git log -1 --format=%ct)
docker build --provenance=false --build-arg SOURCE_DATE_EPOCH="$SOURCE_DATE_EPOCH" --target server -t auction/server:local .
docker build --provenance=false --build-arg SOURCE_DATE_EPOCH="$SOURCE_DATE_EPOCH" --target seed -t auction/seed:local .
docker build --provenance=false --build-arg SOURCE_DATE_EPOCH="$SOURCE_DATE_EPOCH" --target migrations -t auction/migrations:local .

SERVER_IMAGE_ID=$(docker image inspect --format '{{.Id}}' auction/server:local)
SEED_IMAGE_ID=$(docker image inspect --format '{{.Id}}' auction/seed:local)
MIGRATIONS_IMAGE_ID=$(docker image inspect --format '{{.Id}}' auction/migrations:local)
# The registry digest appears when the image store provides one (after a
# push, or with the containerd image store right after a local build); the
# manifest records it when present and the local image id always accompanies
# it, so the release is pinned either way.
SERVER_REPO_DIGEST=$(docker image inspect --format '{{if .RepoDigests}}{{index .RepoDigests 0}}{{end}}' auction/server:local)
MIGRATIONS_REPO_DIGEST=$(docker image inspect --format '{{if .RepoDigests}}{{index .RepoDigests 0}}{{end}}' auction/migrations:local)

# The RELEASE_ID changes on any artifact or configuration change; the UTC
# timestamp keeps consecutive releases of identical inputs apart.
ID_INPUT="${COMMIT}|${SERVER_IMAGE_ID}|${MIGRATIONS_IMAGE_ID}|${SEED_IMAGE_ID}|${SEED_CHECKSUM}|${SEED_GO_VERSION}|${SCHEMA_VERSION}|${CONFIG_FINGERPRINT}"
ID_HASH=$(printf '%s' "$ID_INPUT" | sha256sum | cut -c1-8)
RELEASE_ID="r$(date -u +%Y%m%dT%H%M%SZ)-${SHORT_COMMIT}-${ID_HASH}"

for image in server seed migrations; do
    docker tag "auction/${image}:local" "auction/${image}:rel-${RELEASE_ID}"
done

mkdir -p releases
MANIFEST="releases/${RELEASE_ID}.json"
cat > "$MANIFEST" <<EOF
{
  "release_id": "${RELEASE_ID}",
  "created_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "commit": "${COMMIT}",
  "commit_short": "${SHORT_COMMIT}",
  "working_tree_clean": ${TREE_CLEAN},
  "schema_version": ${SCHEMA_VERSION},
  "images": {
    "server": {
      "tag": "auction/server:rel-${RELEASE_ID}",
      "image_id": "${SERVER_IMAGE_ID}",
      "repo_digest": "${SERVER_REPO_DIGEST}"
    },
    "migrations": {
      "tag": "auction/migrations:rel-${RELEASE_ID}",
      "image_id": "${MIGRATIONS_IMAGE_ID}",
      "repo_digest": "${MIGRATIONS_REPO_DIGEST}"
    },
    "seed": {
      "tag": "auction/seed:rel-${RELEASE_ID}",
      "image_id": "${SEED_IMAGE_ID}"
    }
  },
  "seed_artifact": {
    "source_checksum": "sha256:${SEED_CHECKSUM}",
    "go_version": "${SEED_GO_VERSION}"
  },
  "configuration": {
    "env_file": "${ENV_FILE}",
    "fingerprint": "sha256:${CONFIG_FINGERPRINT}",
    "note": "secret values stay outside git and images; changing the configuration creates a new release id"
  },
  "reproduce": [
    "git checkout ${COMMIT}",
    "ENV_FILE=${ENV_FILE} scripts/release.sh",
    "docker compose up -d --wait db",
    "docker compose run --rm migrate",
    "docker compose run --rm seed            # optional demo data",
    "docker compose up -d --no-build server"
  ]
}
EOF

echo "release: ${RELEASE_ID}"
echo "release: commit ${SHORT_COMMIT}, schema version ${SCHEMA_VERSION}"
echo "release: server      ${SERVER_IMAGE_ID}"
echo "release: migrations  ${MIGRATIONS_IMAGE_ID}"
echo "release: seed        ${SEED_IMAGE_ID} (sources sha256:${SEED_CHECKSUM})"
echo "release: config      sha256:${CONFIG_FINGERPRINT} (${ENV_FILE})"
echo "release: manifest written to ${MANIFEST}"
