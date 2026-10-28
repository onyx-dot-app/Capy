package features

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "features.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLocalWithoutAFileTurnsAgentAndHighlightsOn(t *testing.T) {
	g, err := Local(filepath.Join(t.TempDir(), "features.json"))
	if err != nil || !g.Enabled(Agent) || !g.Enabled(Highlights) {
		t.Fatalf("no file: agent %v, highlights %v, err %v; want both on, no error", g.Enabled(Agent), g.Enabled(Highlights), err)
	}
}

func TestLocalFileTurnsAgentOff(t *testing.T) {
	g, err := Local(write(t, `{"agent": false, "later": true}`))
	if err != nil || g.Enabled(Agent) {
		t.Fatalf("agent %v, err %v; want off, no error", g.Enabled(Agent), err)
	}
	// The file shows only what this build has.
	if g.Enabled("later") {
		t.Error("an unknown feature from the file is on")
	}
}

func TestLocalMalformedFileKeepsTheDefaults(t *testing.T) {
	for _, body := range []string{`{"agent": "no"}`, `[false]`, `secret transcript`, `null`} {
		g, err := Local(write(t, body))
		if !errors.Is(err, ErrMalformed) || !g.Enabled(Agent) {
			t.Errorf("%s: agent %v, err %v; want the defaults and ErrMalformed", body, g.Enabled(Agent), err)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("the error repeats the file: %v", err)
		}
	}
}

func TestLocalUnreadableFileKeepsTheDefaults(t *testing.T) {
	dir := t.TempDir() // a directory where the file should be
	g, err := Local(dir)
	if err == nil || !g.Enabled(Agent) || strings.Contains(err.Error(), dir) {
		t.Fatalf("agent %v, err %v; want the defaults and the cause only", g.Enabled(Agent), err)
	}
}
