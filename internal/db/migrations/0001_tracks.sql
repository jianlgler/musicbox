-- One row per audio file. Objects in storage are named by sha256,
-- the UNIQUE constraint is what makes uploads and imports idempotent.
-- IF NOT EXISTS is skipped because during a migration the table can be in an unexpected state
CREATE TABLE tracks (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    sha256      text        NOT NULL UNIQUE CHECK (sha256 ~ '^[0-9a-f]{64}$'),  
    size_bytes  bigint      NOT NULL CHECK (size_bytes > 0),
    status      text        NOT NULL DEFAULT 'pending'
                            CHECK (status IN ('pending', 'ready', 'failed')),
    title       text,
    artist      text,
    album       text,
    created_at  timestamptz NOT NULL DEFAULT now()
);
