package audio

import (
	"math"
	"time"

	"github.com/ebitengine/purego/objc"
)

// cmTime mirrors Core Media's CMTime.
type cmTime struct {
	Value     int64
	Timescale int32
	Flags     uint32
	Epoch     int64
}

// kCMTimeFlags_Valid, and the flags of a time that is no number of seconds.
const (
	cmTimeValid      = 1 << 0
	cmTimeNotNumeric = 1<<2 | 1<<3 | 1<<4 // positive and negative infinity, indefinite
)

// seconds is t in seconds; ok is false for a time that is none.
func (t cmTime) seconds() (float64, bool) {
	if t.Flags&cmTimeValid == 0 || t.Flags&cmTimeNotNumeric != 0 || t.Timescale <= 0 || t.Value < 0 {
		return 0, false
	}
	return float64(t.Value) / float64(t.Timescale), true
}

// Probe reads the duration and the creation date of the media file at path
// with AVFoundation, which reads WAV, MP3, AAC, M4A, MP4, MOV and FLAC: Info
// is zero for a file it cannot read, such as Ogg or WebM. It reads the file,
// so call it off the main thread.
func Probe(path string) (Info, error) {
	if err := load(); err != nil {
		return Info{}, err
	}
	var info Info
	withPool(func() {
		url := send(objc.ID(objc.GetClass("NSURL")), "fileURLWithPath:", nsString(path))
		if url == 0 {
			return
		}
		asset := send(objc.ID(objc.GetClass("AVURLAsset")), "URLAssetWithURL:options:", url, objc.ID(0))
		if asset == 0 {
			return
		}
		if s, ok := objc.Send[cmTime](asset, sel("duration")).seconds(); ok && s < math.MaxInt64/float64(time.Second) {
			info.Duration = time.Duration(s * float64(time.Second)).Round(time.Millisecond)
		}
		item := send(asset, "creationDate")
		if item == 0 {
			return
		}
		date := send(item, "dateValue")
		if date == 0 {
			return
		}
		if unix := objc.Send[float64](date, sel("timeIntervalSince1970")); unix > 0 {
			info.RecordedAt = time.UnixMilli(int64(unix * 1000)).UTC()
		}
	})
	return info, nil
}
