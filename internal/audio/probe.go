package audio

import "time"

// Info is what a media file's metadata says of it; a zero field is what
// it does not say.
type Info struct {
	// Duration is how long its audio lasts.
	Duration time.Duration
	// RecordedAt is when it was recorded, as its creation date says.
	RecordedAt time.Time
}
