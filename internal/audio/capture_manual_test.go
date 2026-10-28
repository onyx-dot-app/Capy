//go:build darwin && manual

package audio

import (
	"context"
	"encoding/binary"
	"os/exec"
	"testing"
	"time"
)

// Manual capture check (needs a Mac with a microphone and the permissions):
//
//	CGO_ENABLED=0 go test -tags manual -run TestCapture -v ./internal/audio/
//
// It returns the loudest sample captured in 3 s.
func captureFor(t *testing.T, s Source) int {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch, err := s.Start(ctx)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	frames, peak := 0, 0
	for f := range ch {
		if len(f) != FrameBytes {
			t.Fatalf("frame of %d bytes, want %d", len(f), FrameBytes)
		}
		for i := 0; i < len(f); i += 2 {
			peak = max(peak, abs(int(int16(binary.LittleEndian.Uint16(f[i:])))))
		}
		frames++
	}
	if err := s.Err(); err != nil {
		t.Fatalf("capture: %v", err)
	}
	// 3 s is 30 frames; allow for start-up latency.
	if frames < 20 {
		t.Fatalf("got %d frames in 3 s, want about 30", frames)
	}
	return peak
}

func abs(v int) int { return max(v, -v) }

func TestCaptureMic(t *testing.T) { captureFor(t, NewMic()) }

// A tap without permission delivers silence rather than an error, so the
// check plays a sound and expects to hear it.
func TestCaptureSystem(t *testing.T) { hearSound(t, NewSystem()) }

// This Mac: the mixed capture must still hear what the Mac plays.
func TestCaptureThisMac(t *testing.T) { hearSound(t, Mix(NewSystem(), NewMic())) }

func hearSound(t *testing.T, s Source) {
	play := exec.Command("afplay", "/System/Library/Sounds/Submarine.aiff")
	time.AfterFunc(500*time.Millisecond, func() { _ = play.Start() })
	peak := captureFor(t, s)
	_ = play.Wait()
	if peak < 1000 {
		t.Fatalf("loudest sample %d while a sound played: the tap heard silence (permission?)", peak)
	}
	t.Logf("loudest sample %d", peak)
}
