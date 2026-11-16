# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A self-hosted music server (Go API + ingest worker, Postgres, SeaweedFS S3 storage), built step by
step as a **learning project**. `docs/progress.md` is the source of truth for which step is done
and which is next; each step also records what it taught, its tests, mutation-check results and
what was verified by hand. Keep it (and `api.md`, `security.md`, `database.md`, `architecture.md`)
updated as part of finishing a step.

**Claude writes the code, and explains it.** The user is learning: for each step, write the
implementation and tests, then explain what was built and why (concepts, trade-offs, security
reasoning), so the explanation can feed `docs/progress.md`. Don't hand the coding back to the user.

Skills to use:

| Skill | When |
| --- | --- |
| `musicbox-tutor` | the explaining part of each build step (not "let the user write it": Claude writes the code) |
| `musicbox-migration` | any table, column, index or constraint change |
| `musicbox-security-review` | new endpoints, and reviewing a diff before a commit |
| `security-review` | security review of the branch's pending changes before committing a step |
| `run` | starting the API and worker against real Postgres/SeaweedFS to verify a change live |

## Commands

```sh
./scripts/gen-secrets.sh                 # creates .env and seaweedfs/s3.json; never overwrites existing values
docker compose up -d --wait              # Postgres + SeaweedFS (bucket creation: see README)
set -a; . ./.env; set +a                 # load env before running anything that talks to them
go run ./cmd/api                         # 127.0.0.1:8080; runs migrations at startup
go run ./cmd/worker                      # start after the API (the worker runs no migrations)

go test ./...                            # unit tests, no Docker needed
go test -race -cover ./...
go test ./internal/httpapi -run 'TestSearch$' -v          # a single test
go test ./internal/probe -run '^$' -fuzz '^FuzzRead$' -fuzztime 60s   # also FuzzText, FuzzNumber
go vet ./... && gofmt -l .
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

If the user isn't in the `docker` group in the current shell, wrap Docker commands:
`sg docker -c "docker compose ..."`. SQL shell:
`docker compose exec postgres psql -U musicbox -d musicbox`.

## Architecture (the parts that span files)

**Two binaries, one queue.** `cmd/api` accepts uploads; `cmd/worker` processes them. They
communicate only through Postgres (`jobs` table) and object storage.

**Upload flow** (`internal/httpapi/upload.go`): sniff magic bytes (`internal/audio`) → spool to a
temp file while hashing → `Put` to `audio/<sha256>` → `tracks.Store.Create` inserts the track **and**
its `ingest_track` job in one transaction (`ON CONFLICT (sha256) DO NOTHING` makes re-uploads
idempotent: 201 new, 200 duplicate). Storage is written before the row, so a crash leaves at worst
an orphan object, never a row without bytes.

**Worker** (`internal/jobs`): claims with `FOR UPDATE SKIP LOCKED` under a lease; every update is
fenced on `attempts` (`ErrLeaseLost` if another worker took over). `JobTimeout` must stay below
`Lease`. Errors are transient (retried with exponential backoff + jitter) unless wrapped with
`jobs.Permanent`. Delivery is at-least-once, so handlers must be idempotent.

**Ingest** (`internal/ingest`): download into a private temp dir, verify size + SHA-256 + audio
header, then parse via `internal/probe`. **Parsing never runs in the worker process**:
`probe.Isolated` re-executes the worker binary with `probe.ChildArg` (handled at the top of
`cmd/worker/main.go`), with an empty environment (no DB/S3 secrets), a timeout kill, `Pdeathsig`
and capped output. Inside the child, TagLib runs as WebAssembly (wazero) that can only see the
file's directory, so the file must be alone in it. Every value from a file is cleaned in
`probe.go` (UTF-8, control/bidi chars, lengths, number ranges, JPEG/PNG only with dimension
check) and re-checked by the parent; the DB enforces the same limits (migration `0003`). Covers
are stored content-addressed at `covers/<sha256>`.

**HTTP layer** (`internal/httpapi`): standard `ServeMux` patterns, middleware (logging, security
headers) wrapped inside `httpapi.New` so tests cover it. Interfaces (`TrackStore`, `ObjectStore`,
…) are defined by the consumer here and satisfied implicitly by `*tracks.Store` / `*storage.Store`;
tests are black-box (`package httpapi_test`) with in-memory fakes. Handlers never write SQL; the
data layer (`internal/tracks`) never sees HTTP. Artists and albums have no tables: they're
`GROUP BY` over ready tracks, an album being `(coalesce(album_artist, artist), album)`.

**Migrations** (`internal/db`): embedded SQL, run by the API under an advisory lock, each file in
one transaction. Never edit an applied migration; no `IF NOT EXISTS`.

## Conventions that aren't obvious from the code

- **Untrusted input is validated before Postgres sees it** (malformed UUID → 404; NUL / invalid
  UTF-8 / oversized query params → 400), because Postgres would turn them into 500s. Clients only
  ever get `{"error":"internal error"}` for 5xx; details go to the log, with client text logged via
  `%q`. Never log query strings.
- **Hashes (`sha256`, `cover_sha256`) are internal** and never appear in API responses or headers.
  Object keys come only from `storage.AudioKey` / `CoverKey` (64 lowercase hex).
- List endpoints always have a `limit` (default 50, max 200), return `[]` not `null`, and wrap
  arrays in an object (`{"tracks":[…]}`).
- **Dependencies and images: the two-week rule.** Adopt a module or image version only if it was
  published at least 14 days ago (check with
  `go list -m -f '{{.Path}} {{.Version}} {{.Time.Format "2006-01-02"}}' all`); pin images by digest.
  Record vetting in `docs/security.md`.
- **Mutation checks are part of a step**: after writing tests, deliberately break the important
  lines and confirm a test fails; record results in `docs/progress.md`.
- Never commit `.env`, `seaweedfs/s3.json`, or audio files (only `internal/probe/testdata/` fixtures
  are allowed, via a `.gitignore` exception). The repo is public. Commit/push only when asked.
