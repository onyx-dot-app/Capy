package agent

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/agent/ask"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/cli"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/fake"
	"github.com/onyx-dot-app/Capy/internal/agent/prefs"
	"github.com/onyx-dot-app/Capy/internal/agent/threads"
	"github.com/onyx-dot-app/Capy/internal/features"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/store/sqlite"
)

type on struct{}

func (on) Enabled(features.Feature) bool { return true }

// TestMain lets the test binary stand in for what the module starts: as
// `<binary> mcp`, Capy mcp; named claude or codex, fakeagent.
func TestMain(m *testing.M) {
	switch name := filepath.Base(os.Args[0]); {
	case len(os.Args) > 1 && os.Args[1] == "mcp":
		os.Exit(ServeMCP(context.Background(), os.Args[2:], MCPDeps{
			DataDir: func() (string, error) { return "", os.ErrNotExist },
			Gate:    func(string) features.Gate { return on{} },
			OpenLibrary: func(ctx context.Context, path string) (store.Store, func() error, error) {
				db, err := sqlite.Open(ctx, path)
				if err != nil {
					return nil, nil, err
				}
				return db, db.Close, nil
			},
			Stderr: io.Discard,
		}))
	case name == "claude" || name == "codex":
		os.Exit(fake.Main(os.Args[1:], name == "codex", os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// cliFixture is the module on a library of one session, its agent id a
// link to this test binary, which acts as that CLI.
type cliFixture struct {
	m       *Module
	lib     *sqlite.DB
	dir     string
	session string
	script  string
}

func newCLIFixture(t *testing.T, id, program string) *cliFixture {
	t.Helper()
	ctx := context.Background()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin", program)
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(self, bin); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	lib, err := sqlite.Open(ctx, filepath.Join(dir, "capy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lib.Close() })
	if err := os.MkdirAll(folder(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := prefs.Load(filepath.Join(folder(dir), "agent.json")).Update(func(p *prefs.Prefs) {
		p.Agent, p.Paths = id, map[string]string{id: bin}
	}); err != nil {
		t.Fatal(err)
	}
	// The real tools are never found: Root and Home are the test's, and
	// no login shell is asked.
	env := cli.Env{Home: root, Root: root, Run: cli.Exec, Timeout: 10 * time.Second}
	m, err := New(ctx, Deps{Store: lib, Gate: on{}, Dir: dir, Self: self, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	if err := m.Allow(id); err != nil {
		t.Fatal(err)
	}
	f := &cliFixture{m: m, lib: lib, dir: dir, session: endedSession(t, lib), script: filepath.Join(root, "script.json")}
	t.Setenv(fake.ScriptEnv, f.script)
	return f
}

func (f *cliFixture) play(t *testing.T, s fake.Script) {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.script, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func finish(t *testing.T, turn *ask.Turn) ask.State {
	t.Helper()
	var last ask.State
	timeout := time.After(30 * time.Second)
	for {
		select {
		case st, ok := <-turn.States():
			if !ok {
				return last
			}
			last = st
		case <-timeout:
			t.Fatalf("no end; last %+v", last)
		}
	}
}

// TestAskThroughEachCLIWithCapyMCP asks through each built-in adapter: its
// arguments and stream as the CLI's, its process, and the real Capy mcp
// it starts for the run, on the module's library. A rename shows as the
// run's edit, Undo restores the title, a follow-up resumes the
// conversation, and one the agent lost starts over with the thread.
func TestAskThroughEachCLIWithCapyMCP(t *testing.T) {
	for id, program := range map[string]string{"claude-code": "claude", "codex": "codex"} {
		t.Run(id, func(t *testing.T) {
			f := newCLIFixture(t, id, program)
			ctx := context.Background()
			f.play(t, fake.Script{
				Calls: []fake.Call{
					{Tool: "get_session", Args: map[string]any{"id": "{session}", "part": "transcript"}},
					{Tool: "rename_session", Args: map[string]any{"id": "{session}", "title": "Pricing plan"}},
				},
				Answer: "Ana updates pricing [00:07].",
			})
			before, err := f.lib.GetSession(ctx, f.session)
			if err != nil {
				t.Fatal(err)
			}
			turn, err := f.m.Ask().Ask(ctx, ask.Scope{SessionID: f.session}, "What are the action items?")
			if err != nil {
				t.Fatal(err)
			}
			st := finish(t, turn)
			if st.Failure != nil || st.Answer != "Ana updates pricing [00:07]." || len(st.Used) != 2 {
				t.Fatalf("last state %+v", st)
			}
			if len(st.Edits) != 1 || st.Edits[0].Actor != "agent" || uuid.Validate(st.Edits[0].Origin) != nil {
				t.Fatalf("edits %+v, want the rename by the run", st.Edits)
			}
			if s, _ := f.lib.GetSession(ctx, f.session); s.Title != "Pricing plan" {
				t.Fatalf("title %q after the rename", s.Title)
			}
			if _, err := f.m.Ask().Undo(ctx, st.Edits[0].ID); err != nil {
				t.Fatal(err)
			}
			if s, _ := f.lib.GetSession(ctx, f.session); s.Title != before.Title {
				t.Fatalf("title %q after Undo", s.Title)
			}
			d, err := f.m.Ask().Thread(ctx, st.ThreadID)
			if err != nil || d.ConversationID == "" {
				t.Fatalf("thread %+v, %v", d.Thread, err)
			}

			f.play(t, fake.Script{Answer: "By Thursday."})
			turn, err = f.m.Ask().FollowUp(ctx, d.ID, "By when?")
			if err != nil {
				t.Fatal(err)
			}
			if st := finish(t, turn); st.Failure != nil || st.Answer != "By Thursday." {
				t.Fatalf("follow-up %+v", st)
			}
			if again, _ := f.m.Ask().Thread(ctx, d.ID); again.ConversationID != d.ConversationID {
				t.Fatalf("the follow-up did not resume %s: now %s", d.ConversationID, again.ConversationID)
			}

			f.play(t, fake.Script{Answer: "Ana.", RejectResume: true})
			turn, err = f.m.Ask().FollowUp(ctx, d.ID, "Who again?")
			if err != nil {
				t.Fatal(err)
			}
			if st := finish(t, turn); st.Failure != nil || st.Answer != "Ana." {
				t.Fatalf("after a lost conversation %+v", st)
			}
			if again, _ := f.m.Ask().Thread(ctx, d.ID); again.ConversationID == d.ConversationID || len(again.Messages) != 7 {
				t.Fatalf("conversation %s (was %s), %d messages", again.ConversationID, d.ConversationID, len(again.Messages))
			}
			if runs, _ := os.ReadDir(runsDir(f.dir)); len(runs) != 0 {
				t.Fatalf("runs still named after they ended: %v", runs)
			}
		})
	}
}

// TestAFailedCLIRunTellsWhy reads the CLI's way of saying it: signed out,
// or out of its usage.
func TestAFailedCLIRunTellsWhy(t *testing.T) {
	for id, program := range map[string]string{"claude-code": "claude", "codex": "codex"} {
		for fail, want := range map[string]gateway.Failure{"signed-out": gateway.SignedOut, "usage-limit": gateway.UsageLimit} {
			t.Run(id+"/"+fail, func(t *testing.T) {
				f := newCLIFixture(t, id, program)
				f.play(t, fake.Script{Answer: "x", Fail: fail})
				turn, err := f.m.Ask().Ask(context.Background(), ask.Scope{SessionID: f.session}, "Hi")
				if err != nil {
					t.Fatal(err)
				}
				st := finish(t, turn)
				if st.Failure == nil || st.Failure.Kind != want {
					t.Fatalf("failure %+v, want %v", st.Failure, want)
				}
				d, _ := f.m.Ask().Thread(context.Background(), st.ThreadID)
				if len(d.Messages) != 1 || d.Messages[0].Status != threads.StatusUnanswered {
					t.Fatalf("messages %+v", d.Messages)
				}
			})
		}
	}
}

// TestCapyMCPServesOnlyARunGoing refuses a run the gateway did not start,
// or one that ended, so that nothing else can pass its edits for Ask's.
func TestCapyMCPServesOnlyARunGoing(t *testing.T) {
	dir := t.TempDir()
	serve := func(args ...string) int {
		return ServeMCP(context.Background(), args, MCPDeps{
			DataDir: func() (string, error) { return dir, nil },
			Gate:    func(string) features.Gate { return on{} },
			OpenLibrary: func(context.Context, string) (store.Store, func() error, error) {
				t.Fatal("the library opened")
				return nil, nil, nil
			},
			Stderr: io.Discard,
		})
	}
	if code := serve("-actor", "agent", "-run", uuid.NewString()); code != exitUsage {
		t.Fatalf("an unknown run: status %d, want %d", code, exitUsage)
	}
	if code := serve("-data", "relative/dir"); code != exitUsage {
		t.Fatalf("a relative -data: status %d, want %d", code, exitUsage)
	}
	if code := serve("-data", dir, "-actor", "agent", "-run", "../"+strings.Repeat("a", 8)); code != exitUsage {
		t.Fatalf("a run that is a path: status %d", code)
	}
}
