# Database

Postgres 17, run locally with Docker Compose (`compose.yaml`). Data lives in the named
volume `pgdata`, so it survives `docker compose down` (but not `down -v`).

## Schema

### `tracks`

One row per audio file.

| Column | Type | Rules | Notes |
| --- | --- | --- | --- |
| `id` | `uuid` | primary key, default `gen_random_uuid()` | Random, so IDs can't be guessed by counting |
| `sha256` | `text` | `NOT NULL`, `UNIQUE`, must match `^[0-9a-f]{64}$` | Content hash; the object's name in storage. `UNIQUE` makes re-imports harmless and gives a fast "do we have this file?" lookup |
| `size_bytes` | `bigint` | `NOT NULL`, `> 0` | `bigint` because `integer` stops at ~2.1 GB |
| `status` | `text` | `NOT NULL`, default `'pending'`, one of `pending`/`ready`/`failed` | Set by the ingest worker |
| `title`, `artist`, `album` | `text` | nullable | `NULL` until the ingest worker reads the tags |
| `created_at` | `timestamptz` | `NOT NULL`, default `now()` | Stored in UTC, shown in the session's time zone |

`now()` returns the start time of the *transaction*, so rows inserted by one statement share
the same `created_at`. That's why ordering always adds `id` as a tiebreaker.

### `jobs`

The work queue (migration `0002_jobs.sql`). One row per piece of background work; for now the
only kind is `ingest_track` (read tags, extract cover art, mark the track ready).

| Column | Type | Rules | Notes |
| --- | --- | --- | --- |
| `id` | `bigint` | identity, primary key | Sequential is fine: jobs are never exposed to clients |
| `kind` | `text` | `NOT NULL`, one of `ingest_track` | Extended when a new kind of job appears |
| `track_id` | `uuid` | `NOT NULL`, references `tracks(id)`, `ON DELETE CASCADE` | A real foreign key, so the track is guaranteed to exist |
| `status` | `text` | `queued` / `running` / `done` / `failed`, default `queued` | |
| `attempts` | `integer` | `>= 0`, default 0 | Incremented on every claim; doubles as the fencing token |
| `max_attempts` | `integer` | `> 0`, default 5 | After this many failures the job is `failed` for good |
| `run_at` | `timestamptz` | default `now()` | Not claimable before this time; retries move it into the future |
| `locked_until` | `timestamptz` | required when `running` | The lease; once in the past, the job can be reclaimed |
| `last_error` | `text` | nullable | Truncated; may contain text from the uploaded file (untrusted) |
| `created_at`, `updated_at` | `timestamptz` | default `now()` | |

Table check `jobs_check`: a `running` job must have a lease (`locked_until`).

**Indexes:**

| Index | Columns | Why |
| --- | --- | --- |
| `jobs_one_active_per_track` | unique `(kind, track_id)` where `queued` or `running` | At most one active job per track: re-uploads can't enqueue duplicate work. A new job is allowed once the old one is `done` or `failed` |
| `jobs_queued_run_at` | `(run_at)` where `queued` | What the claim query searches |
| `jobs_running_lease` | `(locked_until)` where `running` | Finding expired leases |

All three are **partial** (`WHERE status = …`): finished jobs pile up, but the indexes only
contain rows that are still in play, so they stay small.

**How job and track statuses line up:**

| `jobs.status` | `tracks.status` |
| --- | --- |
| `queued` or `running` | `pending` |
| `done` | `ready` |
| `failed` | `failed` |

#### How the queue is used

**Enqueue:** the track row and its job are inserted **in one transaction**, so a track can't be
left `pending` with no job to finish it. (This is why the queue lives in Postgres: with Redis or
Kafka the two writes couldn't share a transaction.)

**Claim**, as one statement, committed immediately:

```sql
UPDATE jobs
SET status = 'running', attempts = attempts + 1,
    locked_until = now() + interval '5 minutes', updated_at = now()
WHERE id = (
    SELECT id FROM jobs
    WHERE (status = 'queued'  AND run_at <= now())
       OR (status = 'running' AND locked_until < now())   -- lease expired: worker died
    ORDER BY run_at
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING id, track_id, attempts, max_attempts;
```

`FOR UPDATE` locks the chosen row; `SKIP LOCKED` makes other workers pass over it instead of
waiting. The work then happens **outside** any transaction, protected by the lease, so no
transaction or pooled connection is held open for the length of a job.

**Finish**, only if this worker still owns the job:

```sql
UPDATE jobs SET status = 'done', locked_until = NULL, updated_at = now()
WHERE id = $1 AND attempts = $2   -- $2 = the attempt number this worker claimed
```

If the lease expired and another worker reclaimed the job, `attempts` has moved on, the update
matches 0 rows, and the slow worker knows it lost the job. `attempts` acts as a **fencing token**.

**Retry** (a transient error, like storage being down): `status = 'queued'`,
`run_at = now() + delay` with exponential backoff (30 s, 1 min, 2 min, 4 min, capped at 1 h)
plus up to 20% random jitter, so jobs that failed together don't all retry at the same instant.

**Fail** (a permanent error, like a corrupt file, or the last attempt): `status = 'failed'` and
the track's status to `failed`. Both keep a cleaned `last_error`: at most 1000 bytes, invalid
UTF-8 replaced and NUL bytes removed, because Postgres `text` rejects both and the update would
fail, leaving the job stuck.

Complete, Retry and Fail are all fenced (`WHERE id = $1 AND attempts = $2 AND status = 'running'`)
and update the job and its track in one transaction, so a stale worker changes neither.

**Reclaimed past the limit:** the claim query also picks up expired `running` jobs without
checking `attempts`. If a worker crashed during the last attempt, the reclaim makes `attempts`
exceed `max_attempts`; the worker then marks the job `failed` without running it
("lease expired during the last attempt"). Otherwise a file that crashes every worker would be
retried forever. Implemented in `internal/jobs`.

**Delivery is at-least-once.** A worker can finish the work and crash before marking the job
done; the lease expires and the job runs again. So every job must be **idempotent**: running it
twice must give the same result as running it once. Ingest is: today it only reads and verifies;
from Step 5 it sets the same tag values and writes the cover to the same key (`covers/<sha256>`).

### `schema_migrations`

Created by the migration runner. One row per applied migration file.

| Column | Type |
| --- | --- |
| `name` | `text` primary key (the file name, for example `0001_tracks.sql`) |
| `applied_at` | `timestamptz` |

## Migrations

Files in `internal/db/migrations/`, named `NNNN_description.sql` and applied in filename order.
Zero-padding keeps the order right (`0010` sorts after `0009`).

At startup, `db.Migrate` (`internal/db/migrate.go`):

1. Takes one dedicated connection from the pool (advisory locks belong to a connection).
2. Takes `pg_advisory_lock(72707369)`, so only one API instance migrates at a time; others wait.
3. Creates `schema_migrations` if it's missing (after the lock, to avoid a creation race).
4. For each file not yet recorded: runs it **and** records it in one transaction, then commits.
5. Releases the lock (also released automatically if the process dies, since the connection closes).

The SQL files are embedded in the binary with `//go:embed`.

### Rules

- **Never edit a migration that has run anywhere.** Its name is already recorded, so the change
  would never be applied there. Write a new migration instead.
- **Don't use `IF NOT EXISTS` in migrations.** If the object already exists, the database is in an
  unexpected state, and the migration should fail loudly.
- A failed migration leaves nothing behind (Postgres DDL is transactional) and isn't recorded,
  so it's retried on the next start.
- Multi-statement files work because `Exec` without arguments uses Postgres's simple protocol.
  Don't add arguments to that call.

## Useful commands

```sh
docker compose up -d --wait                       # start and wait until healthy
docker compose exec postgres psql -U musicbox -d musicbox   # interactive SQL shell
docker compose exec postgres psql -U musicbox -d musicbox -c '\d tracks'
docker compose exec postgres psql -U musicbox -d musicbox -c 'SELECT * FROM schema_migrations;'
docker compose stop postgres                      # stop (data kept)
```

Postgres doesn't start on boot. Run `docker compose up -d --wait` after a reboot.
