package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// maxWAVBytes bounds replay files (about 4.6 hours of 16 kHz mono PCM16).
const maxWAVBytes = 512 << 20

// WAV replays a 16 kHz mono PCM16 WAV file as if it were being captured.
type WAV struct {
	path     string
	realtime bool
}

// NewWAV returns a Source for path. With realtime, frames are paced at
// 100 ms like a live capture; otherwise they are sent as fast as the
// consumer reads them.
func NewWAV(path string, realtime bool) *WAV {
	return &WAV{path: path, realtime: realtime}
}

// Start implements Source.
func (w *WAV) Start(ctx context.Context) (<-chan []byte, error) {
	info, err := os.Stat(w.path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxWAVBytes {
		return nil, fmt.Errorf("audio: %s is larger than %d MiB", w.path, maxWAVBytes>>20)
	}
	f, err := os.Open(w.path)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxWAVBytes+1))
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	if len(raw) > maxWAVBytes {
		return nil, fmt.Errorf("audio: %s is larger than %d MiB", w.path, maxWAVBytes>>20)
	}
	pcm, err := parseWAV(raw)
	if err != nil {
		return nil, fmt.Errorf("audio: %s: %w", w.path, err)
	}
	out := make(chan []byte)
	go func() {
		defer close(out)
		var tick <-chan time.Time
		if w.realtime {
			t := time.NewTicker(100 * time.Millisecond)
			defer t.Stop()
			tick = t.C
		}
		// Frames are views of the file's bytes, which nothing writes to.
		for off := 0; off < len(pcm); off += FrameBytes {
			frame := pcm[off:min(off+FrameBytes, len(pcm))]
			if tick != nil {
				select {
				case <-tick:
				case <-ctx.Done():
					return
				}
			}
			select {
			case out <- frame:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// Err implements Source; replay never fails after Start.
func (w *WAV) Err() error { return nil }

// parseWAV returns the PCM data of a RIFF/WAVE file that is 16 kHz, mono,
// 16-bit PCM, the only format Scribe Live accepts without conversion.
func parseWAV(b []byte) ([]byte, error) {
	if len(b) < 12 || !bytes.Equal(b[0:4], []byte("RIFF")) || !bytes.Equal(b[8:12], []byte("WAVE")) {
		return nil, errors.New("not a RIFF/WAVE file")
	}
	var haveFormat bool
	for p := 12; p+8 <= len(b); {
		id := string(b[p : p+4])
		size := int(binary.LittleEndian.Uint32(b[p+4 : p+8]))
		body := p + 8
		if body+size > len(b) {
			return nil, fmt.Errorf("chunk %q overruns the file", id)
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, errors.New("short fmt chunk")
			}
			format := binary.LittleEndian.Uint16(b[body:])
			channels := binary.LittleEndian.Uint16(b[body+2:])
			rate := binary.LittleEndian.Uint32(b[body+4:])
			bits := binary.LittleEndian.Uint16(b[body+14:])
			if format != 1 || channels != 1 || rate != SampleRate || bits != 16 {
				return nil, fmt.Errorf("want 16 kHz mono 16-bit PCM, got format %d, %d channels, %d Hz, %d bits", format, channels, rate, bits)
			}
			haveFormat = true
		case "data":
			if !haveFormat {
				return nil, errors.New("data chunk before fmt chunk")
			}
			return b[body : body+size&^1], nil
		}
		p = body + size + size&1 // chunks are padded to even sizes
	}
	return nil, errors.New("no data chunk")
}
