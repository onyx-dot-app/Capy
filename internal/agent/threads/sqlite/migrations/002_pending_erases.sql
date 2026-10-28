-- Keep only the metadata needed to authorize a retry until the deleted
-- content has been removed from the write-ahead log.
CREATE TABLE pending_thread_erases (
    id         TEXT PRIMARY KEY,
    capability TEXT NOT NULL,
    session_id TEXT
);
