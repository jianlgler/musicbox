# musicbox

A self-hosted music server, shared with friends. Built as a learning project for a DevSecOps
transition: system design, Go, Postgres, containers, and security practices along the way.

**Status:** Week 1, in progress. The API serves health checks and a track list from Postgres.
See [docs/progress.md](docs/progress.md).

## Quickstart

Prerequisites: Go 1.27+, Docker with Compose v2.

```sh
# 1. Create .env (git-ignored, never commit it) with a random password
cat > .env <<EOF
POSTGRES_USER=musicbox
POSTGRES_DB=musicbox
POSTGRES_PASSWORD=$(openssl rand -hex 24)
DATABASE_URL=postgres://musicbox:\${POSTGRES_PASSWORD}@127.0.0.1:5432/musicbox?sslmode=disable
EOF
chmod 600 .env

# 2. Start Postgres and wait until it's healthy
docker compose up -d --wait

# 3. Load .env into the shell and run the API (migrations run automatically)
set -a; . ./.env; set +a
go run ./cmd/api

# 4. Try it
curl -i localhost:8080/readyz
curl -i 'localhost:8080/api/v1/tracks?limit=10'
```

The password must stay URL-safe (letters and digits), because it's embedded in `DATABASE_URL`.

## Layout

```
cmd/api/main.go                  wiring: config, pool, migrations, HTTP server
internal/httpapi/server.go       HTTP layer: routes, handlers, validation, JSON
internal/httpapi/middleware.go   rules for every request: logging, security headers
internal/tracks/tracks.go        data layer: Track type, SQL queries
internal/db/migrate.go           migration runner (embedded SQL, advisory lock)
internal/db/migrations/*.sql     schema changes, applied in filename order
compose.yaml                     local Postgres
docs/                            documentation
```

## Documentation

| Doc | Covers |
| --- | --- |
| [architecture.md](docs/architecture.md) | target system, layers, how a request flows, design decisions |
| [api.md](docs/api.md) | every HTTP endpoint, parameters, responses |
| [database.md](docs/database.md) | schema, migrations, how to inspect the database |
| [security.md](docs/security.md) | security measures in place, and what's planned |
| [progress.md](docs/progress.md) | what's been built, step by step, and what's next |
