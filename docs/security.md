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

### Uploads (`POST /api/v1/tracks`)

| Measure | Against |
| --- | --- |
| Size limit 200 MiB: refused up front if `Content-Length` says so, and enforced while reading with `http.MaxBytesReader` (`Content-Length` can lie or be missing) | Filling the disk or RAM with one request |
| `ReadTimeout: 30s` for every request; uploads raise their own deadline to 10 minutes (`http.ResponseController`) | Slowloris on the body: trickling bytes to hold a connection |
| File type from magic bytes, allowlist only; the stored `Content-Type` is the detected one | HTML or scripts stored as "audio" and later served to a browser; executables in the library |
| SHA-256 computed by the server from the bytes it received | A client claiming a hash to overwrite or point at someone else's file |
| Object stored **before** the row; row and job in one transaction | Rows pointing at missing audio; tracks stuck `pending` with no job |
| Temp file always deleted, on success and on every error path (tested) | Copies of uploads piling up on disk |
| Raw body, not `multipart/form-data` | One less parser handling untrusted input |

`/tmp` is RAM-backed (tmpfs) on Fedora, so concurrent large uploads use memory. Set `TMPDIR` to
a directory on disk when running for real.

The upload is still unauthenticated, like every route: the API is reachable only from this
machine until authentication arrives. A per-member quota and rate limit come with it.

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
- **The Go client (`internal/storage`)** keeps the secret key out of every error message and
  never sends it: SigV4 sends only the access key and a signature (tested). Object keys come
  only from `AudioKey`, which accepts exactly 64 lowercase hex characters, so input can't
  produce a key outside `audio/`.
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

#### Go modules

The same two-week rule applies to Go modules: a version is adopted only if it was published at
least 14 days earlier, which gives the community time to notice a compromised or broken release.
`go.sum` pins the exact bytes of every module, like a digest does for images.

Check the publish date of any module version:

```sh
go list -m -f '{{.Path}} {{.Version}} {{.Time.Format "2006-01-02"}}' all
```

Vetting done on 2026-11-15 for the S3 client (cutoff 2026-11-01):

| Module | Version | Published | Why |
| --- | --- | --- | --- |
| `github.com/aws/aws-sdk-go-v2/service/s3` | v1.114.6 | 2026-10-27 | Newest before the cutoff (v1.114.10 was 5 days old) |
| `github.com/aws/aws-sdk-go-v2` + 9 internal modules | (pulled by s3) | 2026-10-27 | Same release; all published by AWS |
| `github.com/aws/smithy-go` | v1.28.7 | 2026-10-27 | AWS's runtime for generated SDKs |
| `golang.org/x/text` | v0.43.0 (was v0.29.0) | 2026-10-09 | Fixes GO-2026-5970 and GO-2026-6629, found by `govulncheck` |
| `golang.org/x/sync` | v0.24.0 (was v0.17.0) | 2026-09-23 | Required by the new `x/text` |

Deliberately **not** used: `aws-sdk-go-v2/config` and `aws-sdk-go-v2/credentials`. `config`
pulls in SSO, STS and EC2 metadata clients and silently reads `~/.aws` and `AWS_*` variables.
The client is built with `s3.New(s3.Options{...})`, so every setting is explicit in
`internal/storage`, and fewer modules means less code to trust.

#### Known vulnerabilities: `govulncheck`

```sh
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

- Run through `go run …@version`: the tool's version is pinned and nothing is installed globally
  or added to `go.mod`. v1.9.0 was published 2026-10-11.
- It checks the Go vulnerability database and reports only vulnerabilities in code the program
  can **actually call** (it follows the call graph), with an example trace, so findings are
  real, not noise.
- Exit status 3 means vulnerabilities were found. That makes it ready for CI.
- First run (2026-11-15): two vulnerabilities in `golang.org/x/text` v0.29.0, reachable through
  `pgxpool.Pool.Acquire` from `db.Migrate`. Fixed by upgrading to v0.43.0; the rerun reports
  `No vulnerabilities found`.

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

`internal/storage/storage_test.go` checks that requests are SigV4-signed with the access key,
that the secret key never appears in a header, URL, body or error, that `AudioKey` rejects
traversal and non-hex input, and that a 403 is never mistaken for "object missing".

## Planned

| Measure | When |
| --- | --- |
| Authentication (JWT), roles, invites | friends-sharing weeks |
| **Authorization on every object** (`WHERE id = $1 AND owner_id = $2`, return 404) to prevent IDOR/BOLA. UUIDs alone are not access control | with playlists |
| Separate Postgres roles: migrator (owns schema) vs app (rows only), so SQL injection can't drop tables | later |
| `Strict-Transport-Security` and `sslmode=require` | when TLS arrives (AWS) |
| Panic-recovery, rate-limiting, request-ID middleware | hardening / observability weeks |
| `gosec`, `govulncheck`, image scanning in CI | CI weeks |
| Structured logging (`log/slog`, JSON) | observability weeks |

## Legal note

Sharing ripped commercial music with friends is likely not covered by private-copy exceptions.
Use Creative Commons music for the shared library.
