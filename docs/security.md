# Security

What's in place, why, and what's planned. The threat model includes **a malicious friend**:
once the server is shared, authenticated users are not automatically trusted.

## In place

### Secrets

- Credentials live only in `.env`, which is git-ignored and `chmod 600`. Nothing secret is in
  the repo; check with `git ls-files | grep env` (should print nothing).
- The connection string (`DATABASE_URL`) is never logged.
- Don't paste `docker compose config` output anywhere: it prints the password.

### Network exposure

- Postgres is published on `127.0.0.1:5432` only, and SeaweedFS's S3 API on `127.0.0.1:8333`.
  Docker writes its own firewall rules and bypasses `ufw`, so binding to `0.0.0.0` would expose
  them to the network.
- `127.0.0.1` is one layer, not the whole defense: every program on the machine can reach it,
  and web pages can too (DNS rebinding). That's why storage also requires signed requests.
- The API listens on `127.0.0.1:8080` only.

### Input validation

- `limit` is parsed as an integer and bounded to 1–200. Without a cap, `?limit=99999999` would
  load the whole library into memory (cheap denial of service).
- SQL values always go in as parameters (`$1`), never formatted into the query string.
- The database enforces its own rules (`CHECK`, `UNIQUE`, `NOT NULL`), so bad data is rejected
  even if application code has a bug.

### Error handling

- Clients get generic error messages (`internal error`, `unavailable`). Database errors can
  reveal hostnames, users and schema details, so they go to the server log only.
- `rows.Err()` is always checked, so a connection lost mid-query returns 500, not a silently
  shortened list.

### Logging

- **Log injection:** client-controlled text (`r.URL.Path`, query values) is logged with `%q`,
  which escapes newlines. Without it, `/x%0A<fake line>` would forge an extra log line.
- **Query strings are never logged.** Stream tokens will travel in URLs (HTML `<audio>` can't
  send an `Authorization` header).
- Every request is logged by middleware, including ones the router rejects (404, 405).
- **Errors can carry attacker text.** `jobs.last_error` may quote content from an uploaded file
  (for example a tag the parser choked on). It's truncated when stored, and must be treated as
  untrusted wherever it's shown: escaped in logs, never rendered as HTML (stored XSS otherwise).

### HTTP server

- `ReadHeaderTimeout: 5s` defends against **Slowloris** (clients sending headers one byte at a
  time to hold connections open). Verified: an unfinished request is dropped after 5.0 s.
  Plain `http.ListenAndServe` has no timeouts (flagged by `gosec` as G114).

### HTTP security headers

Set on every response by `securityHeaders` middleware:

| Header | Value | Prevents |
| --- | --- | --- |
| `X-Content-Type-Options` | `nosniff` | Browsers guessing content types (e.g. running data as script) |
| `Cache-Control` | `no-store` | Browsers or proxies keeping copies of private data |
| `Referrer-Policy` | `no-referrer` | URLs (and tokens in them) leaking to other sites |
| `Content-Security-Policy` | `frame-ancestors 'none'` | Clickjacking through frames |

### Object storage (SeaweedFS)

Details in [seaweedfs.md](seaweedfs.md).

- **Least privilege:** the API's S3 identity (`musicbox-api`) can only Read, Write and List on
  bucket `musicbox`. It can't create buckets or change permissions (verified: `AccessDenied`).
  Bucket creation goes through the admin console (`weed shell`), reachable only inside the
  container network.
- **No anonymous access** (verified: unsigned requests get 403).
- **Signed requests (SigV4):** the secret key never travels; each request carries an HMAC
  signature scoped to a date, region and service. A wrong secret is refused (verified).
- **Keys generated, not typed:** `scripts/gen-secrets.sh` writes random keys into `.env` and
  `seaweedfs/s3.json`, both git-ignored and `600`. The script never prints a secret.
- **Only the S3 port is published** (`127.0.0.1:8333`). Master, volume and filer have
  unauthenticated admin APIs and stay internal.
- The S3 config is mounted read-only into the container.
- **Keys stay off command lines:** tools get them through environment variables
  (`docker run -e VAR` with no value), never as arguments, where other users could read them
  in the process list.

### Supply chain

Every image is pinned by digest, so a tag moved upstream can't silently change what runs. New
images are only adopted once they're at least two weeks old.

| Image | Version | Digest | Built |
| --- | --- | --- | --- |
| `postgres` | 17.11-alpine | `sha256:b0f9560a…552b24` | (running image at pin time) |
| `chrislusf/seaweedfs` | 4.48 | `sha256:4e61d15f…72d` | 2026-09-28 |
| `amazon/aws-cli` (tool) | 2.37.13 | `sha256:d32864c2…1e5196` | 2026-10-12 |

The Postgres digest is a multi-architecture index, so the same pin works on amd64 and ARM.

### Database

- Random UUIDs for IDs, so they can't be enumerated by counting.
- Migrations run in transactions under an advisory lock (no half-applied schema, no races
  between instances).

### Local machine

- Being in the `docker` group is equivalent to root on this machine.

### Tests that guard security behavior

`internal/httpapi/server_test.go` includes negative assertions, so a regression fails `go test`:
invalid input never reaches the data layer, 500 responses don't leak internal error text,
`sha256` isn't exposed, and all security headers are present on 200, 404 and 405 responses.

## Planned

| Measure | When |
| --- | --- |
| Authentication (JWT), roles, invites | friends-sharing weeks |
| **Authorization on every object** (`WHERE id = $1 AND owner_id = $2`, return 404) to prevent IDOR/BOLA. UUIDs alone are not access control | with playlists |
| Separate Postgres roles: migrator (owns schema) vs app (rows only), so SQL injection can't drop tables | later |
| `Strict-Transport-Security` and `sslmode=require` | when TLS arrives (AWS) |
| Panic-recovery, rate-limiting, request-ID middleware | hardening / observability weeks |
| `gosec`, dependency and image scanning in CI | CI weeks |
| Structured logging (`log/slog`, JSON) | observability weeks |

## Legal note

Sharing ripped commercial music with friends is likely not covered by private-copy exceptions.
Use Creative Commons music for the shared library.
