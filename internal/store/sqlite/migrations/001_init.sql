CREATE TABLE sessions (
    id            TEXT    PRIMARY KEY,
    mode          TEXT    NOT NULL,
    source        TEXT    NOT NULL,
    lang_a        TEXT    NOT NULL,
    lang_b        TEXT    NOT NULL,
    title         TEXT    NOT NULL DEFAULT '',
    recap         TEXT    NOT NULL DEFAULT '',
    recap_status  TEXT    NOT NULL DEFAULT 'pending',
    started_at    INTEGER NOT NULL,
    ended_at      INTEGER,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    deleted_at    INTEGER
);

CREATE INDEX sessions_started ON sessions (started_at DESC, id DESC) WHERE deleted_at IS NULL;

-- rid is an explicit INTEGER PRIMARY KEY because the FTS index below refers to
-- it; an implicit rowid may be renumbered by VACUUM.
CREATE TABLE segments (
    rid              INTEGER PRIMARY KEY,
    id               TEXT    NOT NULL UNIQUE,
    session_id       TEXT    NOT NULL REFERENCES sessions (id),
    seq              INTEGER NOT NULL,
    lang             TEXT    NOT NULL,
    text             TEXT    NOT NULL,
    translation      TEXT    NOT NULL DEFAULT '',
    translation_lang TEXT    NOT NULL DEFAULT '',
    start_ms         INTEGER NOT NULL,
    end_ms           INTEGER NOT NULL,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL,
    UNIQUE (session_id, seq)
);

-- External-content FTS5 index. The trigram tokenizer matches any substring of
-- three or more characters, which also works for CJK text that has no spaces.
CREATE VIRTUAL TABLE segments_fts USING fts5 (
    text, translation,
    content = 'segments', content_rowid = 'rid',
    tokenize = 'trigram'
);
-- Remove deleted entries from the index at once instead of at the next merge,
-- so deleted transcripts cannot be rebuilt from it.
INSERT INTO segments_fts (segments_fts, rank) VALUES ('secure-delete', 1);

CREATE TRIGGER segments_ai AFTER INSERT ON segments BEGIN
    INSERT INTO segments_fts (rowid, text, translation) VALUES (new.rid, new.text, new.translation);
END;

CREATE TRIGGER segments_ad AFTER DELETE ON segments BEGIN
    INSERT INTO segments_fts (segments_fts, rowid, text, translation) VALUES ('delete', old.rid, old.text, old.translation);
END;

CREATE TRIGGER segments_au AFTER UPDATE OF text, translation ON segments BEGIN
    INSERT INTO segments_fts (segments_fts, rowid, text, translation) VALUES ('delete', old.rid, old.text, old.translation);
    INSERT INTO segments_fts (rowid, text, translation) VALUES (new.rid, new.text, new.translation);
END;
