package codex

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/cli"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/internal/faketool"
)

const (
	thread      = "01a13661-6bb4-72e3-96c7-281e68e70643"
	capySession = "11111111-1111-4111-8111-111111111111"
)

// odd is a path TOML must quote: spaces, quotes, a backslash, and what
// JSON escapes.
const odd = `/Users/a "b"/C\apy <&>.app/Capy`

// tools is Capy's server as the gateway gives it.
var tools = gateway.Tools{Command: []string{odd, "mcp", "--run", `r "1"`}}

// configs are the values of every -c of args before the prompt's --.
func configs(args []string) []string {
	var cs []string
	for i := 0; i+1 < len(args)-2; i++ {
		if args[i] == "-c" {
			cs = append(cs, args[i+1])
		}
	}
	return cs
}

// capyServer decodes the -c flags naming Capy's server as codex does:
// each value is TOML.
func capyServer(t *testing.T, cs []string) (command string, args []string) {
	t.Helper()
	var doc strings.Builder
	n := 0
	for _, c := range cs {
		if rest, ok := strings.CutPrefix(c, "mcp_servers.capy."); ok && (strings.HasPrefix(rest, "command=") || strings.HasPrefix(rest, "args=")) {
			doc.WriteString(rest + "\n")
			n++
		}
	}
	var v struct {
		Command string   `toml:"command"`
		Args    []string `toml:"args"`
	}
	if _, err := toml.Decode(doc.String(), &v); err != nil || n != 2 {
		t.Fatalf("Capy's server %q is not TOML codex reads: %v", doc.String(), err)
	}
	return v.Command, v.Args
}

// TestRunArgsLockCodexDown guards each flag that keeps a run to Capy's
// tools, read-only, with none of the user's config: dropping one would
// let codex run a shell, browse, or load the user's own servers.
func TestRunArgsLockCodexDown(t *testing.T) {
	args, err := runArgs(gateway.Query{Prompt: "-m o3: what was said?", Tools: tools}, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(args); n < 2 || args[0] != "exec" || args[n-2] != "--" || args[n-1] != "-m o3: what was said?" {
		t.Fatalf("args %q; want exec …, the prompt last, right after --", args)
	}
	opts := args[:len(args)-2]
	if i := slices.Index(opts, "-s"); i < 0 || opts[i+1] != "read-only" {
		t.Errorf("no -s read-only in %q", args)
	}
	for _, f := range []string{"--json", "--ignore-user-config", "--skip-git-repo-check"} {
		if !slices.Contains(opts, f) {
			t.Errorf("no %s in %q", f, args)
		}
	}
	if modelOf(args) != "" || slices.Contains(opts, "-m") {
		t.Errorf("a model without one: %q", args)
	}
	cs := configs(args)
	for _, c := range []string{`web_search="disabled"`, `mcp_servers.capy.default_tools_approval_mode="approve"`} {
		if !slices.Contains(cs, c) {
			t.Errorf("no -c %s in %q", c, args)
		}
	}
	var disabled []string
	for i, a := range opts {
		if a == "--disable" && i+1 < len(opts) {
			disabled = append(disabled, opts[i+1])
		}
	}
	for _, f := range []string{"shell_tool", "unified_exec", "image_generation", "apps", "plugins", "browser_use", "computer_use"} {
		if !slices.Contains(disabled, f) {
			t.Errorf("%s not disabled: %q", f, disabled)
		}
	}
	if cmd, cargs := capyServer(t, cs); cmd != odd || !slices.Equal(cargs, tools.Command[1:]) {
		t.Fatalf("Capy's server %q %q, want %q", cmd, cargs, tools.Command)
	}
	if _, err := runArgs(gateway.Query{Prompt: "p"}, ""); err == nil {
		t.Error("a run without Capy's tools accepted")
	}
}

// TestRunArgsWithNoToolsNameNoServer guards a run with no tools: no
// mcp_servers flag at all, --ignore-user-config leaving codex none of the
// user's, and the rest of the lockdown kept.
func TestRunArgsWithNoToolsNameNoServer(t *testing.T) {
	args, err := runArgs(gateway.Query{Prompt: "polish these", NoTools: true}, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(args); args[0] != "exec" || args[n-2] != "--" || args[n-1] != "polish these" {
		t.Fatalf("args %q; want exec …, the prompt last, right after --", args)
	}
	for _, c := range configs(args) {
		if strings.HasPrefix(c, "mcp_servers.") {
			t.Errorf("a server flag -c %s in %q", c, args)
		}
	}
	opts := args[:len(args)-2]
	if !slices.Contains(opts, "--ignore-user-config") || !slices.Contains(configs(args), `web_search="disabled"`) {
		t.Errorf("args %q lost the lockdown", args)
	}
	if i := slices.Index(opts, "shell_tool"); i < 1 || opts[i-1] != "--disable" {
		t.Errorf("args %q; want the shell disabled", args)
	}
}

// TestRunArgsOfAResume guards exec resume, which has no -s: a -s there
// fails the run, and a sandbox dropped leaves codex writing.
func TestRunArgsOfAResume(t *testing.T) {
	args, err := runArgs(gateway.Query{Prompt: "and then?", ConversationID: thread, Tools: tools}, "gpt-6.1-sol")
	if err != nil {
		t.Fatal(err)
	}
	n := len(args)
	if args[0] != "exec" || args[1] != "resume" || args[n-3] != thread || args[n-2] != "--" || args[n-1] != "and then?" {
		t.Fatalf("args %q; want exec resume …, %s, -- and the prompt", args, thread)
	}
	if slices.Contains(args[:n-2], "-s") {
		t.Errorf("-s in a resume: %q", args)
	}
	if !slices.Contains(configs(args), `sandbox_mode="read-only"`) {
		t.Errorf("no read-only sandbox in %q", args)
	}
	if modelOf(args[:n-2]) != "gpt-6.1-sol" {
		t.Errorf("no --model=gpt-6.1-sol in %q", args)
	}
	for _, id := range []string{"--dangerously-bypass-approvals-and-sandbox", "not-a-uuid"} {
		if _, err := runArgs(gateway.Query{Prompt: "p", ConversationID: id, Tools: tools}, ""); err == nil {
			t.Errorf("conversation id %q accepted", id)
		}
	}
}

// TestRunAnswersWithCodexsDefaultModel guards against a run of no model
// given: --ignore-user-config leaves codex none, so a run without -m from
// Codex's own config fails.
func TestRunAnswersWithCodexsDefaultModel(t *testing.T) {
	a, tool := installed(t)
	cfg := filepath.Join(a.home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(`model = "gpt-6.1-sol"`), 0o600); err != nil {
		t.Fatal(err)
	}
	model := func(q gateway.Query) string {
		t.Helper()
		q.Prompt, q.Tools = "p", tools
		r, err := a.Run(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		for range r.Events() { // the fake exits 2 on exec
		}
		calls := faketool.Calls(t, tool)
		last := calls[len(calls)-1]
		return modelOf(last)
	}
	if got := model(gateway.Query{}); got != "gpt-6.1-sol" {
		t.Fatalf("model %q without one given, want Codex's default", got)
	}
	if got := model(gateway.Query{Model: "gpt-6-astra"}); got != "gpt-6-astra" {
		t.Fatalf("model %q, want the one given", got)
	}
}

// fixture reads the lines of testdata/name, recorded from codex.
func fixture(t *testing.T, name string) [][]byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n"))
}

// replay feeds lines to a new stream, then ends it as codex exited with
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

// TestStreamReadsARun guards the parse of a recorded run: the thread to
// resume, both messages in order, each tool call started then done with
// the session it names, and its end.
func TestStreamReadsARun(t *testing.T) {
	events, end := replay(t, fixture(t, "run.jsonl"), 0, nil)
	var texts []string
	for i, e := range events {
		if m, ok := e.(gateway.Message); ok {
			texts = append(texts, m.Text)
			events[i] = gateway.Message{}
		}
	}
	want := []gateway.Event{
		gateway.Started{ConversationID: thread},
		gateway.Message{},
		gateway.ToolCall{Name: "get_session", SessionID: capySession},
		gateway.ToolCall{Name: "get_session", SessionID: capySession, Done: true},
		gateway.ToolCall{Name: "rename_session", SessionID: capySession},
		gateway.ToolCall{Name: "rename_session", SessionID: capySession, Done: true},
		gateway.Message{},
	}
	if !slices.Equal(events, want) {
		t.Fatalf("events %v, want %v", events, want)
	}
	if !strings.HasPrefix(texts[0], "I’ll read the session") || !strings.HasPrefix(texts[1], "Renamed to **Rehearsal plan**") {
		t.Fatalf("messages %q, want both in order", texts)
	}
	if end != (gateway.Done{}) {
		t.Fatalf("end %v, want Done", end)
	}
}

// TestStreamReadsAResume guards a resumed thread keeping its ID.
func TestStreamReadsAResume(t *testing.T) {
	events, end := replay(t, fixture(t, "resume.jsonl"), 0, nil)
	if len(events) != 2 || events[0] != (gateway.Started{ConversationID: thread}) {
		t.Fatalf("events %v; want Started %s, then the answer", events, thread)
	}
	if end != (gateway.Done{}) {
		t.Fatalf("end %v, want Done", end)
	}
}

// TestStreamClassifiesFailures guards each failure Ask tells apart, as
// codex reported it, and that the error lines of a retry that then
// succeeded fail nothing.
func TestStreamClassifiesFailures(t *testing.T) {
	rejected, err := os.ReadFile(filepath.Join("testdata", "resume-rejected.stderr"))
	if err != nil {
		t.Fatal(err)
	}
	signedOut := fixture(t, "signed-out.jsonl")
	retried := [][]byte{signedOut[0], signedOut[1], signedOut[2], []byte(`{"type":"turn.completed","usage":{}}`)}
	run := fixture(t, "run.jsonl")
	crashed := run[:len(run)-1]
	for _, tc := range []struct {
		name   string
		lines  [][]byte
		code   int
		stderr []byte
		want   gateway.Event
	}{
		{"signed out", signedOut, 1, nil, gateway.Failed{Kind: gateway.SignedOut, Code: 1}},
		{"retried", retried, 0, nil, gateway.Done{}},
		{"usage limit", fixture(t, "usage-limit.jsonl"), 1, nil, gateway.Failed{Kind: gateway.UsageLimit, Code: 1}},
		// codex prints nothing on stdout for a rejected resume.
		{"resume rejected", nil, 1, rejected, gateway.Failed{Kind: gateway.ResumeRejected, Code: 1}},
		{"crashed", crashed, 101, []byte("thread 'main' panicked at core/src/codex.rs\n"), gateway.Failed{Kind: gateway.Exited, Code: 101}},
	} {
		if _, end := replay(t, tc.lines, tc.code, tc.stderr); end != tc.want {
			t.Errorf("%s: end %+v, want %+v", tc.name, end, tc.want)
		}
	}
}

// TestContinueCommandResumesInCodex guards the command Terminal runs:
// the codex found, Capy's server, the thread; never an id codex could
// take for a flag, nor a codex not found.
func TestContinueCommandResumesInCodex(t *testing.T) {
	unprobed := New(Options{Env: cli.Env{Home: t.TempDir(), Root: t.TempDir(), Run: cli.Exec, Timeout: 10 * time.Second}}).(gateway.HandOff)
	if _, err := unprobed.ContinueCommand(thread, tools); !errors.Is(err, gateway.ErrUnavailable) {
		t.Fatalf("before a probe: %v, want ErrUnavailable", err)
	}
	a, tool := installed(t)
	got, err := a.ContinueCommand(thread, tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 || got[0] != tool || got[1] != "resume" || got[2] != "-c" || got[4] != "-c" || got[6] != thread {
		t.Fatalf("command %q; want %s resume -c … -c … %s", got, tool, thread)
	}
	if cmd, args := capyServer(t, []string{got[3], got[5]}); cmd != odd || !slices.Equal(args, tools.Command[1:]) {
		t.Fatalf("Capy's server %q %q, want %q", cmd, args, tools.Command)
	}
	if _, err := a.ContinueCommand("--dangerously-bypass-approvals-and-sandbox", tools); err == nil {
		t.Fatal("a non-UUID id accepted")
	}
}

// modelOf is the model of --model=<model>, which keeps a model one
// argument that codex cannot take for a flag; "" for none.
func modelOf(args []string) string {
	for _, a := range args {
		if m, ok := strings.CutPrefix(a, "--model="); ok {
			return m
		}
	}
	return ""
}

// TestStreamSeesCodexsOwnTools reports whatever codex does that is not
// one of Capy's tools, by its kind, so that a tool a later codex turns
// back on shows in the logs and in the replay's check.
func TestStreamSeesCodexsOwnTools(t *testing.T) {
	var s stream
	var got []gateway.Event
	for _, l := range []string{
		`{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"cat ~/.ssh/id_ed25519","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"reasoning","text":"thinking"}}`,
		`{"type":"item.completed","item":{"id":"item_3","type":"web_search","query":"x"}}`,
	} {
		got = append(got, s.Line([]byte(l))...)
	}
	want := []gateway.Event{gateway.ToolCall{Name: "command_execution"}, gateway.ToolCall{Name: "web_search", Done: true}}
	if !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
}

// TestAToolCallOfAnotherShapeIsKept loses only the session of a call
// whose arguments are not the shape Capy's tools take, not the call.
func TestAToolCallOfAnotherShapeIsKept(t *testing.T) {
	var s stream
	got := s.Line([]byte(`{"type":"item.started","item":{"id":"i1","type":"mcp_tool_call","server":"capy","tool":"get_session","arguments":{"id":["x"]}}}`))
	if len(got) != 1 || got[0] != (gateway.ToolCall{Name: "get_session"}) {
		t.Fatalf("events %v", got)
	}
}
