//go:build e2e

// Package e2e drives the capy binary through its replay entry point (a WAV
// file in) and its upload entry point (audio files in), a JSON report out
// at e2e/out/<case>.json. Against the fake Zoom
// server the report must equal the golden file; against the real API
// (CAPY_E2E_LIVE=1 and ZOOM_API_KEY) every check in it must pass.
//
//	go test -tags e2e ./e2e/                       # fake upstream, golden reports
//	go test -tags e2e ./e2e/ -update               # rewrite the golden reports
//	CAPY_E2E_LIVE=1 go test -tags e2e -run Live ./e2e/
package e2e

import (
	"bytes"
	"errors"
	"flag"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/onyx-dot-app/Capy/internal/audio"
	"github.com/onyx-dot-app/Capy/internal/fakezoom"
)

var update = flag.Bool("update", false, "rewrite the golden reports")

var (
	capy      string // the built binary
	fakeAgent string // fakeagent, built as claude
)

func TestMain(m *testing.M) {
	flag.Parse()
	dir, err := os.MkdirTemp("", "capy-e2e-")
	if err != nil {
		panic(err)
	}
	capy = filepath.Join(dir, "capy")
	fakeAgent = filepath.Join(dir, "bin", "claude")
	for out, pkg := range map[string]string{capy: "..", fakeAgent: "../internal/agent/gateway/fake/cmd/fakeagent"} {
		build := exec.Command("go", "build", "-o", out, pkg)
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := build.CombinedOutput(); err != nil {
			panic(string(out))
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type replayCase struct {
	name, wav, langA, langB string
	turns                   []string // what the fake recognizes, in order
	// decisions are what the fake's Decisions finds; args capy's own.
	decisions []fakezoom.Decision
	args      []string
}

var cases = []replayCase{
	{
		name: "captions", wav: "captions-en.wav", langA: "en-US", langB: "zh-CN",
		turns: []string{
			"Welcome to the Capy demo.",
			"Zoom Scribe turns this speech into text as I talk.",
			"Then Translator and Summarizer do the rest.",
		},
	},
	{
		// A name, a decision and a risk, marked at Stop.
		name: "highlights", wav: "captions-en.wav", langA: "en-US", langB: "zh-CN",
		turns: []string{
			"Okay, annual goes to forty-nine, monthly stays at five.",
			"Alex, can you update the pricing slide before Thursday?",
			"The EU residency review isn't done, and that could block the launch there.",
		},
		decisions: []fakezoom.Decision{
			{Kind: "decision", Text: "annual goes to forty-nine", P: 0.93},
			{Kind: "asked_you", Text: "update the pricing slide", P: 0.95},
			{Kind: "heads_up", Text: "could block the launch", P: 0.9},
		},
		args: []string{"-highlights", "-listener", "Alex Rivera", "-also-called", "Alex"},
	},
}

// replay runs capy on c, with extra arguments, and returns its exit error
// and the report.
func replay(t *testing.T, c replayCase, out string, realtime bool, env []string, extra ...string) ([]byte, error) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"-replay", filepath.Join("testdata", c.wav),
		"-lang-a", c.langA, "-lang-b", c.langB, "-translate", "-report", out,
	}
	if !realtime {
		args = append(args, "-realtime=false")
	}
	args = append(args, c.args...)
	cmd := exec.Command(capy, append(args, extra...)...)
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = os.Stdout, &stderr
	runErr := cmd.Run()
	if runErr != nil {
		t.Logf("capy stderr:\n%s", stderr.String())
	}
	report, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("no report: %v (capy: %v)", err, runErr)
	}
	return report, runErr
}

// fakeZoom serves c's turns as the Zoom API would, with the key the
// replay is given in env.
func fakeZoom(t *testing.T, c replayCase) (env []string) {
	t.Helper()
	turns := make([]fakezoom.Turn, len(c.turns))
	for i, text := range c.turns {
		turns[i] = fakezoom.Turn{Text: text, AfterBytes: (i + 1) * 30 * audio.FrameBytes}
	}
	fake := fakezoom.New(fakezoom.Script{APIKey: "e2e-key", Turns: turns, Decisions: c.decisions})
	t.Cleanup(fake.Close)
	return []string{"ZOOM_API_KEY=e2e-key", "CAPY_API_BASE_URL=" + fake.URL}
}

// matchGolden fails unless report equals testdata/<name>.golden.json,
// which -update rewrites.
func matchGolden(t *testing.T, name string, report []byte) {
	t.Helper()
	golden := filepath.Join("testdata", name+".golden.json")
	if *update {
		if err := os.WriteFile(golden, report, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if !bytes.Equal(report, want) {
		t.Fatalf("report differs from %s:\n%s", golden, report)
	}
}

func TestFakeReplay(t *testing.T) {
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			report, err := replay(t, c, filepath.Join("out", "fake-"+c.name+".json"), false, fakeZoom(t, c))
			if err != nil {
				t.Fatalf("capy failed: %v\n%s", err, report)
			}
			matchGolden(t, "fake-"+c.name, report)
		})
	}
}

func TestLiveReplay(t *testing.T) {
	if os.Getenv("CAPY_E2E_LIVE") != "1" || os.Getenv("ZOOM_API_KEY") == "" {
		t.Skip("set CAPY_E2E_LIVE=1 and ZOOM_API_KEY to run against the Zoom API")
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			report, err := replay(t, c, filepath.Join("out", "live-"+c.name+".json"), true, nil)
			if err != nil {
				t.Fatalf("a check failed (%v):\n%s", err, report)
			}
		})
	}
}

// uploadCase is files imported together as one batch, each a copy of a
// fixture under its own name: the fake transcribes a file by its name.
type uploadCase struct {
	name  string
	files map[string]string // name → fixture
	args  []string          // -name, -translate-to
	pass  bool              // the verdict
}

var uploadCases = []uploadCase{
	{name: "upload", pass: true, args: []string{"-name", "Customer interviews"},
		files: map[string]string{"Launch rehearsal.wav": "captions-en.wav", "Standup.wav": "captions-en.wav"}},
	// Named as the app names it; each transcript translated.
	{name: "upload-translated", pass: true, args: []string{"-translate-to", "fr-FR"},
		files: map[string]string{"interview-acme.wav": "captions-en.wav", "interview-birch.wav": "captions-en.wav"}},
	// A failed summary keeps its transcript; a failed transcription saves
	// nothing, and the file stays in the batch, failed.
	{name: "upload-failures", args: []string{"-name", "Calls"},
		files: map[string]string{"flaky call.wav": "captions-en.wav", "broken.wav": "captions-en.wav"}},
}

// upload runs capy -upload on c's files, in name order, and returns its
// exit error and the report.
func upload(t *testing.T, c uploadCase, out string, env []string) ([]byte, error) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	args := append([]string{"-upload", "-lang-a", "en-US", "-report", out}, c.args...)
	for _, name := range slices.Sorted(maps.Keys(c.files)) {
		b, err := os.ReadFile(filepath.Join("testdata", c.files[name]))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, path)
	}
	cmd := exec.Command(capy, args...)
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = os.Stdout, &stderr
	runErr := cmd.Run()
	if runErr != nil {
		t.Logf("capy stderr:\n%s", stderr.String())
	}
	report, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("no report: %v (capy: %v)", err, runErr)
	}
	return report, runErr
}

func TestFakeUpload(t *testing.T) {
	for _, c := range uploadCases {
		t.Run(c.name, func(t *testing.T) {
			fake := fakezoom.New(fakezoom.Script{APIKey: "e2e-key", FailSpeech: "broken", FailSpeechSummary: "flaky"})
			t.Cleanup(fake.Close)
			env := []string{"ZOOM_API_KEY=e2e-key", "CAPY_API_BASE_URL=" + fake.URL}
			report, err := upload(t, c, filepath.Join("out", "fake-"+c.name+".json"), env)
			if (err == nil) != c.pass {
				t.Fatalf("capy exited with %v, want the verdict pass=%v:\n%s", err, c.pass, report)
			}
			matchGolden(t, "fake-"+c.name, report)
		})
	}
}

func TestLiveUpload(t *testing.T) {
	if os.Getenv("CAPY_E2E_LIVE") != "1" || os.Getenv("ZOOM_API_KEY") == "" {
		t.Skip("set CAPY_E2E_LIVE=1 and ZOOM_API_KEY to run against the Zoom API")
	}
	c := uploadCases[0]
	report, err := upload(t, c, filepath.Join("out", "live-"+c.name+".json"), nil)
	if err != nil {
		t.Fatalf("a check failed (%v):\n%s", err, report)
	}
}

// TestFakeHighlightsEval scores the labeled meetings against a fake
// Decisions that is sure of a decision, a request, a risk and a debate,
// and wrongly of an announcement: the report scores each kind, and the wrong one
// fails its gate.
func TestFakeHighlightsEval(t *testing.T) {
	fake := fakezoom.New(fakezoom.Script{APIKey: "e2e-key", Decisions: []fakezoom.Decision{
		{Kind: "decision", Text: "annual goes to forty-nine", P: 0.93},
		{Kind: "asked_you", Text: "update the pricing slide", P: 0.96},
		{Kind: "heads_up", Text: "could block the launch", P: 0.9},
		{Kind: "debate", Text: "I don't agree", P: 0.91},
		{Kind: "announcement", Text: "keep the dry run Thursday at four", P: 0.88},
	}})
	t.Cleanup(fake.Close)
	out := filepath.Join("out", "fake-highlights-eval.json")
	if err := os.MkdirAll("out", 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(capy, "-highlights-eval", "-report", out,
		filepath.Join("..", "testdata", "eval", "launch-sync.json"), filepath.Join("..", "testdata", "eval", "quiet-standup.json"))
	cmd.Env = append(os.Environ(), "ZOOM_API_KEY=e2e-key", "CAPY_API_BASE_URL="+fake.URL)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("capy: %v, want exit 1 for a kind failing its gate\n%s", err, stderr.String())
	}
	report, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	matchGolden(t, "fake-highlights-eval", report)
}
