-- A rename, kept apart from the recap's title so that a new recap keeps it.
ALTER TABLE sessions ADD COLUMN custom_title TEXT NOT NULL DEFAULT '';  -- '': the recap's title
-- When the session moved to Recently Deleted; NULL while it is not there.
ALTER TABLE sessions ADD COLUMN trashed_at   INTEGER;
-- When the engine last reported the session still recording.
ALTER TABLE sessions ADD COLUMN heartbeat_at INTEGER;

-- Every rename, recap edit and trash, with the value it replaced, to undo.
CREATE TABLE session_edits (
    id         TEXT    PRIMARY KEY,
    session_id TEXT    NOT NULL REFERENCES sessions (id),
    field      TEXT    NOT NULL,   -- 'title' | 'recap' | 'trash'
    old_value  TEXT    NOT NULL,
    new_value  TEXT    NOT NULL,
    actor      TEXT    NOT NULL,   -- who: 'user', or the caller's name
    origin     TEXT    NOT NULL DEFAULT '',  -- the caller's own tag, such as a run id
    created_at INTEGER NOT NULL,
    undone_at  INTEGER
);

CREATE INDEX session_edits_session ON session_edits (session_id);
CREATE INDEX session_edits_origin ON session_edits (origin);
CREATE INDEX sessions_trashed ON sessions (trashed_at) WHERE trashed_at IS NOT NULL AND deleted_at IS NULL;
