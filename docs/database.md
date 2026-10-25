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
