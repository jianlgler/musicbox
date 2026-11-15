# musicbox

A self-hosted music server, shared with friends. Built as a learning project
transition: system design, Go, Postgres, containers, and security practices along the way.

**Status:** Week 2 in progress. The API serves health checks, a track list from Postgres, and uploads;
SeaweedFS provides S3 object storage for audio. See [docs/progress.md](docs/progress.md).

## Quickstart

Prerequisites: Go 1.27+, Docker with Compose v2, `openssl`.

```sh
# 1. Generate secrets into .env and seaweedfs/s3.json (both git-ignored, never commit them).
#    Safe to re-run: existing values are kept.
./scripts/gen-secrets.sh

# 2. Start Postgres and SeaweedFS, wait until both are healthy
docker compose up -d --wait

# 3. Create the storage bucket (once)
echo "s3.bucket.create -name musicbox" | docker compose exec -T seaweedfs weed shell

# 4. Load .env into the shell and run the API (migrations run automatically)
set -a; . ./.env; set +a
go run ./cmd/api

# 5. Try it
curl -i localhost:8080/readyz
curl -i 'localhost:8080/api/v1/tracks?limit=10'
curl -i --data-binary @song.flac localhost:8080/api/v1/tracks   # upload

# 6. In another terminal: the ingest worker (start the API first, it runs the migrations).
#    Run several to process jobs in parallel. Ctrl-C finishes the current job, then stops.
set -a; . ./.env; set +a
go run ./cmd/worker
```

Both containers restart on their own when Docker starts (`restart: unless-stopped`).
To work with the stored files from the command line, see [docs/seaweedfs.md](docs/seaweedfs.md#using-it-from-the-command-line).

## Tests

```sh
go test ./...                 # no Docker or database needed
go test -race -cover ./...    # also check for data races, show coverage
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...   # known vulnerabilities in reachable code
```

The HTTP layer is tested with fakes for the database (see [docs/progress.md](docs/progress.md#automated-tests)).

## Layout

```
cmd/api/main.go                  API wiring: config, pool, storage, migrations, HTTP server
internal/httpapi/server.go       HTTP layer: routes, handlers, validation, JSON
internal/httpapi/middleware.go   rules for every request: logging, security headers
internal/httpapi/server_test.go  HTTP layer tests (fakes, no database)
internal/tracks/tracks.go        data layer: Track type, SQL queries
internal/httpapi/upload.go       POST /api/v1/tracks: size limit, type check, hashing, store, enqueue
internal/storage/storage.go      object storage layer: S3 client (Put, Get, Exists, Delete, Ping)
internal/audio/sniff.go          recognises audio formats from their first bytes
cmd/worker/main.go               ingest worker: wiring, signals
internal/jobs/                   job queue SQL and the worker loop (retries, backoff, shutdown)
internal/ingest/ingest.go        what an ingest job does: verify the stored object
internal/db/migrate.go           migration runner (embedded SQL, advisory lock)
internal/db/migrations/*.sql     schema changes, applied in filename order
compose.yaml                     local Postgres and SeaweedFS (images pinned by digest)
scripts/gen-secrets.sh           generates .env and seaweedfs/s3.json (never overwrites)
docs/                            documentation
```

## Documentation

| Doc | Covers |
| --- | --- |
| [design.md](docs/design.md) | goals and non-goals, key flows, capacity, data model, threat model (STRIDE), alternatives, open questions |
| [architecture.md](docs/architecture.md) | target system, layers, how a request flows, design decisions |
| [api.md](docs/api.md) | every HTTP endpoint, parameters, responses |
| [database.md](docs/database.md) | schema, migrations, how to inspect the database |
| [seaweedfs.md](docs/seaweedfs.md) | object storage: why, how it works, security, setup |
| [security.md](docs/security.md) | security measures in place, and what's planned |
| [progress.md](docs/progress.md) | what's been built, step by step, and what's next |
