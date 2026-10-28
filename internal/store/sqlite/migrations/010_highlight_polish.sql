-- What the user's agent wrote of a highlight, as JSON ('' for none), and
-- when; a debate is a highlight of its own, its polish always set.
ALTER TABLE highlights ADD COLUMN polish      TEXT    NOT NULL DEFAULT '';
ALTER TABLE highlights ADD COLUMN polished_at INTEGER;
