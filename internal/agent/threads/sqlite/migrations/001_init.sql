-- The threads of every Agent capability, each about one session of Capy's
-- library (session_id) or none (NULL: cross-session).
CREATE TABLE threads (
    id              TEXT    PRIMARY KEY,
    capability      TEXT    NOT NULL,  -- 'ask'; a later capability names its own
    session_id      TEXT,              -- a capy.db session; NULL: cross-session
    scope           TEXT    NOT NULL,  -- 'session' | 'all' | 'range:<from_ms>-<to_ms>'
    agent           TEXT    NOT NULL,  -- a gateway agent ID
    conversation_id TEXT    NOT NULL DEFAULT '',
    title           TEXT    NOT NULL DEFAULT '',  -- the first question
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE INDEX threads_session ON threads (session_id, created_at) WHERE session_id IS NOT NULL;
CREATE INDEX threads_cross ON threads (capability, created_at) WHERE session_id IS NULL;

CREATE TABLE messages (
    id         TEXT    PRIMARY KEY,
    thread_id  TEXT    NOT NULL REFERENCES threads (id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    role       TEXT    NOT NULL,  -- 'user' | 'agent' | 'edit'
    text       TEXT    NOT NULL,  -- for 'edit': the edit id
    status     TEXT    NOT NULL DEFAULT 'done',  -- 'done' | 'incomplete' | 'unanswered'
    created_at INTEGER NOT NULL,
    UNIQUE (thread_id, seq)
);
