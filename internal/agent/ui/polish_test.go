package ui

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/polish"
	"github.com/onyx-dot-app/Capy/internal/agent/prefs"
	"github.com/onyx-dot-app/Capy/internal/features"
	"github.com/onyx-dot-app/Capy/internal/profile"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/store/sqlite"
)

// features is a gate of the features turned on in it.
type featureSet map[features.Feature]bool

func (g featureSet) Enabled(f features.Feature) bool { return g[f] }

// toastHost is the test's host, keeping the toasts shown.
type toastHost struct {
	host
	mu     *sync.Mutex
	toasts *[]ui.Toast
}

func (h toastHost) Toast(t ui.Toast) {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.toasts = append(*h.toasts, t)
}

type polishFixture struct {
	a      *AddOn
	h      toastHost
	p      *polish.Polish
	prefs  *prefs.File
	lib    *sqlite.DB
	gate   featureSet
	tt     *ui.Tester
	toasts []ui.Toast
	mu     sync.Mutex
}

// sends is claude as it sends what it reads to Anthropic.
func sends(a *testAgent) *testAgent {
	a.info.Sends = &gateway.Recipient{Provider: "Anthropic", Account: "your Claude account"}
	return a
}

// newPolishFixture is Settings › Highlights' add-on rows with agents
// found, Agent and Highlights on.
func newPolishFixture(t *testing.T, agents ...gateway.Agent) *polishFixture {
	t.Helper()
	lib, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "capy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lib.Close() })
	f := &polishFixture{prefs: prefs.Load(filepath.Join(t.TempDir(), "agent.json")), lib: lib,
		gate: featureSet{features.Agent: true, features.Highlights: true}}
	f.h = toastHost{host: host{q: make(chan func(), 16)}, mu: &f.mu, toasts: &f.toasts}
	gw := gateway.New(gateway.Options{}, agents...)
	t.Cleanup(gw.Close)
	f.p = polish.New(polish.Options{Store: lib, Gateway: gw, Gap: time.Millisecond,
		Agent:   func() string { return "claude-code" },
		On:      func() bool { return f.a.PolishOn("claude-code") },
		Profile: func() profile.Profile { return profile.Profile{} },
		Stopped: func(s polish.Stop) { f.a.PolishStopped(s) }})
	t.Cleanup(f.p.Close)
	f.a = New(Options{Gate: f.gate, Gateway: gw, Polish: f.p, Prefs: f.prefs})
	f.a.Start(f.h)
	f.tt = ui.NewTester(func(c *ui.Context) {
		ui.Column(c).Fill().Padding(20, 24).AlignItems(ui.Stretch).Children(func() { f.a.HighlightsSettings(c) })
	}, 560, 400)
	f.h.settle(t)
	f.tt.Frame()
	return f
}

// flip clicks Polish with agent's switch, right of its row.
func (f *polishFixture) flip(t *testing.T) {
	t.Helper()
	r, ok := f.tt.Find("Polish with agent")
	if !ok {
		t.Fatalf("no Polish with agent; texts %q", f.tt.Texts())
	}
	f.tt.ClickAt(560-24-12-18, r.Y+r.H)
	f.h.settle(t)
	f.tt.Frame()
}

func (f *polishFixture) confirm(t *testing.T, key ui.Key) {
	t.Helper()
	f.tt.Key(0, key)
	f.h.settle(t)
	f.tt.Frame()
}

func TestPolishRowAsksConsentOnceThenTurnsOn(t *testing.T) {
	cc := sends(claude("/Users/ana"))
	f := newPolishFixture(t, cc)
	if !f.tt.HasText("Claude Code writes a sentence for each highlight, and finds debates. The turns around each highlight go to Anthropic through your Claude account.") {
		t.Fatalf("the caption does not name the agent and where it sends; texts %q", f.tt.Texts())
	}
	// Cancel leaves it off, and allows nothing.
	f.flip(t)
	if !f.tt.HasText("Polish with Claude Code?") {
		t.Fatalf("no consent asked; texts %q", f.tt.Texts())
	}
	f.confirm(t, ui.KeyEscape)
	if p := f.prefs.Get(); p.Polish || f.a.o.Gateway.Allowed("claude-code") || f.tt.HasText("Polish with Claude Code?") {
		t.Fatalf("after Cancel: %+v", p)
	}
	// Allow turns it on, and is asked once.
	f.flip(t)
	f.confirm(t, ui.KeyEnter)
	if p := f.prefs.Get(); !p.Polish || !f.a.o.Gateway.Allowed("claude-code") || !f.a.PolishOn("claude-code") {
		t.Fatalf("after Allow: %+v", p)
	}
	f.flip(t)
	f.flip(t)
	if f.tt.HasText("Polish with Claude Code?") || !f.prefs.Get().Polish {
		t.Fatal("consent asked again, or the switch did not turn back on")
	}
}

func TestPolishRowNeedsAnAgentAndBothFeatures(t *testing.T) {
	missing := sends(claude("/Users/ana"))
	missing.probe = gateway.Probe{}
	f := newPolishFixture(t, missing)
	if !f.tt.HasText("Polish needs an agent. Set one up in Agents below.") {
		t.Fatalf("texts %q", f.tt.Texts())
	}
	f.flip(t)
	if f.prefs.Get().Polish || f.tt.HasText("Polish with Claude Code?") {
		t.Fatal("a switch with no agent turned on")
	}
	// Turned on before the agent went missing, it names it.
	if err := f.prefs.Update(func(p *prefs.Prefs) { p.Polish, p.Agent = true, "claude-code" }); err != nil {
		t.Fatal(err)
	}
	f.tt.Frame()
	if !f.tt.HasText("Claude Code isn’t found. Choose another in Agents below.") || f.a.PolishOn("claude-code") {
		t.Fatalf("texts %q", f.tt.Texts())
	}
	for _, off := range []features.Feature{features.Agent, features.Highlights} {
		f.gate[off] = false
		f.tt.Frame()
		if f.tt.HasText("Polish with agent") {
			t.Errorf("the row shows with %s off", off)
		}
		f.gate[off] = true
	}
}

// signedOut is claude, signed out: every run fails so.
type signedOut struct{ *testAgent }

func (signedOut) Run(context.Context, gateway.Query) (gateway.Run, error) {
	r := &endedRun{events: make(chan gateway.Event, 1)}
	r.events <- gateway.Failed{Kind: gateway.SignedOut, Code: 1}
	close(r.events)
	return r, nil
}

type endedRun struct{ events chan gateway.Event }

func (r *endedRun) Events() <-chan gateway.Event { return r.events }
func (r *endedRun) Stop()                        {}

func TestPolishStoppedToastsOnceAndOffersTryAgain(t *testing.T) {
	cc := sends(claude("/Users/ana"))
	f := newPolishFixture(t, signedOut{cc})
	if err := f.prefs.Update(func(p *prefs.Prefs) { p.Polish = true }); err != nil {
		t.Fatal(err)
	}
	if err := f.a.o.Gateway.Allow("claude-code"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id := uuid.NewString()
	if err := f.lib.CreateSession(ctx, store.Session{ID: id, Mode: store.ModeCaptions, Source: store.SourceMic, LangA: "en-US", LangB: "zh-CN", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := f.lib.AppendSegment(ctx, store.Segment{ID: uuid.NewString(), SessionID: id, Lang: "en-US", Text: "We ship Monday.", EndMs: 900}); err != nil {
		t.Fatal(err)
	}
	if err := f.lib.AddHighlights(ctx, []store.Highlight{{ID: uuid.NewString(), SessionID: id, Kind: store.KindDecision, Confidence: 0.9, CreatedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	f.a.Highlighted(id, false)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := f.p.LastStop(); ok || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.h.settle(t)
	f.tt.Frame()
	f.mu.Lock()
	toasts := slices.Clone(f.toasts)
	f.mu.Unlock()
	if len(toasts) != 1 || !strings.Contains(toasts[0].Description, "Sign in to Claude Code") {
		t.Fatalf("toasts %+v, want one saying to sign in", toasts)
	}
	if !f.tt.HasText("Sign in to Claude Code, then try again.") {
		t.Fatalf("Settings does not say why; texts %q", f.tt.Texts())
	}
	// Try Again runs the final pass: still signed out, it stops again.
	if err := f.tt.Click("Try Again"); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		f.h.settle(t)
		f.mu.Lock()
		n := len(f.toasts)
		f.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d toasts: Try Again ran no pass", n)
		}
	}
}

func TestPolishRowOffForAnAgentChosenWithoutConsent(t *testing.T) {
	cc := sends(claude("/Users/ana"))
	f := newPolishFixture(t, cc)
	// On for another agent, then Claude Code chosen, never allowed.
	if err := f.prefs.Update(func(p *prefs.Prefs) { p.Polish, p.Agent = true, "claude-code" }); err != nil {
		t.Fatal(err)
	}
	f.tt.Frame()
	if !f.tt.HasText("Turn this on to allow Claude Code to polish highlights.") || f.a.PolishOn("claude-code") {
		t.Fatalf("texts %q; want it off, saying how to allow it", f.tt.Texts())
	}
	f.flip(t)
	if !f.tt.HasText("Polish with Claude Code?") {
		t.Fatalf("turning it on asked no consent; texts %q", f.tt.Texts())
	}
	f.confirm(t, ui.KeyEnter)
	if !f.a.PolishOn("claude-code") {
		t.Fatal("not on after Allow")
	}
}
