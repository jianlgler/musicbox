-- Work queue in Postgres: workers claim jobs with FOR UPDATE SKIP LOCKED.
-- Delivery is at-least-once (a job can run twice), so every job must be idempotent.
CREATE TABLE jobs (
    -- Internal only, never exposed to clients, so a sequential id is fine (unlike tracks.id)
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind          text        NOT NULL CHECK (kind IN ('ingest_track')),
    -- Deleting a track deletes its jobs
    track_id      uuid        NOT NULL REFERENCES tracks (id) ON DELETE CASCADE,
    status        text        NOT NULL DEFAULT 'queued'
                              CHECK (status IN ('queued', 'running', 'done', 'failed')),
    attempts      integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts  integer     NOT NULL DEFAULT 5 CHECK (max_attempts > 0),
    -- Not claimable before this time: retries back off by moving it into the future
    run_at        timestamptz NOT NULL DEFAULT now(),
    -- Lease: set while running; once it's in the past, the job can be reclaimed
    locked_until  timestamptz,
    -- Truncated by the worker. May contain text from the uploaded file: never render it as HTML
    last_error    text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    -- A running job must have a lease
    CHECK (status <> 'running' OR locked_until IS NOT NULL)
);

-- At most one active ingest job per track: uploading the same file again can't enqueue duplicate work
CREATE UNIQUE INDEX jobs_one_active_per_track
    ON jobs (kind, track_id)
    WHERE status IN ('queued', 'running');

-- What the claim query searches: only rows that could be picked up
CREATE INDEX jobs_queued_run_at   ON jobs (run_at)       WHERE status = 'queued';
CREATE INDEX jobs_running_lease   ON jobs (locked_until) WHERE status = 'running';
