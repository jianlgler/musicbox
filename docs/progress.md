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
| 6e | Tests: `httptest`, table-driven, fakes through interfaces | done |
| 7 | SeaweedFS (S3 API) in Compose, generated secrets, images pinned by digest ([seaweedfs.md](seaweedfs.md)) | done |
| 8 | Design doc, Filesystem MCP server, push | next |

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
- **Step 6e:** `go test`, black-box test packages (`_test`), `httptest` recorders, table-driven
  subtests, fakes with pointer receivers, negative (security) assertions, `-race` and `-cover`,
  mutation checks (break the code, make sure a test fails).
- **Step 7:** object storage vs database vs local disk, the S3 API as a portable standard,
  SeaweedFS's parts, idempotent secret generation (and why `POSTGRES_PASSWORD` must never be
  regenerated), least-privilege identities, SigV4 signing and the derived signing key, binding
  inside a container vs publishing on the host, pinning images by digest and the two-week rule,
  diagnosing `SignatureDoesNotMatch` (old `curl` signing a different payload hash), keeping
  secrets off command lines.

## Automated tests

`go test ./...` runs in about a second with no Docker. `internal/httpapi` has 95.8% statement
coverage. What `internal/httpapi/server_test.go` covers:

| Test | Checks |
| --- | --- |
| `TestHealthz` | 200 even when the database is down |
| `TestReadyz` | 200 when the database is up, 503 when it's down |
| `TestListTracksLimit` | default 50; 1 and 200 accepted; 0, 201, -5, abc, 1.5 → 400 **and the data layer is never called** |
| `TestListTracksDatabaseError` | 500 with a generic body; **no internal detail leaks** (IP, user, error text) |
| `TestListTracksEmpty` | empty library → `{"tracks":[]}`, not `null` |
| `TestListTracksBody` | JSON field names, `null` for missing tags, `sha256` **not exposed** |
| `TestMethodNotAllowed` | `POST` → 405 |
| `TestSecurityHeaders` | all four headers on a 200, a 405 and a 404 |

Mutation check, done once: raising the cap to 300, removing `nosniff`, or returning the real
database error each made a test fail.

Not covered yet: the SQL in `internal/tracks` and the migration runner (need integration tests
against real Postgres).

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
- SeaweedFS with the API's key: upload, byte-identical download, list and delete work; unsigned
  requests get 403, a wrong secret gets `SignatureDoesNotMatch`, creating a bucket gets
  `AccessDenied`. `gen-secrets.sh` kept all existing values and printed no secret.
