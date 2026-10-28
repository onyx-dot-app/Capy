-- The upload a file session came in with others, a UUID; '' for none.
ALTER TABLE sessions ADD COLUMN batch_id TEXT NOT NULL DEFAULT '';
