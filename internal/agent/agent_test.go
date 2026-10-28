package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/agent/prefs"
	"github.com/onyx-dot-app/Capy/internal/agent/threads"
	"github.com/onyx-dot-app/Capy/internal/features"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/store/sqlite"
)

type off struct{}

func (off) Enabled(features.Feature) bool { return false }

// TestDeleteNowErasesThreadsWithAgentOff keeps Delete Now's promise: a
// session's Ask threads go with it, whether Agent is on or off.
func TestDeleteNowErasesThreadsWithAgentOff(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sqlite.Open(ctx, filepath.Join(dir, "capy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := New(ctx, Deps{Store: db, Gate: off{}, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	session1, session2 := endedSession(t, db), endedSession(t, db)
	for _, id := range []string{session1, session2} {
		if _, err := m.Threads().CreateThread(ctx, threads.Thread{ID: uuid.NewString(), Capability: "ask", SessionID: id,
			Scope: threads.ScopeSession, Agent: "claude-code", Title: "What did we decide?"}); err != nil {
			t.Fatal(err)
		}
	}
	purger := session.NewPurger(db)
	purger.OnPurge(m.PurgeHook())
	if _, err := db.TrashSession(ctx, session1, "user", ""); err != nil {
		t.Fatal(err)
	}
	if err := purger.Purge(ctx, session1); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]int{session1: 0, session2: 1} {
		list, err := m.Threads().ListThreads(ctx, threads.ListQuery{SessionID: id})
		if err != nil || len(list) != want {
			t.Fatalf("threads of %s: %d, %v; want %d", id, len(list), err, want)
		}
	}
	// Agent's data is in its own folder: agent.db, and the empty folder
	// agents run in.
	for _, p := range []string{"agent/agent.db", "agent/work"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "agent", "work")); len(entries) != 0 {
		t.Errorf("the working folder is not empty: %v", entries)
	}
}

func endedSession(t *testing.T, db *sqlite.DB) string {
	t.Helper()
	ctx := context.Background()
	s := store.Session{ID: uuid.NewString(), Mode: store.ModeCaptions, Source: store.SourceMic, LangA: "en-US", LangB: "zh-CN",
		StartedAt: time.Now().Add(-time.Hour)}
	if err := db.CreateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession(ctx, s.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	return s.ID
}

func TestAgentPrefsDropWhatThisBuildCannotUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json")
	body := `{"agent":"claude code!","paths":{"codex":"relative/codex","claude-code":"/opt/bin/claude"},
		"models":{"codex":"gpt-6.1-sol","x":""},
		"inspectorWidth":9000,"livePanelWidth":100,"connected":["claude-code","claude-code",""]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p := prefs.Read(path)
	if p.Agent != "" || p.Paths["codex"] != "" || p.Paths["claude-code"] != "/opt/bin/claude" || p.Models["codex"] != "gpt-6.1-sol" ||
		len(p.Models) != 1 ||
		p.InspectorWidth != prefs.InspectorWidth || p.LivePanelWidth != prefs.LivePanelWidth || !slices.Equal(p.Connected, []string{"claude-code"}) {
		t.Fatalf("normalized %+v", p)
	}
	f := prefs.Load(path)
	if err := f.Update(func(p *prefs.Prefs) { p.Agent = "codex" }); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 || prefs.Read(path).Agent != "codex" {
		t.Fatalf("saved: %v, %v, %q", fi.Mode(), err, prefs.Read(path).Agent)
	}
	// A copy from Get cannot change the file's preferences.
	got := f.Get()
	got.Connected[0] = "changed"
	if f.Get().Connected[0] != "claude-code" {
		t.Fatal("Get shares its slices with the file")
	}
	// An unwritable folder fails the save without naming the path.
	ro := prefs.Load(filepath.Join(t.TempDir(), "missing", "agent.json"))
	if err := ro.Update(func(p *prefs.Prefs) { p.Agent = "x" }); err == nil || errors.Is(err, os.ErrNotExist) == false {
		t.Fatalf("save to a missing folder: %v", err)
	}
	provider := filepath.Join(t.TempDir(), "agent.json")
	// A model may name its provider.
	if err := os.WriteFile(provider, []byte(`{"models":{"codex":"openai/gpt-oss-120b"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if p := prefs.Read(provider); p.Models["codex"] != "openai/gpt-oss-120b" {
		t.Fatalf("model %q", p.Models["codex"])
	}
}
