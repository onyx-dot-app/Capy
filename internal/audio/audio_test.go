package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFramerEmitsWholeFramesAndKeepsRemainder(t *testing.T) {
	var f framer
	pcm := make([]byte, FrameBytes*2+100)
	for i := range pcm {
		pcm[i] = byte(i)
	}
	frames := f.push(pcm[:FrameBytes-1])
	if len(frames) != 0 {
		t.Fatalf("partial frame emitted: %d frames", len(frames))
	}
	frames = f.push(pcm[FrameBytes-1:])
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	got := append(append(frames[0], frames[1]...), f.flush()...)
	if !bytes.Equal(got, pcm) {
		t.Fatal("frames and remainder do not reassemble the input")
	}
	// Frames must not alias the framer's buffer, which later pushes reuse.
	before := append([]byte(nil), frames[0]...)
	f.push(make([]byte, FrameBytes))
	if !bytes.Equal(frames[0], before) {
		t.Fatal("emitted frame changed after a later push")
	}
}

func TestFloat32ToPCM16ClipsAndRounds(t *testing.T) {
	got := float32ToPCM16([]float32{0, 1, -1, 2, -2, 0.5})
	want := []int16{0, 32767, -32767, 32767, -32768, 16384}
	for i, w := range want {
		if v := int16(binary.LittleEndian.Uint16(got[i*2:])); v != w {
			t.Errorf("sample %d = %d, want %d", i, v, w)
		}
	}
}

func TestSinkDropsWhenFullAndIgnoresWritesAfterClose(t *testing.T) {
	s := newSink()
	for range sinkBuffer + 5 {
		s.write(make([]byte, FrameBytes)) // must never block
	}
	if s.dropped != 5 {
		t.Fatalf("dropped = %d, want 5", s.dropped)
	}
	s.close()
	s.close()
	s.write(make([]byte, FrameBytes)) // must not panic on the closed channel
	n := 0
	for range s.ch {
		n++
	}
	if n != sinkBuffer {
		t.Fatalf("drained %d frames, want %d", n, sinkBuffer)
	}
}

// wav builds a RIFF/WAVE file with an optional chunk before the data.
func wav(channels uint16, rate uint32, bits uint16, extra []byte, pcm []byte) []byte {
	var b bytes.Buffer
	chunk := func(id string, body []byte) {
		b.WriteString(id)
		_ = binary.Write(&b, binary.LittleEndian, uint32(len(body)))
		b.Write(body)
		if len(body)%2 == 1 {
			b.WriteByte(0)
		}
	}
	b.WriteString("RIFF\x00\x00\x00\x00WAVE")
	fmtBody := make([]byte, 16)
	binary.LittleEndian.PutUint16(fmtBody[0:], 1)
	binary.LittleEndian.PutUint16(fmtBody[2:], channels)
	binary.LittleEndian.PutUint32(fmtBody[4:], rate)
	binary.LittleEndian.PutUint16(fmtBody[14:], bits)
	chunk("fmt ", fmtBody)
	if extra != nil {
		chunk("LIST", extra)
	}
	chunk("data", pcm)
	return b.Bytes()
}

func TestParseWAV(t *testing.T) {
	pcm := []byte{1, 2, 3, 4, 5, 6}
	tests := []struct {
		name    string
		file    []byte
		want    []byte
		wantErr string
	}{
		{"plain", wav(1, 16000, 16, nil, pcm), pcm, ""},
		{"odd-sized chunk before data is padded", wav(1, 16000, 16, []byte{9, 9, 9}, pcm), pcm, ""},
		{"stereo", wav(2, 16000, 16, nil, pcm), nil, "want 16 kHz mono"},
		{"44.1 kHz", wav(1, 44100, 16, nil, pcm), nil, "want 16 kHz mono"},
		{"8-bit", wav(1, 16000, 8, nil, pcm), nil, "want 16 kHz mono"},
		{"not wav", []byte("hello world, not audio"), nil, "not a RIFF"},
		{"truncated data", wav(1, 16000, 16, nil, pcm)[:46], nil, "overruns"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseWAV(tt.file)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || !bytes.Equal(got, tt.want) {
				t.Fatalf("got %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

func TestWAVReplaysEveryByteInFrames(t *testing.T) {
	pcm := make([]byte, FrameBytes*3+10)
	for i := range pcm {
		pcm[i] = byte(i * 7)
	}
	path := filepath.Join(t.TempDir(), "speech.wav")
	if err := os.WriteFile(path, wav(1, 16000, 16, nil, pcm), 0o600); err != nil {
		t.Fatal(err)
	}
	ch, err := NewWAV(path, false).Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	var sizes []int
	for f := range ch {
		got = append(got, f...)
		sizes = append(sizes, len(f))
	}
	if !bytes.Equal(got, pcm) {
		t.Fatal("replayed audio differs from the file")
	}
	if len(sizes) != 4 || sizes[3] != 10 {
		t.Fatalf("frame sizes %v, want three full frames and a 10-byte tail", sizes)
	}
}

func TestWAVStopsOnCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "speech.wav")
	if err := os.WriteFile(path, wav(1, 16000, 16, nil, make([]byte, FrameBytes*50)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := NewWAV(path, true).Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	<-ch
	cancel()
	n := 0
	for range ch {
		n++
	}
	if n > 1 {
		t.Fatalf("%d frames after cancel, want at most 1 in flight", n)
	}
}

// fakeSource replays frames, then holds its channel open until ctx ends
// unless ends is set.
type fakeSource struct {
	frames [][]byte
	ends   bool
	err    error // returned by Start
	ctx    context.Context
}

func (f *fakeSource) Start(ctx context.Context) (<-chan []byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.ctx = ctx
	ch := make(chan []byte, len(f.frames))
	for _, fr := range f.frames {
		ch <- fr
	}
	if f.ends {
		close(ch)
	} else {
		context.AfterFunc(ctx, func() { close(ch) })
	}
	return ch, nil
}

func (f *fakeSource) Err() error { return nil }

// level is a frame whose samples all equal v.
func level(v int16) []byte {
	f := make([]byte, FrameBytes)
	for i := 0; i < len(f); i += 2 {
		binary.LittleEndian.PutUint16(f[i:], uint16(v))
	}
	return f
}

func levels(n int, v int16) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = level(v)
	}
	return out
}

// sampleLevels returns each frame's first sample, failing on a frame that is
// not flat at full length.
func sampleLevels(t *testing.T, ch <-chan []byte) []int16 {
	t.Helper()
	var got []int16
	for f := range ch {
		v := int16(binary.LittleEndian.Uint16(f))
		if !bytes.Equal(f, level(v)) {
			t.Fatalf("frame %d is not a whole frame of %d", len(got), v)
		}
		got = append(got, v)
	}
	return got
}

func TestSumPCMAddsAndClips(t *testing.T) {
	for _, tt := range []struct{ a, b, want int16 }{
		{1000, 500, 1500},
		{-1000, 300, -700},
		{30000, 10000, 32767},
		{-30000, -10000, -32768},
	} {
		got := sumPCM([][]byte{level(tt.a), level(tt.b)})
		if !bytes.Equal(got, level(tt.want)) {
			t.Errorf("%d + %d: got %d, want %d", tt.a, tt.b, int16(binary.LittleEndian.Uint16(got)), tt.want)
		}
	}
}

func TestMixSumsTheSourcesFrameByFrame(t *testing.T) {
	// No more than mixLag frames each: whichever source the mix hears
	// first, it waits for the other.
	system := &fakeSource{frames: levels(mixLag, 1000), ends: true}
	mic := &fakeSource{frames: levels(mixLag, 200), ends: true}
	ch, err := Mix(system, mic).Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := sampleLevels(t, ch)
	if len(got) != mixLag {
		t.Fatalf("got %d frames, want %d: %v", len(got), mixLag, got)
	}
	for i, v := range got {
		if v != 1200 {
			t.Fatalf("frame %d = %d, want 1200 (system + mic)", i, v)
		}
	}
}

// A source that stops delivering, like a tap whose output device went
// away, must not hold up the other for more than mixLag frames.
func TestMixPassesASourceThroughWhenTheOtherStalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	system := &fakeSource{} // open, silent
	mic := &fakeSource{frames: levels(mixLag+4, 300)}
	ch, err := Mix(system, mic).Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		select {
		case f := <-ch:
			if !bytes.Equal(f, level(300)) {
				t.Fatalf("frame %d is not the mic's", i)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("frame %d held back while system audio is silent", i)
		}
	}
	select {
	case <-ch:
		t.Fatalf("got a frame while the mic was only %d ahead", mixLag)
	default:
	}
	cancel()
	if got := sampleLevels(t, ch); len(got) != mixLag {
		t.Fatalf("after stop, got %d of the mic's held frames, want %d", len(got), mixLag)
	}
}

func TestMixLeavesOutAMissingMicrophone(t *testing.T) {
	system := &fakeSource{frames: levels(3, 700), ends: true}
	ch, err := Mix(system, &fakeSource{err: ErrNoMicrophone}).Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := sampleLevels(t, ch); len(got) != 3 || got[0] != 700 {
		t.Fatalf("got %v, want the system audio alone", got)
	}

	_, err = Mix(&fakeSource{err: ErrNoMicrophone}).Start(context.Background())
	if !errors.Is(err, ErrNoMicrophone) {
		t.Fatalf("err = %v, want ErrNoMicrophone with no source left", err)
	}
}

func TestMixStopsStartedSourcesWhenOneFails(t *testing.T) {
	system := &fakeSource{}
	denied := &PermissionError{What: "microphone", Pane: MicrophonePane}
	_, err := Mix(system, &fakeSource{err: denied}).Start(context.Background())
	if !errors.Is(err, denied) {
		t.Fatalf("err = %v, want the microphone permission error", err)
	}
	if system.ctx.Err() == nil {
		t.Fatal("system audio still capturing after the mix failed to start")
	}
}
