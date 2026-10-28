-- The x-zm-trackingid Zoom answered a file's transcription with, which
-- its logs know the request by; '' for none and for a recording.
ALTER TABLE sessions ADD COLUMN tracking_id TEXT NOT NULL DEFAULT '';
