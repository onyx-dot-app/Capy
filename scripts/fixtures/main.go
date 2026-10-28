// Command fixtures speaks the E2E scripts with macOS's voices into the
// 16 kHz mono PCM16 WAV files of e2e/testdata, with a pause between turns so
// Scribe ends each one. Run it on a Mac from the repository root:
//
//	go run ./scripts/fixtures
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

const (
	english = "Flo (English (US))"
	pause   = 1500 // ms of silence after each turn
)

type turn struct{ voice, text string }

var fixtures = map[string][]turn{
	"captions-en.wav": {
		{english, "Welcome to the Capy demo."},
		{english, "Zoom Scribe turns this speech into text as I talk."},
		{english, "Then Translator and Summarizer do the rest."},
	},
}

func main() {
	tmp, err := os.MkdirTemp("", "capy-fixtures-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	for name, turns := range fixtures {
		var pcm []byte
		for i, t := range turns {
			pcm = append(pcm, speak(tmp, i, t)...)
			pcm = append(pcm, make([]byte, 16*2*pause)...)
		}
		out := filepath.Join("e2e", "testdata", name)
		if err := os.WriteFile(out, wav(pcm), 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("wrote %s (%.1f s)\n", out, float64(len(pcm))/32000)
	}
}

// speak returns the PCM16 of one turn.
func speak(dir string, i int, t turn) []byte {
	aiff := filepath.Join(dir, fmt.Sprint(i, ".aiff"))
	out := filepath.Join(dir, fmt.Sprint(i, ".wav"))
	for _, cmd := range [][]string{
		{"say", "-v", t.voice, "-o", aiff, t.text},
		{"afconvert", "-f", "WAVE", "-d", "LEI16@16000", "-c", "1", aiff, out},
	} {
		if b, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput(); err != nil {
			log.Fatalf("%v: %v\n%s", cmd, err, b)
		}
	}
	b, err := os.ReadFile(out)
	if err != nil {
		log.Fatal(err)
	}
	return data(b)
}

// data returns the samples of a WAV file's data chunk.
func data(b []byte) []byte {
	for p := 12; p+8 <= len(b); {
		size := int(binary.LittleEndian.Uint32(b[p+4:]))
		if string(b[p:p+4]) == "data" {
			return b[p+8 : p+8+size]
		}
		p += 8 + size + size&1
	}
	log.Fatal("no data chunk")
	return nil
}

func wav(pcm []byte) []byte {
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(16000), uint32(32000), uint16(2), uint16(16)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}
