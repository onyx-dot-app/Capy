-- An import of audio or video files, each transcribed into a session of
-- its own: its name, and the languages of every file of it.
CREATE TABLE batches (
    id         TEXT    PRIMARY KEY,
    name       TEXT    NOT NULL,
    lang       TEXT    NOT NULL,             -- spoken in its files
    target     TEXT    NOT NULL DEFAULT '',  -- translated to; '' for none
    created_at INTEGER NOT NULL
);

-- The files of a batch, in the order chosen. Until a file is saved as a
-- session, its path is kept to transcribe it, after a restart too.
CREATE TABLE batch_files (
    id          TEXT    PRIMARY KEY,
    batch_id    TEXT    NOT NULL REFERENCES batches (id),
    seq         INTEGER NOT NULL,
    path        TEXT    NOT NULL,             -- '' once saved
    name        TEXT    NOT NULL,
    size        INTEGER NOT NULL,
    duration_ms INTEGER NOT NULL,             -- from its metadata; 0: unknown
    recorded_at INTEGER,                      -- from its metadata; NULL: unknown
    state       TEXT    NOT NULL,             -- 'waiting' | 'saved' | 'failed' | 'canceled'
    problem     TEXT    NOT NULL DEFAULT '',  -- why a failed file was not transcribed
    title       TEXT    NOT NULL DEFAULT '',  -- a rename before it was saved
    session_id  TEXT    NOT NULL DEFAULT '',  -- once saved
    UNIQUE (batch_id, seq)
);

CREATE INDEX batch_files_session ON batch_files (session_id) WHERE session_id <> '';

-- Uploads before batches were kept named no batch: their sessions stand on
-- their own.
UPDATE sessions SET batch_id = '' WHERE batch_id <> '';
