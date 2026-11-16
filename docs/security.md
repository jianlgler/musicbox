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
- **Checked before Postgres sees it**, so a bad request is a 400 or 404 and not a 500 (and
  never a database error in the logs that anyone can trigger at will): path ids must be
  canonical UUIDs; text parameters must be valid UTF-8 without NUL (the query string is
  percent-decoded to raw bytes, so `%00` and `%FF` arrive as such), bounded in characters, and
  given once (`?q=a&q=b` is refused rather than silently picking one).
- **LIKE wildcards in search are escaped** (`tracks.EscapeLike`). Parameters stop SQL injection,
  but not pattern injection: inside the pattern, a user's `%` would still mean "anything".

### Cover art (`GET /api/v1/tracks/{id}/cover`)

The one endpoint that returns bytes an uploader chose, to a browser.

- The `Content-Type` comes from the bytes **on every request** (JPEG or PNG magic), not from the
  type stored next to the object. Anything else is a 500 and nothing is sent: served as
  `text/html`, an "image" could be a stored XSS. `nosniff` stops the browser guessing.
- The object key comes from the database row, re-validated by `storage.CoverKey` (64 hex): a
  tampered row can't point the server at `../audio/…` or another object.
- `Cache-Control: private, max-age=86400` replaces the global `no-store` for this response only
  (a cover never changes). `private`: browsers may keep it, shared proxies may not.
- The cover's hash is never sent (no `ETag` from it), like the audio hash.
- No authorization yet: anyone who can reach the API can fetch any cover by track id. That
  arrives with authentication (see [Planned](#planned)).

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

**Known limitation: temp files and concurrent uploads.** Every upload in progress holds a full
temp file (the SHA-256, and so the object key, is only known after the last byte), and nothing
caps how many uploads run at once. The temp file goes in `$TMPDIR`, default `/tmp`, which on
Fedora is tmpfs, backed by RAM. Ten simultaneous 200 MiB uploads would use about **2 GB of
memory**; more would push the machine into swap or the OOM killer. The comment in
`internal/httpapi/upload.go` says the same.

- **Now:** set `TMPDIR` to a directory on disk when running for real (then it's disk space, not
  RAM, but still unbounded).
- **Fix, before untrusted clients can reach the API:** cap concurrent uploads, for example a
  semaphore around the handler (a buffered channel of size N; when it's full, answer 503 with
  `Retry-After` instead of queueing), so the worst case is N × 200 MiB. Per-member quotas and
  rate limits (with authentication) cap it per person; the semaphore caps it in total.

The ingest worker's temp file (`os.MkdirTemp` in `internal/ingest`) has the same tmpfs issue,
but it's already bounded: one file at a time per worker process.

The upload is still unauthenticated, like every route: the API is reachable only from this
machine until authentication arrives. A per-member quota and rate limit come with it.

### Ingest worker (`cmd/worker`)

The worker handles untrusted bytes (anyone with an account can upload), so it's built to fail
one job, never the whole worker:

| Measure | Against |
| --- | --- |
| Separate process from the API | A crash or memory blow-up in parsing can't take the API down |
| `recover()` around each job: a panic fails that job (permanent), the worker carries on | A crafted file that crashes a parser stopping ingest for everyone |
| Per-job timeout (4 min) shorter than the lease (5 min), checked at startup | A hung job, and two workers running the same job |
| A job reclaimed after its last attempt is failed without running | A file that crashes the worker being retried forever |
| Reads at most the expected size + 1 byte from storage | An object replaced by something huge making the worker read forever |
| SHA-256 and size of the stored object checked against the row | Corrupted or tampered objects being marked `ready` |
| Error text cleaned before storing (UTF-8, NUL, 1000 bytes) and logged with `%q` | Bytes from a file breaking the `UPDATE` (stuck job) or forging log lines |
| Fenced updates (`attempts`) | A slow worker overwriting the result of the worker that took over |

#### Parsing tags (`internal/probe`)

Tag parsing is the riskiest code in the project: a mature but large C++ library reading bytes
chosen by any member. It's contained in three layers:

```
worker process ──spawns──▶ child process (same binary, "__probe-child <file>")
                             └─ wazero sandbox ──▶ TagLib compiled to WebAssembly
                                  sees only the file's directory, read-only
```

| Measure | Against |
| --- | --- |
| TagLib runs as WebAssembly in `wazero`: memory bugs stay inside the WASM instance's own memory; no syscalls except reading one mounted directory | A buffer overflow in the C++ parser becoming code execution in the worker |
| Each file in its own `0700` temp directory | The sandbox mounts the whole directory: other files would be readable |
| A child process per file, killed (SIGKILL) after 30 s | A file that hangs the parser (WASM can't be cancelled, and Go can't kill a goroutine) |
| The child gets an **empty environment** (only `TMPDIR`, `GOMEMLIMIT`, `GOMAXPROCS`) | A compromised parser reading `DATABASE_URL` or the S3 keys |
| `Pdeathsig: SIGKILL` | Orphaned parsers when the worker dies |
| Child output capped at 16 MiB, stderr at 4 KiB | A child flooding the worker's memory |
| The child's cover is checked again by the parent | Trusting the child more than the file it parsed |
| Unreadable, hung or crashed → permanent failure | The same bytes hanging a worker 5 times |
| Text: valid UTF-8; control, zero-width and bidi characters removed; whitespace collapsed; ≤ 500 characters; database `CHECK` too | Log forging, terminal escapes, `"evil\u202Egpj.exe"` displaying as `evilexe.jpg`, Postgres rejecting the row |
| Numbers parsed by hand, stopping as soon as they pass the maximum; ranges also in the database | Overflow; years like 0001 |
| Covers: type from the magic bytes (JPEG, PNG only), ≤ 10 MiB, `DecodeConfig` checks dimensions ≤ 8000 px before anything decodes pixels | Decompression bombs; SVG or HTML "covers" served to browsers later |

**A bug that testing caught:** `limitedBuffer` first embedded `bytes.Buffer`, which silently
added `bytes.Buffer`'s `ReadFrom` method. `os/exec` drains the child's output with `io.Copy`,
which calls `ReadFrom` when the destination has one, so the 16 MiB limit was never applied. A
stricter test (checking *why* a flooding child failed) exposed it. `limitedBuffer` now holds the
buffer as a named field, and a test fails if it ever implements `io.ReaderFrom` again.

**Limits:**
- Memory: `GOMEMLIMIT=256MiB` is a soft target, not a hard cap. `RLIMIT_AS` doesn't suit Go or
  wazero (both reserve large virtual address ranges). A hard limit needs a cgroup, which comes
  with running the worker as a container or systemd service.
- Go's fuzzer only sees coverage in Go code, so `FuzzRead` can't steer into TagLib's internals:
  for TagLib it's random testing. The sandbox, not the fuzzing, is the main protection.
- Text is cleaned, not escaped: clients must still escape it when rendering (React does by
  default).

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

Cover images override `Cache-Control` (see [Cover art](#cover-art-get-apiv1tracksidcover)).

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

Vetting done on 2026-11-16 for tag parsing (cutoff 2026-11-02):

| Module | Version | Published | Why |
| --- | --- | --- | --- |
| `go.senan.xyz/taglib` | v0.15.0 | 2026-10-12 | TagLib 2.1.1 compiled to WebAssembly; maintained; LGPL-2.1 (obligations only when distributing binaries) |
| `github.com/tetratelabs/wazero` | v1.11.1-0.20260428013916-2bbd517b7633 | 2026-04-28 | Pure-Go WebAssembly runtime (no cgo). An untagged commit, but the one taglib is built and tested with |
| `golang.org/x/sys` | v0.44.0 (wazero asks for v0.43.0) | 2026-04-23 | Required by wazero. Raised to v0.44.0: `govulncheck` reported GO-2026-5024 in v0.43.0, Windows-only and not called by this code, but the fix was free |

Rejected: `github.com/dhowden/tag` (pure Go, BSD, but untouched since April 2024 with no tagged
release, no WAV, and two 2014–2017 modules pulled in).

**The bundled `taglib.wasm` (388 KB) is a binary blob, so it was verified:** the module's
`repro.yaml` workflow rebuilds it from source on every tag, fails if the result differs, and
publishes a signed build attestation. Checked with:

```sh
gh attestation verify taglib.wasm --repo sentriz/go-taglib --format json \
  --jq '.[].verificationResult.signature.certificate | {buildSignerURI, sourceRepositoryRef}'
```

Result: digest `7cc788ae…8225d3`, built by `repro.yaml@refs/tags/v0.15.0` from commit
`30044ec9`. The bytes in the Go module are the bytes that CI rebuilt from that tag.

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
`library_test.go` adds: malformed UUIDs, NUL and invalid UTF-8 never reach the data layer, and
a cover object that isn't a JPEG or PNG is never served.

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
| **Cap concurrent uploads** (semaphore around the upload handler, 503 when full) and `TMPDIR` on disk, so temp files can't exhaust RAM on tmpfs (see [Uploads](#uploads-post-apiv1tracks)) | before the API faces untrusted clients |
| Panic-recovery, rate-limiting, request-ID middleware | hardening / observability weeks |
| `gosec`, `govulncheck`, image scanning in CI | CI weeks |
| Structured logging (`log/slog`, JSON) | observability weeks |

## Legal note

Sharing ripped commercial music with friends is likely not covered by private-copy exceptions.
Use Creative Commons music for the shared library.
