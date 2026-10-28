//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway/fake"
)

// askScript is fakeagent's part: read the session, rename it, and answer
// citing its first turn.
var askScript = fake.Script{
	Calls: []fake.Call{
		{Tool: "get_session", Args: map[string]any{"id": "{session}", "part": "transcript"}},
		{Tool: "rename_session", Args: map[string]any{"id": "{session}", "title": "Capy demo"}},
	},
	Answer: "The demo shows Scribe turning speech into text [00:02], then Translator and Summarizer.",
}

// TestFakeAsk asks a replayed session a question through the whole path:
// the binary, its engine and library, Agent's gateway and Ask, the Claude
// Code adapter running fakeagent as claude in its own process group, and
// the real Capy mcp that fakeagent starts with the run's ID. Asked after
// the last turn, and while the replay records.
func TestFakeAsk(t *testing.T) {
	c := cases[0]
	script := filepath.Join(t.TempDir(), "script.json")
	b, err := json.Marshal(askScript)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, b, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, at := range map[string]string{"ask-fake": "0", "ask-at-fake": "2"} {
		t.Run(name, func(t *testing.T) {
			env := append(fakeZoom(t, c), fake.ScriptEnv+"="+script)
			report, err := replay(t, c, filepath.Join("out", name+".json"), false, env,
				"-ask", "What does the demo show?", "-ask-at", at, "-agent", "claude-code", "-agent-path", fakeAgent)
			if err != nil {
				t.Fatalf("capy failed: %v\n%s", err, report)
			}
			matchGolden(t, name, report)
		})
	}
}

// TestFakeAskDeletesSession verifies that replay restores a session the
// agent deleted before reading its hidden thread and transcript. The
// golden report retains each edit and validates the answer's citation.
func TestFakeAskDeletesSession(t *testing.T) {
	c := cases[0]
	script := filepath.Join(t.TempDir(), "script.json")
	b, err := json.Marshal(fake.Script{
		Calls: []fake.Call{
			{Tool: "get_session", Args: map[string]any{"id": "{session}", "part": "transcript"}},
			{Tool: "rename_session", Args: map[string]any{"id": "{session}", "title": "Capy demo"}},
			{Tool: "update_recap", Args: map[string]any{"id": "{session}", "recap": "# Demo\n\n- Scribe, Translator and Summarizer"}},
			{Tool: "delete_session", Args: map[string]any{"id": "{session}"}},
		},
		Answer: "The demo begins at [00:02]. I edited it and moved it to Recently Deleted.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, b, 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(fakeZoom(t, c), fake.ScriptEnv+"="+script)
	report, err := replay(t, c, filepath.Join("out", "ask-delete-fake.json"), false, env,
		"-ask", "Edit the demo and move it to Recently Deleted.", "-agent", "claude-code", "-agent-path", fakeAgent)
	if err != nil {
		t.Fatalf("capy failed: %v\n%s", err, report)
	}
	matchGolden(t, "ask-delete-fake", report)
}
