// Package audio captures speech as the stream Scribe Live expects: 16 kHz
// mono little-endian PCM16 in frames of about 100 ms.
package audio

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"sync"
)

const (
	// SampleRate is the rate Scribe Live expects.
	SampleRate = 16000
	// FrameBytes is 100 ms of 16 kHz mono PCM16.
	FrameBytes = SampleRate / 10 * 2
)

// Source produces PCM16 frames until ctx is canceled or the source ends.
// The channel is closed when capture stops; Err then reports why, or nil
// after cancellation or the natural end of a file.
type Source interface {
	Start(ctx context.Context) (<-chan []byte, error)
	Err() error
}

// ErrUnsupported is returned by native sources on platforms other than macOS.
var ErrUnsupported = errors.New("audio: capture is only supported on macOS")

// ErrNoMicrophone is returned by Mic when the Mac has no audio input.
var ErrNoMicrophone = errors.New("audio: no microphone is available")

// PermissionError reports that macOS denied a capture permission. Pane is
// the x-apple.systempreferences URL of the System Settings pane that grants it.
type PermissionError struct {
	What string
	Pane string
}

func (e *PermissionError) Error() string {
	return "audio: " + e.What + " permission denied"
}

// Settings panes for the two capture permissions.
const (
	MicrophonePane = "x-apple.systempreferences:com.apple.preference.security?Privacy_Microphone"
	// SystemAudioPane is Screen & System Audio Recording, whose System
	// Audio Recording Only list grants it.
	SystemAudioPane = "x-apple.systempreferences:com.apple.preference.security?Privacy_ScreenCapture"
)

// sinkBuffer holds 6.4 s of frames: capture callbacks run on real-time
// threads and must never wait for the consumer, so frames beyond it are dropped.
const sinkBuffer = 64

// sink turns capture callbacks into the frame channel of a Source. Its
// methods are safe from any thread, including after close.
type sink struct {
	mu      sync.Mutex
	ch      chan []byte
	f       framer
	closed  bool
	err     error
	dropped int
}

func newSink() *sink { return &sink{ch: make(chan []byte, sinkBuffer)} }

// write frames pcm and queues every complete frame without blocking.
func (s *sink) write(pcm []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	for _, frame := range s.f.push(pcm) {
		select {
		case s.ch <- frame:
		default:
			s.dropped++
		}
	}
}

// fail records the first error that ends or degrades capture.
func (s *sink) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

// close ends the channel once; later writes are ignored.
func (s *sink) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
}

func (s *sink) errOrNil() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// framer cuts arbitrary chunks of PCM16 into FrameBytes frames.
type framer struct {
	buf []byte
}

// push appends pcm and returns every complete frame, each a fresh slice.
func (f *framer) push(pcm []byte) [][]byte {
	f.buf = append(f.buf, pcm...)
	var frames [][]byte
	for len(f.buf) >= FrameBytes {
		frames = append(frames, append([]byte(nil), f.buf[:FrameBytes]...))
		f.buf = f.buf[FrameBytes:]
	}
	if len(f.buf) == 0 {
		f.buf = nil
	}
	return frames
}

// flush returns the incomplete remainder, if any, and empties the framer.
func (f *framer) flush() []byte {
	rest := f.buf
	f.buf = nil
	return rest
}

// float32ToPCM16 converts samples in [-1, 1] to little-endian PCM16,
// clipping out-of-range values.
func float32ToPCM16(samples []float32) []byte {
	out := make([]byte, len(samples)*2)
	for i, s := range samples {
		v := float64(s) * math.MaxInt16
		v = max(min(v, math.MaxInt16), math.MinInt16)
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(math.Round(v))))
	}
	return out
}
