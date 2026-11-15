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
| 8 | Design doc: goals, flows, capacity, data model, threat model (STRIDE), alternatives ([design.md](design.md)) | done |

## Week 2: Ingest pipeline

Upload a file, store it in SeaweedFS, and let a worker read its tags and cover art in the background.

| Step | What | Status |
| --- | --- | --- |
| 1 | `jobs` table (migration `0002`) and queue design: `SKIP LOCKED`, leases, retries, fencing ([database.md](database.md#jobs)) | done |
| 2 | `internal/storage`: S3 client in Go (AWS SDK), dependency vetting, `govulncheck` ([security.md](security.md#go-modules)) | done |
| 3 | `POST /api/v1/tracks`: streaming upload, size limit, hashing, file-type check ([api.md](api.md#post-apiv1tracks-upload-a-track)) | done |
| 4 | `cmd/worker`: claim, process, retry with backoff, graceful shutdown ([database.md](database.md#how-the-queue-is-used), [security.md](security.md#ingest-worker-cmdworker)) | done |
| 5 | Tag parsing and cover art; fuzz testing the parser | next |
| 6 | Library endpoints: artists, albums, search | to do |
| 7 | Integration tests against real Postgres and SeaweedFS | to do |
| 8 | Docs and commit | to do |

### What Week 2 taught

- **Step 1:** queues in Postgres, `FOR UPDATE SKIP LOCKED`, holding a lock vs a lease, fencing
  tokens, exponential backoff, at-least-once delivery and idempotent jobs, why enqueueing must
  share a transaction with the data it refers to, sequential vs random IDs, foreign keys vs a
  generic payload, partial and partial-unique indexes, untrusted text in error messages.
- **Step 2:** vetting dependencies (publish dates, the two-week rule applied to Go modules,
  who publishes what, minimal vs convenience packages), `go.sum` as a pin, `govulncheck` and
  call-graph reachability, upgrading a transitive dependency for a security fix, building an
  SDK client explicitly instead of from ambient config, path vs virtual-host style S3 URLs
  (and the SDK's silent fallback for IP endpoints), mapping SDK errors to a sentinel
  (`NoSuchKey` vs `NotFound` on HEAD), faking an HTTP service with `httptest.Server`.
- **Step 3:** raw body vs multipart, `http.MaxBytesReader` and why `Content-Length` can't be
  trusted, per-request deadlines with `http.ResponseController` (`ReadTimeout` vs
  `ReadHeaderTimeout`), magic-byte sniffing and allowlists, never trusting the client's
  `Content-Type` or hash, spooling to a temp file because the key is only known at the end,
  `io.MultiWriter`/`io.MultiReader`, telling a disk error from a client error, ordering writes
  across two systems that can't share a transaction, `INSERT … ON CONFLICT DO NOTHING RETURNING`
  and concurrent duplicates under Read Committed, 201 vs 200 for idempotent creates.

- **Step 4:** a worker loop (claim, run, record), leases vs job timeouts (timeout < lease, or
  two workers run one job), fencing in every update, permanent vs transient errors (wrapping
  with a type, `errors.As`), exponential backoff with jitter, the reclaim-past-the-limit gap,
  graceful shutdown (`signal.NotifyContext`, `context.WithoutCancel`, a second signal forces),
  `recover()` so one input can't crash the worker, verifying content-addressed objects
  (`io.TeeReader`, `io.LimitReader`), Postgres `text` rejecting invalid UTF-8 and NUL, testing a
  loop with a fake that cancels the context when drained, why Go can't kill a goroutine.

## What each step taught (Week 1)

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
- **Step 8:** writing a design doc: goals and non-goals, flows, back-of-envelope capacity,
  assets, actors and trust boundaries, STRIDE threat modeling, recording rejected alternatives
  and open questions.

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

`internal/storage` (92.3% coverage) is tested against an `httptest.Server` standing in for S3:

| Test | Checks |
| --- | --- |
| `TestNewValidatesConfig` | every field required; endpoint must be `http(s)://host`; **no error leaks the secret** |
| `TestAudioKey` | 64 lowercase hex only: uppercase, short, long, `../`, `/`, trailing newline rejected |
| `TestPutSignsPathStyleRequest` | `PUT /musicbox/audio/<sha256>`, SigV4 for our access key, body and Content-Type sent, **secret never on the wire** |
| `TestExists` | HEAD (no download); 200 → true, 404 → false, **403 and 500 → error, not "missing"** |
| `TestGet`, `TestGetNotFound` | body returned; `NoSuchKey` → `ErrNotFound` |
| `TestDeleteAndPing`, `TestPingFailsOnWrongKey` | Delete works; Ping is `HEAD /musicbox`; a 403 fails without leaking the secret |

Mutation check: virtual-host style, dropping the HEAD `NotFound` mapping, an unanchored hash
regexp, and removing the scheme check each made a test fail. Virtual-host style survived at
first, because the SDK quietly falls back to path style for an IP endpoint; the tests now use
`localhost` so the setting is really exercised.

Upload (`internal/httpapi/upload_test.go`, package coverage 91.1%; `internal/audio` 100%):

| Test | Checks |
| --- | --- |
| `TestUploadNewTrack` | 201; stored under `audio/<server-computed sha256>`; stored type is the **detected** `audio/flac`, not the client's `text/html`; hash not in the response; temp file gone |
| `TestUploadSameFileTwice` | 200, and the bytes are **not uploaded again** |
| `TestUploadOrphanObject` | object already stored but no row (crash recovery): row created, no second upload |
| `TestUploadRejected` | empty → 400, HTML / ELF / multipart → 415, chunked over 200 MiB → 413, disconnect → 400; **never reaches storage or the database**; temp file gone |
| `TestUploadTooLargeContentLength` | 413 **without reading a byte** |
| `TestUploadServerErrors` | storage HEAD, storage PUT or database failing → generic 500, no leak; **no row when the object wasn't stored** |
| `TestDetect` (`internal/audio`) | each format recognised; HTML, ELF, ZIP, MP4 video, AVI, AAC, multipart and reserved MP3 header values rejected |

Mutation check: removing the read limit, the early `Content-Length` check or the temp-file
cleanup; trusting the client's `Content-Type`; re-uploading existing objects; answering 201 for
duplicates; ignoring a storage error; creating the row after a failed Put. All 8 made a test fail.

Worker (`internal/jobs` 71%: the queue SQL needs Postgres; `internal/ingest` 94.1%):

| Test | Checks |
| --- | --- |
| `TestNewWorkerRejectsTimeoutLongerThanLease` | timeout ≥ lease refused at startup |
| `TestBackoff` | 30 s, 1 m, 2 m, 4 m…, capped at 1 h, no overflow at attempt 1000 |
| `TestSuccessCompletes`, `TestTransientErrorRetriesWithBackoff` | done → Complete; transient on attempt 2 → Retry after 1 m to 1 m 12 s (jitter) |
| `TestLastAttemptFails`, `TestPermanentErrorFailsAtOnce` | attempt 5/5 → Fail; permanent on attempt 1 → Fail, no retry |
| `TestReclaimedPastLimitFailsWithoutRunning` | attempt 6/5 → Fail, **handler never runs** |
| `TestPanicFailsJobAndWorkerContinues` | a panic fails that job; **the next job still runs** |
| `TestJobGetsATimeout` | the handler's context has a deadline within `JobTimeout` |
| `TestLeaseLostDoesNotStopTheWorker` | `ErrLeaseLost` is not a worker error: no pause, next job claimed at once |
| `TestClaimErrorIsRetried` | database down at claim time → worker keeps trying, job done when it's back |
| `TestGracefulShutdown` | shutdown mid-job: **the job's context isn't cancelled**, its result is recorded, no new job claimed |
| `TestCleanError` | invalid UTF-8 replaced, NUL removed, cut at 1000 bytes without splitting a character |
| `TestHandle` (`internal/ingest`) | intact → ok; missing, replaced, truncated, larger, not audio, track deleted → **permanent**; database down, storage down, connection drop mid-read → **transient** |

Mutation check: 11 deliberate bugs (no past-limit check, no `recover`, shutdown cancelling the
job, never giving up, retrying permanent errors, timeout allowed past the lease, lost lease as
an error, linear backoff, hash not checked, missing object retried, storage outage failing
tracks). 10 failed a test at once; the lost-lease one survived because a 1 ms poll hid the
pause, and the test now uses a 1-hour poll so the pause shows up as a timeout.

Not covered yet: the SQL in `internal/tracks` (including `Create`'s transaction and duplicate
handling) and `internal/jobs/queue.go`, the migration runner, and `internal/storage` against
real SeaweedFS (need integration tests, Step 7; all were checked by hand, see below).

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

Week 2, checked on 2026-11-15 against real Postgres and SeaweedFS:

- The API pings storage at startup; with the real keys it starts, applies `0001` and `0002`.
- `internal/storage` (Go client): `Exists` false and `Get` → `ErrNotFound` for a missing key;
  Put, Exists, Get (same bytes) and Delete work; deleting a missing key is not an error. A wrong
  secret gets 403 and the error contains neither secret; writing to another bucket gets 403.
- Upload of a generated 2 s WAV: 201, then 200 for the same file. A FLAC made from it: 201.
  Postgres has one `pending` track per file, each with one `queued` `ingest_track` job, and
  `tracks.sha256` equals `sha256sum` of the file.
- Objects are stored under `audio/<sha256>` with the detected type (`audio/wav`, `audio/flac`);
  downloaded with the AWS CLI, they're byte-identical.
- HTML → 415, empty body → 400, 201 MiB chunked → 413. No `/tmp/musicbox-upload-*` file remains.
- 10 identical uploads of a new file at the same moment: one 201, nine 200, one track, one job.

Worker, checked on 2026-11-15:

- The 3 queued jobs from Step 3: all `done` on attempt 1, tracks `ready`. `SIGTERM` stops it.
- Worker refuses to start when storage is down (fail fast, like the API).
- Object overwritten in storage with 4 changed bytes: job `failed` with "hashes to …", track
  `failed`, no further retries.
- Job set to `running`, attempt 5/5, lease expired (a crashed worker): failed on attempt 6/5
  without running, track `failed`.
- Storage stopped while the worker runs: jobs rescheduled 33 s later (30 s + jitter) with the
  error stored, tracks still `pending`. Storage back: the intact one `done` on attempt 2.
- 20 uploads, 3 workers started together: 7 + 7 + 6 jobs, all 20 `done` on attempt 1, none twice.
- Fencing, with a temporary test against Postgres: a valid lease can't be claimed twice; after
  the lease expires, another claim takes the job at attempt 2; the old owner's Complete gets
  `ErrLeaseLost` and the new owner's `failed` stays, for both job and track.
- Retry for 90 s: `run_at` 90 s ahead and not claimable before; an error with `\xff` and a NUL
  byte stored as "bad � bytes".
