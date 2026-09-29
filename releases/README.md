# Release records

`scripts/release.sh` writes one immutable JSON manifest per release into
this directory. Manifests are working records and are not committed; the
release identity is fully reproducible from the commit and the recorded
instructions (see `reproduce` inside each manifest).

## Fields

| Field | Meaning |
|---|---|
| `release_id` | Unique id `r<UTC timestamp>-<short commit>-<hash>`; the hash covers the commit, all image ids, the seed artifact version, the schema version and the configuration fingerprint — so changing any of them (the configuration included) produces a new release id. |
| `commit` | The exact Git commit the images, SQL and seed script were built from. |
| `schema_version` | The migration version the release expects (the server refuses a database with a different one). |
| `images` | Local immutable image ids (`image_id`) and the registry digest (`repo_digest`) when the image store provides one — after a push, or with the containerd image store right after a local build. |
| `seed_artifact` | sha256 over the seed command sources plus `go.mod`/`go.sum`, and the Go version of the module. |
| `configuration` | Path of the deployment env file and its sha256 fingerprint. Secret values are never recorded. |
| `reproduce` | The ordered commands that rebuild and run the release from the recorded commit. |

## How to release

```sh
make release       # builds the images and writes releases/<RELEASE_ID>.json
make migrate       # apply the SQL of the same commit
make seed          # optional demo data
make up            # start PostgreSQL and the server with --no-build
```

`make up` refuses to compile anything: it only runs the images built by
`make release` (`auction/*:local`, the same images the release tagged
`:rel-<RELEASE_ID>`). A migration failure exits non-zero, and the server
then refuses to start on the wrong schema version (`/readyz` 503, exit 1).
