-- What the ingest worker reads from each file. All nullable: tags are optional, and nothing is
-- known until the worker has run. Values come from uploaded files, so the database enforces
-- ranges itself, whatever the parser returns.
ALTER TABLE tracks
    -- Groups an album's tracks even when each track has a different artist (compilations)
    ADD COLUMN album_artist text,
    ADD COLUMN track_number integer CHECK (track_number BETWEEN 1 AND 9999),
    ADD COLUMN disc_number  integer CHECK (disc_number  BETWEEN 1 AND 999),
    ADD COLUMN year         integer CHECK (year         BETWEEN 1000 AND 9999),
    ADD COLUMN duration_ms  integer CHECK (duration_ms  >= 0),
    -- Cover art lives in storage at covers/<cover_sha256>: an album's tracks usually embed
    -- the same image, so it's stored once
    ADD COLUMN cover_sha256 text    CHECK (cover_sha256 ~ '^[0-9a-f]{64}$');

-- Tag text is shown to every member, so keep it bounded at the database too
ALTER TABLE tracks
    ADD CONSTRAINT tracks_text_lengths CHECK (
        char_length(title)        <= 500 AND
        char_length(artist)       <= 500 AND
        char_length(album)        <= 500 AND
        char_length(album_artist) <= 500
    );
