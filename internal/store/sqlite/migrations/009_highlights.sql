-- The turns Highlights marked, by the segment's seq: a kind and how sure
-- Decisions was, no text. One per turn; a dismissed one stays, so the
-- turn is not marked again.
CREATE TABLE highlights (
    id         TEXT    PRIMARY KEY,
    session_id TEXT    NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    kind       TEXT    NOT NULL,
    confidence REAL    NOT NULL,
    dismissed  INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    UNIQUE (session_id, seq)
);
