# Architecture

## Target system

```
 Clients (React + TypeScript in a Tauri 2 shell: desktop, Android, web)
        │ HTTPS
        ▼
 ┌──────────────┐    ┌───────────────────┐
 │ API service  │    │ Streaming service │   ← split out of the API in W19
 │ (Go)         │    │ (Go)              │
 └──────┬───────┘    └─────────┬─────────┘
        │                      │
        ▼                      ▼
 ┌──────────────┐    ┌───────────────────┐    ┌───────────────┐
 │ Postgres     │◄───│ Ingest worker (Go)│───►│ Object storage│
 │ metadata +   │    │ reads tags,       │    │ S3 API:       │
 │ job queue    │    │ transcodes        │    │ SeaweedFS     │
 └──────────────┘    └───────────────────┘    │ (local), S3   │
                                              │ (AWS)         │
                                              └───────────────┘
```

- **Postgres** stores metadata (tracks, users, playlists) and doubles as the job queue
  (`SELECT ... FOR UPDATE SKIP LOCKED`), so there's no Redis or Kafka to run.
- **Object storage** holds the audio files, named by their SHA-256. The same file uploaded
  twice is stored once, which makes uploads and imports idempotent.
- Audio enters through two paths: an **upload endpoint** and a **folder-import CLI** (`musicctl import`).

## What exists today

Only the API service and Postgres. Object storage arrives in Step 7.

## Layers inside the API

```
cmd/api/main.go                wiring   builds everything, starts the server
internal/httpapi/middleware.go policy   rules applied to ALL requests
internal/httpapi/server.go     HTTP     logic for EACH endpoint
internal/tracks/tracks.go      data     SQL
internal/db/migrate.go         schema   brings the database up to date at startup
```

Each layer only knows the one below it. Handlers never write SQL, and the data layer never
sees an `http.Request`.

### How a request flows

```
request → http.Server → logRequests → securityHeaders → ServeMux → handleListTracks → tracks.Store.List → Postgres
response ←  (logs status, duration)   (headers set)                  (JSON written)
```

### Startup sequence (`main.go`)

1. Read `DATABASE_URL`; exit if it's missing (fail fast).
2. Create the connection pool, and `Ping` with a 5 s timeout so bad credentials fail at startup.
3. Run migrations with a 60 s timeout (may wait for another instance holding the migration lock).
4. Start `http.Server` on `127.0.0.1:8080` with `ReadHeaderTimeout: 5s`.

## Design decisions

| Decision | Why |
| --- | --- |
| Go for the backend | Learning goal; also fast, single static binary, strong standard library for HTTP |
| Standard `net/http` router, no framework | Go 1.22+ `ServeMux` supports method and path patterns, enough for this API |
| `pgx` driver with a connection pool | Each request runs in its own goroutine; a pool shares a few connections safely |
| Interfaces defined by the consumer (`Pinger`, `TrackLister` in `httpapi`) | The HTTP layer can be tested with fakes, no database needed. `*pgxpool.Pool` and `*tracks.Store` satisfy them implicitly |
| Middleware wrapped inside `httpapi.New`, not in `main` | Tests that call `New` cover the middleware too |
| Own migration runner instead of a library | Learning (transactions, advisory locks, `go:embed`); can be swapped for goose/tern later |
| SQL embedded in the binary (`//go:embed`) | One file to deploy; schema can't drift from the code |
| Responses wrapped in an object (`{"tracks": [...]}`), not a bare array | Fields like a pagination cursor can be added later without breaking clients |
| `/healthz` never touches the database | Liveness failure restarts the process; a DB outage shouldn't restart every API instance |
| Postgres as the job queue | One less system to run and secure; enough throughput for this scale |
| SeaweedFS for local object storage | MinIO community edition stopped shipping images (Oct 2025) and was archived |
