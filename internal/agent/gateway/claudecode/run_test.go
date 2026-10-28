package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
)

const (
	conversation = "3a5012b2-7177-42b1-b0e6-253c56cd4ae8"
	capySession  = "11111111-1111-4111-8111-111111111111"
)

// tools is Capy's server as the gateway gives it: a path with a space.
var tools = gateway.Tools{Command: []string{self, "mcp", "--run", "r1"}}

// fixture reads the lines of testdata/name, recorded from claude.
func fixture(t *testing.T, name string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
}

// replay feeds lines to a new stream, then ends it as claude exited with
// code and stderr.
func replay(t *testing.T, lines [][]byte, code int, stderr []byte) (events []gateway.Event, end gateway.Event) {
	t.Helper()
	s := &stream{}
	for _, l := range lines {
		for _, e := range s.Line(l) {
			switch e.(type) {
			case gateway.Done, gateway.Failed:
				t.Fatalf("Line gave %v: only End ends a run", e)
			}
			events = append(events, e)
		}
	}
	return events, s.End(code, stderr)
}

// value is the argument after flag, before the prompt's --.
func value(t *testing.T, args []string, flag string) (string, bool) {
	t.Helper()
	i := slices.Index(args[:len(args)-2], flag)
	if i < 0 || i+1 >= len(args)-2 {
		return "", false
	}
	return args[i+1], true
}

// servers decodes an --mcp-config.
func servers(t *testing.T, cfg string) map[string]struct {
	Command string
	Args    []string
} {
	t.Helper()
	var c struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(cfg), &c); err != nil {
		t.Fatalf("--mcp-config %q: %v", cfg, err)
	}
	return c.MCPServers
}

// TestRunArgsKeepClaudeToCapysTools guards each flag that keeps a run to
// Capy's tools, with none of the user's settings, servers or CLAUDE.md:
// dropping one would let claude run a shell or the user's own servers.
func TestRunArgsKeepClaudeToCapysTools(t *testing.T) {
	args, err := runArgs(gateway.Query{Prompt: "--resume x; what was said?", Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(args); n < 2 || args[n-2] != "--" || args[n-1] != "--resume x; what was said?" {
		t.Fatalf("args %q; want the prompt last, right after --", args)
	}
	for _, f := range []string{"-p", "--strict-mcp-config"} {
		if !slices.Contains(args[:len(args)-2], f) {
			t.Errorf("no %s in %q", f, args)
		}
	}
	for flag, want := range map[string]string{
		"--tools":           "",
		"--allowedTools":    "mcp__capy__*",
		"--setting-sources": "",
		"--permission-mode": "dontAsk",
		"--output-format":   "stream-json",
	} {
		if got, ok := value(t, args, flag); !ok || got != want {
			t.Errorf("%s %q, %v; want %q", flag, got, ok, want)
		}
	}
	if slices.Contains(args[:len(args)-2], "--resume") || modelOf(args) != "" || slices.Contains(args, "--model") {
		t.Errorf("a resume or a model in a new conversation with no model: %q", args)
	}
	cfg, _ := value(t, args, "--mcp-config")
	s := servers(t, cfg)
	if capy, ok := s["capy"]; len(s) != 1 || !ok || capy.Command != self || !slices.Equal(capy.Args, tools.Command[1:]) {
		t.Fatalf("--mcp-config servers %+v; want capy alone, running %q", s, tools.Command)
	}
}

// TestRunArgsWithNoToolsLoadNoServer guards a run with no tools: an empty
// --mcp-config with --strict-mcp-config, so that neither Capy's server nor
// the user's load, no built-in tool, and none allowed.
func TestRunArgsWithNoToolsLoadNoServer(t *testing.T) {
	args, err := runArgs(gateway.Query{Prompt: "polish these", NoTools: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(args); args[n-2] != "--" || args[n-1] != "polish these" {
		t.Fatalf("args %q; want the prompt last, right after --", args)
	}
	opts := args[:len(args)-2]
	if !slices.Contains(opts, "--strict-mcp-config") || slices.Contains(opts, "--allowedTools") {
		t.Errorf("args %q; want --strict-mcp-config and no --allowedTools", args)
	}
	if got, ok := value(t, args, "--tools"); !ok || got != "" {
		t.Errorf("--tools %q, %v; want none", got, ok)
	}
	cfg, _ := value(t, args, "--mcp-config")
	if s := servers(t, cfg); len(s) != 0 {
		t.Fatalf("--mcp-config servers %+v; want none", s)
	}
}

// TestRunArgsResumeOnlyAUUIDAndPassTheModel guards against an id that
// claude would take for a flag, and a model or resume dropped.
func TestRunArgsResumeOnlyAUUIDAndPassTheModel(t *testing.T) {
	args, err := runArgs(gateway.Query{Prompt: "and then?", ConversationID: conversation, Model: "opus", Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := value(t, args, "--resume"); got != conversation {
		t.Fatalf("--resume %q, want %q", got, conversation)
	}
	if got := modelOf(args); got != "opus" {
		t.Fatalf("--model=%q, want opus", got)
	}
	if args[len(args)-1] != "and then?" {
		t.Fatalf("args %q; want the prompt last", args)
	}
	for _, id := range []string{"--dangerously-skip-permissions", "not-a-uuid"} {
		if _, err := runArgs(gateway.Query{Prompt: "p", ConversationID: id, Tools: tools}); err == nil {
			t.Errorf("conversation id %q accepted", id)
		}
	}
	if _, err := runArgs(gateway.Query{Prompt: "p"}); err == nil {
		t.Error("a run without Capy's tools accepted")
	}
}

// TestStreamReadsARun guards the parse of a recorded run with two tool
// calls: the conversation to resume, each call started then done, with
// the session it names, the text streamed as the message, and its end.
func TestStreamReadsARun(t *testing.T) {
	events, end := replay(t, fixture(t, "run.jsonl"), 0, nil)
	var others []gateway.Event
	streamed := ""
	for _, e := range events {
		switch e := e.(type) {
		case gateway.TextDelta:
			streamed += e.Text
		case gateway.Message:
			if e.Text != streamed {
				t.Fatalf("message %q; want the text its deltas wrote, %q", e.Text, streamed)
			}
			streamed = ""
			others = append(others, gateway.Message{})
		default:
			others = append(others, e)
		}
	}
	want := []gateway.Event{
		gateway.Started{ConversationID: conversation},
		gateway.ToolCall{Name: "get_session", SessionID: capySession},
		gateway.ToolCall{Name: "get_session", SessionID: capySession, Done: true},
		gateway.ToolCall{Name: "rename_session", SessionID: capySession},
		gateway.ToolCall{Name: "rename_session", SessionID: capySession, Done: true},
		gateway.Message{},
	}
	if !slices.Equal(others, want) {
		t.Fatalf("events %v, want %v", others, want)
	}
	if !strings.HasPrefix(messages(events)[0], "Action items from") {
		t.Fatalf("message %q, want the answer", messages(events)[0])
	}
	if end != (gateway.Done{}) {
		t.Fatalf("end %v, want Done", end)
	}
}

func messages(events []gateway.Event) []string {
	var texts []string
	for _, e := range events {
		if m, ok := e.(gateway.Message); ok {
			texts = append(texts, m.Text)
		}
	}
	return texts
}

// TestStreamReadsAResume guards a resumed conversation keeping its ID.
func TestStreamReadsAResume(t *testing.T) {
	events, end := replay(t, fixture(t, "resume.jsonl"), 0, nil)
	if len(events) == 0 || events[0] != (gateway.Started{ConversationID: conversation}) {
		t.Fatalf("events %v; want Started %s first", events, conversation)
	}
	if len(messages(events)) != 1 || end != (gateway.Done{}) {
		t.Fatalf("messages %q, end %v; want the answer, then Done", messages(events), end)
	}
}

// TestStreamClassifiesFailures guards each failure Ask tells apart, as
// claude reported it: a rejected resume, from its result or its stderr
// alone; signed out; a usage limit with when it resets; and anything else.
func TestStreamClassifiesFailures(t *testing.T) {
	rejected, err := os.ReadFile(filepath.Join("testdata", "resume-rejected.stderr"))
	if err != nil {
		t.Fatal(err)
	}
	// A run that failed otherwise, after a usage warning short of the
	// limit.
	run := fixture(t, "run.jsonl")
	crashed := run[:len(run)-1]
	for _, tc := range []struct {
		name   string
		lines  [][]byte
		code   int
		stderr []byte
		want   gateway.Failed
		// said is Claude Code's own error, which is not an answer.
		said string
	}{
		{"resume rejected", fixture(t, "resume-rejected.jsonl"), 1, rejected, gateway.Failed{Kind: gateway.ResumeRejected, Code: 1}, ""},
		{"resume rejected, stderr only", nil, 1, rejected, gateway.Failed{Kind: gateway.ResumeRejected, Code: 1}, ""},
		{"signed out", fixture(t, "signed-out.jsonl"), 1, nil, gateway.Failed{Kind: gateway.SignedOut, Code: 1}, "Not logged in"},
		{"usage limit", fixture(t, "usage-limit.jsonl"), 1, nil, gateway.Failed{Kind: gateway.UsageLimit, ResetAt: time.Unix(1793491200, 0), Code: 1}, "hit your limit"},
		{"crashed", crashed, 2, []byte("TypeError: undefined is not a function\n"), gateway.Failed{Kind: gateway.Exited, Code: 2}, ""},
	} {
		events, end := replay(t, tc.lines, tc.code, tc.stderr)
		f, ok := end.(gateway.Failed)
		if !ok || f.Kind != tc.want.Kind || f.Code != tc.want.Code || !f.ResetAt.Equal(tc.want.ResetAt) {
			t.Errorf("%s: end %+v, want %+v", tc.name, end, tc.want)
		}
		for _, e := range events {
			if m, ok := e.(gateway.Message); ok && tc.said != "" && strings.Contains(m.Text, tc.said) {
				t.Errorf("%s: message %q, Claude Code's error given as an answer", tc.name, m.Text)
			}
		}
	}
}

// TestContinueCommandResumesInClaude guards the command Terminal runs:
// the claude found, the conversation, Capy's server; never an id claude
// could take for a flag, nor a claude not found.
func TestContinueCommandResumesInClaude(t *testing.T) {
	a, tool, _ := installed(t, "2.1.296 (Claude Code)")
	h := a.(gateway.HandOff)
	if _, err := h.ContinueCommand(conversation, tools); !errors.Is(err, gateway.ErrUnavailable) {
		t.Fatalf("before a probe: %v, want ErrUnavailable", err)
	}
	a.Probe(context.Background())
	got, err := h.ContinueCommand(conversation, tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || got[0] != tool || got[1] != "--resume" || got[2] != conversation || got[3] != "--mcp-config" {
		t.Fatalf("command %q; want %s --resume %s --mcp-config …", got, tool, conversation)
	}
	s := servers(t, got[4])
	if capy := s["capy"]; len(s) != 1 || capy.Command != self || !slices.Equal(capy.Args, tools.Command[1:]) {
		t.Fatalf("--mcp-config servers %+v; want capy alone, running %q", s, tools.Command)
	}
	if _, err := h.ContinueCommand("--dangerously-skip-permissions", tools); err == nil {
		t.Fatal("a non-UUID id accepted")
	}
}

// modelOf is the model of --model=<model>, which keeps a model one
// argument that claude cannot take for a flag; "" for none.
func modelOf(args []string) string {
	for _, a := range args {
		if m, ok := strings.CutPrefix(a, "--model="); ok {
			return m
		}
	}
	return ""
}

// TestAToolCallOfAnotherShapeIsKept loses only the session of a call
// whose input is not the shape Capy's tools take, not the call.
func TestAToolCallOfAnotherShapeIsKept(t *testing.T) {
	var s stream
	got := s.Line([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"Looking."},{"type":"tool_use","id":"t1","name":"mcp__capy__get_session","input":{"id":5}}]}}`))
	if len(got) != 2 || got[0] != (gateway.Message{Text: "Looking."}) || got[1] != (gateway.ToolCall{Name: "get_session"}) {
		t.Fatalf("events %v", got)
	}
}
