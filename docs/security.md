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

- Postgres is published on `127.0.0.1:5432` only. Docker writes its own firewall rules and
  bypasses `ufw`, so binding to `0.0.0.0` would expose the database to the network.
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

### Database

- Random UUIDs for IDs, so they can't be enumerated by counting.
- Migrations run in transactions under an advisory lock (no half-applied schema, no races
  between instances).

### Local machine

- Being in the `docker` group is equivalent to root on this machine.

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
