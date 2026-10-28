-- Once erasure starts, restoring would expose a partially erased session.
-- Keep this claim until the hooks, library erase and WAL checkpoint succeed.
ALTER TABLE sessions ADD COLUMN purge_pending INTEGER NOT NULL DEFAULT 0;
