-- The name of the audio or video file a session transcribed, without its
-- folders; '' for a recording.
ALTER TABLE sessions ADD COLUMN file_name TEXT NOT NULL DEFAULT '';
