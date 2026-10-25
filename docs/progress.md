# Progress

## Week 1: API, Postgres, object storage

| Step | What | Status |
| --- | --- | --- |
| 1 | Toolchain (Go 1.27.2, Docker, Compose), GitHub repo (private) | done |
| 2 | Minimal Go server, `GET /healthz` returning JSON | done |
| 3 | Postgres with Docker Compose, `.env` secrets, `tracks` table designed | done |
| 4 | Connection pool (`pgx`), `GET /readyz` | done |
| 5 | Migration runner: embedded SQL, transactions, advisory lock | done |
| 6a | Data layer: `internal/tracks` (`Track`, `Store.List`) | done |
| 6b | `GET /api/v1/tracks` with `limit` validation | done |
| 6c | Handlers moved to `internal/httpapi`; `http.Server` with `ReadHeaderTimeout` | done |
| 6d | Middleware: request logging, security headers | done |
| 6e | Tests: `httptest`, table-driven, fakes through interfaces | next |
| 7 | SeaweedFS (S3 API) in Compose, generated secrets | to do |
| 8 | Design doc, Filesystem MCP server, push | to do |

## What each step taught

- **Step 2:** `net/http` handlers, method patterns (`GET /healthz`), headers are sent on the
  first write (`Content-Type` must be set before the body), errors as values.
- **Step 3:** Compose `.env` interpolation, healthchecks and `--wait`, binding to `127.0.0.1`,
  table design with constraints.
- **Step 4:** liveness vs readiness, fail fast on config, connection pools, context timeouts,
  `WriteHeader` for non-200 status codes.
- **Step 5:** migrations as code, transactional DDL, advisory locks (and why they need one
  dedicated connection), `go:embed`, error wrapping with `%w`.
- **Step 6:** layering (HTTP vs data), NULL columns as `*string`, empty slice vs nil in JSON,
  `rows.Err()`, input validation and 400 vs 500, log injection and `%q`, the `http.Handler`
  interface, consumer-defined interfaces and implicit satisfaction, Slowloris, middleware and
  wrapping `ResponseWriter`, security headers.

## Verified behavior

Checked by hand against a running server and Postgres:

- `/readyz` returns 503 when Postgres stops, and 200 again when it restarts, without restarting the API.
- Migrations apply once; a second start applies nothing. Two instances started together: one
  migrates, the other waits and finds nothing to do.
- A failing migration (`CREATE TABLE oops; SELECT 1/0;`) leaves no `oops` table and no record.
- `limit` values `0`, `201`, `-5`, `abc`, `1.5` all return 400.
- A query cut off by a Postgres shutdown returns 500 (caught by `rows.Err()`).
- An unfinished request is dropped after 5.0 s (`ReadHeaderTimeout`).
- `/x%0Afake` is logged as `"/x\nfake"` on one line; `?token=...` never appears in the log.
