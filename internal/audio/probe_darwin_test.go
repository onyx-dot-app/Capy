package audio

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pcmWAV is a RIFF/WAVE file of seconds of 16 kHz mono PCM16 silence.
func pcmWAV(seconds float64) []byte {
	data := make([]byte, int(seconds*SampleRate)*2)
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(data)))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(SampleRate), uint32(SampleRate * 2), uint16(2), uint16(16)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

func TestProbeReadsTheDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Weekly sync.wav")
	if err := os.WriteFile(path, pcmWAV(2.5), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := Probe(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Duration != 2500*time.Millisecond {
		t.Fatalf("Duration = %v, want 2.5s", info.Duration)
	}
	if !info.RecordedAt.IsZero() {
		t.Fatalf("RecordedAt = %v; a WAV file says none", info.RecordedAt)
	}
}

func TestProbeSaysNothingOfAFileItCannotRead(t *testing.T) {
	dir := t.TempDir()
	notAudio := filepath.Join(dir, "notes.m4a")
	if err := os.WriteFile(notAudio, []byte("not audio at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{notAudio, filepath.Join(dir, "missing.mp3")} {
		info, err := Probe(path)
		if err != nil || info != (Info{}) {
			t.Errorf("Probe(%s) = %+v, %v; want nothing, no error", filepath.Base(path), info, err)
		}
	}
}
