//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/store/sqlite"
)

// Fixture sessions, at fixed IDs and times so that the transcript repeats.
const (
	syncID    = "11111111-1111-4111-8111-111111111111"
	reviewID  = "22222222-2222-4222-8222-222222222222"
	trashedID = "33333333-3333-4333-8333-333333333333"
)

var fixtureStart = time.Date(2026, 10, 7, 9, 30, 0, 0, time.UTC)

// fixtureHome is a home folder whose Capy data directory holds the
// fixture library and the user's profile.
func fixtureHome(t *testing.T, features string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "Library", "Application Support", "Capy")
	if err := os.MkdirAll(filepath.Join(dir, "agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	if features != "" {
		if err := os.WriteFile(filepath.Join(dir, "features.json"), []byte(features), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.json"), []byte(`{"name":"Ana","aliases":["Annie"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(dir, "capy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	add := func(id, title string, start time.Time, turns int) {
		if err := db.CreateSession(ctx, store.Session{ID: id, Mode: store.ModeCaptions, Source: store.SourceMic,
			LangA: "en-US", LangB: "zh-CN", StartedAt: start}); err != nil {
			t.Fatal(err)
		}
		for i := range turns {
			seg := store.Segment{ID: fmt.Sprintf("%08d-0000-4000-8000-%s", i, id[24:]), SessionID: id, Seq: i, Lang: "en-US",
				Text: fmt.Sprintf("%s point %d about pricing", title, i), Speaker: fmt.Sprintf("Speaker %d", i%2+1),
				Translation: fmt.Sprintf("第%d点", i), TranslationLang: "zh-CN", StartMs: int64(i) * 7_000, EndMs: int64(i)*7_000 + 6_000}
			if err := db.AppendSegment(ctx, seg); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.EndSession(ctx, id, start.Add(time.Duration(turns)*7*time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := db.SetRecap(ctx, id, store.Recap{Status: store.RecapReady, Title: title, Text: "# " + title + "\n\n- Ship on Friday"}); err != nil {
			t.Fatal(err)
		}
	}
	add(syncID, "Weekly sync", fixtureStart, 3)
	add(reviewID, "Pricing review", fixtureStart.Add(24*time.Hour), 205)
	add(trashedID, "Trashed pricing", fixtureStart.Add(48*time.Hour), 2)
	if _, err := db.TrashSession(ctx, trashedID, "user", ""); err != nil {
		t.Fatal(err)
	}
	return home
}

// mcpCommand is Capy mcp as an agent starts it, in the fixture home.
func mcpCommand(home string, args ...string) *exec.Cmd {
	cmd := exec.Command(capy, append([]string{"mcp"}, args...)...)
	cmd.Env = append(os.Environ(), "HOME="+home, "TZ=UTC")
	return cmd
}

// step is one exchange of the transcript.
type step struct {
	Call   string `json:"call"`
	Args   any    `json:"args,omitempty"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

var editIDs = regexp.MustCompile(`"edit_id":"[0-9a-f-]{36}"`)

// TestFakeMCPContract drives Capy mcp as Claude Code and ChatGPT do and
// pins what they rely on: the tools, their annotations and schemas, and
// their results on a fixture library.
func TestFakeMCPContract(t *testing.T) {
	home := fixtureHome(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := mcpCommand(home)
	cmd.Stderr = &stderr
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect: %v\n%s", err, stderr.String())
	}
	var steps []step
	init := cs.InitializeResult()
	steps = append(steps, step{Call: "initialize", Result: map[string]any{
		"server": init.ServerInfo.Name, "title": init.ServerInfo.Title, "instructions": init.Instructions}})
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tools []map[string]any
	for _, tool := range list.Tools {
		tools = append(tools, map[string]any{"name": tool.Name, "annotations": tool.Annotations, "input": tool.InputSchema})
	}
	steps = append(steps, step{Call: "tools/list", Result: tools})

	call := func(name string, args map[string]any) map[string]any {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		s := step{Call: name, Args: args}
		var out map[string]any
		if res.IsError {
			for _, c := range res.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					s.Error += tc.Text
				}
			}
		} else {
			b, _ := json.Marshal(res.StructuredContent)
			_ = json.Unmarshal(b, &out)
			s.Result = out
		}
		steps = append(steps, s)
		return out
	}
	call("list_sessions", nil)
	call("search", map[string]any{"query": "point 2 pricing", "limit": 3})
	first := call("get_session", map[string]any{"id": reviewID})
	call("get_session", map[string]any{"id": reviewID, "part": "transcript", "cursor": first["next_cursor"]})
	call("get_session", map[string]any{"id": trashedID})
	call("get_live_session", nil)
	call("get_profile", nil)
	call("rename_session", map[string]any{"id": syncID, "title": "Launch plan"})
	call("delete_session", map[string]any{"id": syncID})
	call("list_sessions", map[string]any{"limit": 5})
	// Stdin closed, the server exits on its own, and cleanly: the
	// transport's Close waits for it.
	if err := cs.Close(); err != nil {
		t.Fatalf("close: %v\n%s", err, stderr.String())
	}
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 0 {
		t.Fatalf("capy mcp exited %v\n%s", cmd.ProcessState, stderr.String())
	}

	report, err := json.MarshalIndent(steps, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	report = editIDs.ReplaceAll(report, []byte(`"edit_id":"<edit>"`))
	report = regexp.MustCompile(`"edit_id": "[0-9a-f-]{36}"`).ReplaceAll(report, []byte(`"edit_id": "<edit>"`))
	report = append(report, '\n')
	if err := os.MkdirAll("out", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("out", "mcp.json"), report, 0o600); err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "mcp.golden.json")
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
		t.Fatalf("transcript out/mcp.json differs from %s", golden)
	}
	// Its logs name tools, never what they read.
	if strings.Contains(stderr.String(), "pricing") || strings.Contains(stderr.String(), "Launch plan") {
		t.Fatalf("capy mcp logged session text:\n%s", stderr.String())
	}
}

// TestFakeMCPTurnedOff starts Capy mcp with Agent turned off: it serves
// nothing and exits with status 3, so the agent reports the server failed.
func TestFakeMCPTurnedOff(t *testing.T) {
	home := fixtureHome(t, `{"agent": false}`)
	cmd := mcpCommand(home)
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"), &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "Agent is turned off") {
		t.Fatalf("turned off: %v, stdout %q, stderr %q; want status 3 and nothing served", err, stdout.String(), stderr.String())
	}
}

// TestFakeMCPRefusesAnUnknownActor keeps edits attributable: an agent Ask
// started names its run, and there is no other actor.
func TestFakeMCPRefusesAnUnknownActor(t *testing.T) {
	home := fixtureHome(t, "")
	for _, args := range [][]string{{"-actor", "user"}, {"-actor", "agent"}, {"-actor", "mcp", "-run", "11111111-1111-4111-8111-111111111111"}} {
		cmd := mcpCommand(home, args...)
		cmd.Stdin = strings.NewReader("")
		err := cmd.Run()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 {
			t.Errorf("capy mcp %v: %v, want status 2", args, err)
		}
	}
}
