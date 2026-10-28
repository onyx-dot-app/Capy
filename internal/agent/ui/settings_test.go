package ui

import (
	"context"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/prefs"
	"github.com/onyx-dot-app/Capy/internal/features"
	"github.com/onyx-dot-app/Capy/internal/views/addon"
)

// testAgent is an agent whose probe and connection the test sets; it
// records what it was asked to run.
type testAgent struct {
	info gateway.Info
	mu   sync.Mutex
	// probe is what it is found as, probes how often it was probed.
	probe  gateway.Probe
	probes int
	conn   gateway.Connection
	fail   error // Connect and Disconnect fail with it
	ran    []string
}

func (a *testAgent) Info() gateway.Info { return a.info }

func (a *testAgent) Probe(context.Context) gateway.Probe {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.probes++
	return a.probe
}

func (a *testAgent) Run(context.Context, gateway.Query) (gateway.Run, error) {
	return nil, gateway.ErrUnavailable
}

func (a *testAgent) ConnectCommands() [][]string {
	return [][]string{{a.info.Command, "mcp", "add", "capy", "--", "/Applications/Capy beta.app/Contents/MacOS/Capy", "mcp"}}
}

func (a *testAgent) DisconnectCommand() []string {
	return []string{a.info.Command, "mcp", "remove", "capy"}
}

func (a *testAgent) do(what string, to gateway.Connection) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ran = append(a.ran, what)
	if a.fail != nil {
		return a.fail
	}
	a.conn = to
	return nil
}

func (a *testAgent) Connect(context.Context) error { return a.do("connect", gateway.Connected) }
func (a *testAgent) Disconnect(context.Context) error {
	return a.do("disconnect", gateway.NotConnected)
}

func (a *testAgent) Connection(context.Context) (gateway.Connection, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conn, nil
}

func (a *testAgent) calls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.ran)
}

// modelsAgent is a testAgent that offers models.
type modelsAgent struct{ *testAgent }

func (modelsAgent) DefaultModel(context.Context) (string, error) { return "gpt-6.1-sol", nil }
func (modelsAgent) Models(context.Context) ([]string, error) {
	return []string{"gpt-6.1-sol", "gpt-6.1-mini"}, nil
}

// exitErr is a program that exited with a status.
type exitErr int

func (e exitErr) Error() string { return "exit " + strconv.Itoa(int(e)) }
func (e exitErr) ExitCode() int { return int(e) }

func claude(home string) *testAgent {
	return &testAgent{info: gateway.Info{ID: "claude-code", Name: "Claude Code", Command: "claude",
		Download: "https://claude.com/product/claude-code"},
		probe: gateway.Probe{Found: true, Path: home + "/.local/bin/claude", Version: "2.1.296"}}
}

func chatgpt() *testAgent {
	return &testAgent{info: gateway.Info{ID: "codex", Name: "ChatGPT", Command: "codex", Program: "Codex",
		AppsSubject: "ChatGPT, the Codex CLI and Codex in your editor",
		Download:    "https://chatgpt.com/download"},
		probe: gateway.Probe{Found: true, Path: "/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex", Version: "0.162.0-alpha.17.2"}}
}

type gate struct{ on bool }

func (g *gate) Enabled(f features.Feature) bool { return g.on && f == features.Agent }

// host is the app as the tests stand for it: Update queues fn for the
// test's goroutine, as the main thread would run it.
type host struct{ q chan func() }

func (h host) ShowSession(string)                    {}
func (h host) ShowTranscript(string, int64)          {}
func (h host) LibraryChanged()                       {}
func (h host) LiveSession() *addonSession            { return nil }
func (h host) ExpandCaptionBar() bool                { return false }
func (h host) RegisterShortcut(string, func()) error { return nil }
func (h host) Toast(ui.Toast)                        {}
func (h host) Window() *mygo.Window                  { return nil }
func (h host) Update(fn func())                      { h.q <- fn }
func (h host) Polished(string)                       {}

// settle runs what was posted until nothing more comes.
func (h host) settle(t *testing.T) {
	t.Helper()
	for {
		select {
		case fn := <-h.q:
			fn()
		case <-time.After(300 * time.Millisecond):
			return
		}
	}
}

type fixture struct {
	a     *AddOn
	h     host
	g     *gate
	prefs *prefs.File
	tt    *ui.Tester
	chose string // what Choose… picks
}

func newFixture(t *testing.T, agents ...gateway.Agent) *fixture {
	t.Helper()
	f := &fixture{h: host{q: make(chan func(), 16)}, g: &gate{on: true},
		prefs: prefs.Load(filepath.Join(t.TempDir(), "agent.json"))}
	f.a = New(Options{Gate: f.g, Gateway: gateway.New(gateway.Options{}, agents...), Prefs: f.prefs,
		ChooseFile: func() (string, error) { return f.chose, nil }})
	f.a.Start(f.h)
	f.tt = ui.NewTester(func(c *ui.Context) {
		ui.Column(c).Fill().Padding(20, 24).AlignItems(ui.Stretch).Children(func() { f.a.Settings(c) })
	}, 560, 700)
	f.h.settle(t)
	f.tt.Frame()
	return f
}

func (f *fixture) click(t *testing.T, label string) {
	t.Helper()
	if err := f.tt.Click(label); err != nil {
		t.Fatalf("%v; texts %q", err, f.tt.Texts())
	}
	f.h.settle(t)
	f.tt.Frame()
}

// shows reports whether a text reads exactly s.
func (f *fixture) shows(s string) bool {
	_, ok := f.tt.Find(s)
	return ok
}

// pick opens the pop-up of the row labelled label and chooses the option
// down from the chosen one, with the keyboard.
func (f *fixture) pick(t *testing.T, label string, down int) {
	t.Helper()
	f.field(t, label)
	for range down {
		f.tt.Key(0, ui.KeyDown)
	}
	f.tt.Key(0, ui.KeyEnter)
	f.h.settle(t)
	f.tt.Frame()
}

// confirm presses Enter, which clicks an alert's default button.
func (f *fixture) confirm(t *testing.T) {
	t.Helper()
	f.tt.Key(0, ui.KeyEnter)
	f.h.settle(t)
	f.tt.Frame()
}

// field clicks the control of the settings row labelled label, right of
// the label, which the control shares as its name.
func (f *fixture) field(t *testing.T, label string) {
	t.Helper()
	r, ok := f.tt.Find(label)
	if !ok {
		t.Fatalf("no row %q; texts %q", label, f.tt.Texts())
	}
	f.tt.ClickAt(560-24-12-100, r.Y+r.H/2)
	f.tt.Frame()
}

func (f *fixture) has(t *testing.T, texts ...string) {
	t.Helper()
	for _, s := range texts {
		if !f.tt.HasText(s) {
			t.Errorf("Settings lacks %q; texts %q", s, f.tt.Texts())
		}
	}
}

func TestSettingsListsTheAgentsFoundAndWhere(t *testing.T) {
	home := "/Users/ana"
	t.Setenv("HOME", home)
	cc, gpt := claude(home), chatgpt()
	cc.conn = gateway.Connected
	f := newFixture(t, cc, modelsAgent{gpt})
	snapshot(t, f.tt, "settings-agents")
	f.has(t, "Use", "2.1.296 in ~/.local/bin", "Codex 0.162 in ChatGPT.app",
		"Ask uses these agents in Capy, connected or not.")
	// The agents and their connections come first, then what Ask asks.
	connect, okConnect := f.tt.Find("Connect… ChatGPT")
	if ask, ok := f.tt.Find("Use"); !ok || !okConnect || connect.Y > ask.Y {
		t.Errorf("Connect… at %+v, Use at %+v; want the agents first", connect, ask)
	}
	if !f.shows("Connected") {
		t.Error("the connected agent does not say so")
	}
	if _, ok := f.tt.Find("Disconnect Claude Code"); !ok {
		t.Error("a connected agent offers no Disconnect")
	}
	if _, ok := f.tt.Find("Connect… ChatGPT"); !ok {
		t.Error("an agent not connected offers no Connect…")
	}
	// Only the agent chosen to ask, ChatGPT, has a Model row.
	if f.tt.HasText("From Codex settings") {
		t.Fatal("a Model row for Claude Code, which offers no models")
	}
	f.pick(t, "Use", 1) // ChatGPT, after Claude Code
	if got := f.prefs.Get().Agent; got != "codex" {
		t.Fatalf("Use saved %q, want codex", got)
	}
	f.has(t, "Model", "From Codex settings")
	f.pick(t, "Model", 2) // after From Codex settings and gpt-6.1-sol
	if got := f.prefs.Get().Models["codex"]; got != "gpt-6.1-mini" {
		t.Fatalf("saved model %q", got)
	}
}

func TestSettingsPreserveTheAgentChosenInAsk(t *testing.T) {
	f := newFixture(t, claude("/Users/ana"), chatgpt())
	f.h.settle(t)
	f.tt.Frame() // Settings remembers Claude Code before Ask changes it.
	footer := ui.NewTester(func(c *ui.Context) { f.a.panel.footer(c, "") }, 320, 60)
	if err := footer.Click("Agent"); err != nil {
		t.Fatal(err)
	}
	if err := footer.ChooseMenuItem("ChatGPT"); err != nil {
		t.Fatal(err)
	}
	if got := f.prefs.Get().Agent; got != "codex" {
		t.Fatalf("Ask chose %q, want codex", got)
	}
	f.tt.Frame() // Opening Settings must reflect Ask's choice, not overwrite it.
	if got := f.prefs.Get().Agent; got != "codex" || f.a.settings.askWith != "ChatGPT" {
		t.Fatalf("Settings chose %q and shows %q after Ask chose ChatGPT", got, f.a.settings.askWith)
	}
	// The Settings control can still choose Claude Code.
	f.field(t, "Use")
	f.tt.Key(0, ui.KeyUp)
	f.tt.Key(0, ui.KeyEnter)
	f.h.settle(t)
	f.tt.Frame()
	if got := f.prefs.Get().Agent; got != "claude-code" {
		t.Fatalf("Settings chose %q, want claude-code", got)
	}
}

func TestAnOpenThreadFollowsAndSwitchesTheAgentChosen(t *testing.T) {
	f := newFixture(t, claude("/Users/ana"), chatgpt())
	footer := ui.NewTester(func(c *ui.Context) { f.a.panel.footer(c, "open-thread") }, 320, 60)
	if !footer.HasText("Claude Code") {
		t.Fatalf("footer: texts %q", footer.Texts())
	}
	// Settings' choice reaches the open thread's next question, and every
	// window is drawn anew for it: the caption bar's panel is another.
	f.field(t, "Use")
	f.tt.Key(0, ui.KeyDown)
	f.tt.Key(0, ui.KeyEnter)
	f.tt.Frame()
	if len(f.h.q) == 0 {
		t.Fatal("choosing in Settings redraws no other window")
	}
	f.h.settle(t)
	footer.Frame()
	if f.prefs.Get().Agent != "codex" || !footer.HasText("ChatGPT") {
		t.Fatalf("after Settings chose ChatGPT: saved %q, footer %q", f.prefs.Get().Agent, footer.Texts())
	}
	// The open thread's own pop-up switches it back.
	if err := footer.Click("Agent"); err != nil {
		t.Fatal(err)
	}
	if err := footer.ChooseMenuItem("Claude Code"); err != nil {
		t.Fatalf("%v; menu %q", err, footer.Menu())
	}
	if got := f.prefs.Get().Agent; got != "claude-code" {
		t.Fatalf("the thread's pop-up chose %q, want claude-code", got)
	}
}

func TestConnectShowsTheExactCommandThenConnects(t *testing.T) {
	gpt := chatgpt()
	f := newFixture(t, gpt)
	f.click(t, "Connect… ChatGPT")
	snapshot(t, f.tt, "settings-agents-connect")
	f.has(t, "Connect ChatGPT to Capy?",
		"Capy will run this command. ChatGPT, the Codex CLI and Codex in your editor will be able to read every session in Capy.",
		"codex mcp add capy -- '/Applications/Capy beta.app/Contents/MacOS/Capy' mcp")
	if len(gpt.calls()) != 0 {
		t.Fatal("the alert ran the command before Connect")
	}
	f.click(t, "Connect")
	if !slices.Equal(gpt.calls(), []string{"connect"}) || !f.shows("Connected") {
		t.Fatalf("after Connect: ran %v, texts %q", gpt.calls(), f.tt.Texts())
	}
	// Disconnect asks too.
	f.click(t, "Disconnect ChatGPT")
	f.has(t, "Disconnect ChatGPT?", "ChatGPT will no longer see your sessions.", "codex mcp remove capy")
	f.confirm(t) // the alert's Disconnect: its default button
	if !slices.Equal(gpt.calls(), []string{"connect", "disconnect"}) || f.shows("Connected") {
		t.Fatalf("after Disconnect: ran %v, texts %q", gpt.calls(), f.tt.Texts())
	}
}

func TestConnectionRowStates(t *testing.T) {
	gpt := chatgpt()
	gpt.conn = gateway.Moved
	f := newFixture(t, gpt)
	snapshot(t, f.tt, "settings-agents-moved")
	f.has(t, "Capy has moved", "Capy is no longer where you connected it from")
	if n := slices.Index(f.tt.Texts(), "Reconnect"); n < 0 || slices.Contains(f.tt.Texts()[n+1:], "Reconnect") {
		t.Fatalf("want one Reconnect; texts %q", f.tt.Texts())
	}
	f.click(t, "Reconnect ChatGPT")
	// Both commands it runs show before either runs.
	f.has(t, "Capy will run these commands.", "codex mcp remove capy\ncodex mcp add capy --")
	f.click(t, "Connect")
	// The CLI records the running Capy: the old entry goes first.
	if !slices.Equal(gpt.calls(), []string{"disconnect", "connect"}) || !f.shows("Connected") {
		t.Fatalf("Reconnect ran %v; texts %q", gpt.calls(), f.tt.Texts())
	}
	// A failed command says how it exited and changes nothing else.
	gpt.fail = exitErr(1)
	f.click(t, "Disconnect ChatGPT")
	f.confirm(t)
	f.has(t, "Couldn’t disconnect (exit 1)")
	gpt.fail = nil
	f.click(t, "Try Again ChatGPT")
	if f.tt.HasText("Couldn’t") || f.shows("Connected") {
		t.Fatalf("Try Again: texts %q", f.tt.Texts())
	}
	gpt.fail = errors.Join(errors.New("boom"), exitErr(2))
	f.click(t, "Connect… ChatGPT")
	f.click(t, "Connect")
	f.has(t, "Couldn’t connect (exit 2)")
}

func TestNoAgentFoundExplainsAndLooksAgain(t *testing.T) {
	cc, gpt := claude("/Users/ana"), chatgpt()
	cc.probe = gateway.Probe{Found: true, Path: "/usr/local/bin/claude", Version: "2.0.4", TooOld: "2.1.0"}
	gpt.probe = gateway.Probe{}
	f := newFixture(t, cc, gpt)
	snapshot(t, f.tt, "settings-agents-none")
	f.has(t, "No agent found", "Claude Code 2.0.4 is too old for Ask. Update to 2.1 or later.", "Installed somewhere else?")
	if f.shows("Ask and Polish") || f.tt.HasText("Use") || f.tt.HasText("Connect…") {
		t.Fatalf("no agent, yet Ask or Connect: texts %q", f.tt.Texts())
	}
	f.click(t, "Get ChatGPT")
	if urls := f.tt.OpenedURLs(); !slices.Equal(urls, []string{"https://chatgpt.com/download"}) {
		t.Fatalf("opened %v", urls)
	}
	// Choose… keeps the program as that agent's path, tried first, and
	// looks again.
	f.chose = "/opt/tools/bin/claude"
	before := cc.probes
	f.click(t, "Choose…")
	if got := f.prefs.Get().Paths["claude-code"]; got != "/opt/tools/bin/claude" || cc.probes == before {
		t.Fatalf("Choose…: saved %q, probes %d → %d", got, before, cc.probes)
	}
	f.chose = "/opt/tools/bin/node"
	f.click(t, "Choose…")
	f.has(t, "Choose the claude or codex program.")
	// Look Again finds an agent installed meanwhile.
	gpt.probe = chatgpt().probe
	f.click(t, "Look Again")
	if !f.tt.HasText("Use") || f.tt.HasText("No agent found") {
		t.Fatalf("after Look Again: texts %q", f.tt.Texts())
	}
}

func TestSettingsAskWithOneAgentHasNothingToChoose(t *testing.T) {
	f := newFixture(t, claude("/Users/ana"))
	f.field(t, "Use")
	if menu := f.tt.Menu(); len(menu) != 0 || !f.tt.HasText("Claude Code") {
		t.Fatalf("one agent: menu %q, texts %q; want its name and no pop-up", menu, f.tt.Texts())
	}
}

func TestAgentOffShowsNothingAndLooksForNothing(t *testing.T) {
	cc := claude("/Users/ana")
	f := &fixture{h: host{q: make(chan func(), 16)}, g: &gate{}, prefs: prefs.Load(filepath.Join(t.TempDir(), "agent.json"))}
	f.a = New(Options{Gate: f.g, Gateway: gateway.New(gateway.Options{}, cc), Prefs: f.prefs})
	f.a.Start(f.h)
	tt := ui.NewTester(func(c *ui.Context) {
		ui.Column(c).Fill().Children(func() {
			ui.Text(c, "Captions")
			f.a.Settings(c)
			f.a.Toolbar(c, nil)
			f.a.CaptionHeader(c, nil)
		})
	}, 560, 400)
	f.h.settle(t)
	tt.Frame()
	if texts := tt.Texts(); !slices.Equal(texts, []string{"Captions"}) || cc.probes != 0 {
		t.Fatalf("Agent off: texts %q, %d probes; want nothing of Agent and no discovery", texts, cc.probes)
	}
	if f.a.Inspector(nil, nil) != nil || f.a.CaptionPanel(nil, nil) != nil || f.a.Export(addonSession{}) != nil {
		t.Fatal("a slot filled with Agent off")
	}
}

type addonSession = addon.Session

// snapshot saves the tester's frame as a PNG when $CAPY_SNAPSHOT_DIR is
// set, to review the look.
func snapshot(t *testing.T, tt *ui.Tester, name string) {
	t.Helper()
	dir := os.Getenv("CAPY_SNAPSHOT_DIR")
	if dir == "" {
		return
	}
	tt.Frame()
	f, err := os.Create(filepath.Join(dir, name+".png"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, tt.Image()); err != nil {
		t.Fatal(err)
	}
}

// TestSettingsReadConnectionsAnewWhenShownAgain shows a Disconnect made in
// Terminal meanwhile, once Settings shows again.
func TestSettingsReadConnectionsAnewWhenShownAgain(t *testing.T) {
	gpt := chatgpt()
	gpt.conn = gateway.Connected
	f := newFixture(t, gpt)
	if !f.shows("Connected") {
		t.Fatalf("texts %q", f.tt.Texts())
	}
	gpt.mu.Lock()
	gpt.conn = gateway.NotConnected // codex mcp remove capy, in Terminal
	gpt.mu.Unlock()
	f.tt.Frame()
	f.h.settle(t)
	if !f.shows("Connected") {
		t.Fatal("read again while shown: it should wait for a gap")
	}
	f.a.settings.seen = time.Now().Add(-time.Minute) // closed, and opened again
	f.tt.Frame()
	f.h.settle(t)
	f.tt.Frame()
	if f.shows("Connected") || !f.shows("Connect… ChatGPT") {
		t.Fatalf("after Settings showed again: texts %q", f.tt.Texts())
	}
}
