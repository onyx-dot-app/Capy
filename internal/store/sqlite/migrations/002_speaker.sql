-- Scribe's label of who said a segment ("Speaker 1"); empty when it gave none.
ALTER TABLE segments ADD COLUMN speaker TEXT NOT NULL DEFAULT '';
