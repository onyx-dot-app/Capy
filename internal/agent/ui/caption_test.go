package ui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/fake"
	"github.com/onyx-dot-app/Capy/internal/agent/prefs"
	"github.com/onyx-dot-app/Capy/internal/views/addon"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
)

// barFixture is the inspector's fixture with the caption bar beside it:
// its header's slot and its panel's, while f.h.live records.
type barFixture struct {
	*inspectorFixture
	bar  *ui.Tester
	pane *addon.Pane // the panel the bar last showed
}

func newBar(t *testing.T, agent named) *barFixture {
	t.Helper()
	f := &barFixture{inspectorFixture: newInspector(t, agent, gateway.Options{})}
	f.h.live = &addon.Session{ID: f.session.ID, Live: true}
	f.bar = ui.NewTester(func(c *ui.Context) {
		kit.BarTheme(c)
		ui.Row(c).Fill().AlignItems(ui.Stretch).Background(kit.BarTint).Children(func() {
			ui.Row(c).Grow(1).Height(40).Padding(0, 12).AlignItems(ui.Center).Justify(ui.End).Children(func() {
				f.a.CaptionHeader(c, f.h.live)
			})
			f.pane = f.a.CaptionPanel(c, f.h.live)
			if f.pane != nil {
				ui.Column(c).Width(f.pane.Width).AlignItems(ui.Stretch).Children(f.pane.View)
			}
		})
	}, 900, 400)
	f.settleBar(t)
	return f
}

// settleBar runs what was posted, drawing both windows, until it rests.
func (f *barFixture) settleBar(t *testing.T) {
	t.Helper()
	for range 2 {
		f.bar.Frame()
		f.settle(t)
	}
	f.bar.Frame()
}

func (f *barFixture) clickBar(t *testing.T, label string) {
	t.Helper()
	if err := f.bar.Click(label); err != nil {
		t.Fatalf("%v; texts %q", err, f.bar.Texts())
	}
	f.settleBar(t)
}

func (f *barFixture) barHas(t *testing.T, texts ...string) {
	t.Helper()
	for _, s := range texts {
		if !f.bar.HasText(s) {
			t.Errorf("the caption bar lacks %q; texts %q", s, f.bar.Texts())
		}
	}
}

func TestTheAskPillOpensAskAboutTheLiveSession(t *testing.T) {
	f := newBar(t, claudeLike(fake.Script{}, false))
	if f.bar.HasText("This session") {
		t.Fatal("the panel shows before the pill is pressed")
	}
	f.clickBar(t, "Ask")
	f.barHas(t, "This session", "Live", "Ask about this session",
		"Claude Code reads what’s been said so far. The sentence being spoken now isn’t included yet.",
		"Catch me up", "Action items so far", "Was I mentioned?")
	if f.bar.HasText("Scope") {
		t.Fatal("the live scope can be changed")
	}
	snapshot(t, f.bar, "q1.9-live")
	f.clickBar(t, "Ask")
	if f.bar.HasText("Ask about this session") {
		t.Fatal("the pill did not close the panel")
	}
	// The panel goes with the recording.
	f.clickBar(t, "Ask")
	f.h.live = nil
	f.settleBar(t)
	if f.a.liveOpen || f.bar.HasText("This session") {
		t.Fatal("the panel stayed open after the recording stopped")
	}
}

// TestCatchMeUpStartsWhereThePanelLastClosed covers what was said since
// the panel closed in this session.
func TestCatchMeUpStartsWhereThePanelLastClosed(t *testing.T) {
	f := newBar(t, claudeLike(fake.Script{Calls: []fake.Call{{Tool: "get_live_session", Args: map[string]any{}}}, Answer: "Pricing came up [00:31]."}, false))
	started, err := f.lib.GetSession(context.Background(), f.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.clickBar(t, "Ask")
	f.clickBar(t, "Ask") // closed now: Catch me up starts here
	closedAt := time.Since(started.StartedAt)
	f.clickBar(t, "Ask")
	f.clickBar(t, "Catch me up")
	f.barHas(t, "Catch me up", "Read the live session", "Pricing came up ")
	qs := f.agent.Queries()
	if len(qs) != 1 {
		t.Fatalf("%d runs", len(qs))
	}
	var ms int64
	if _, err := fmt.Sscanf(qs[0].Prompt[strings.LastIndex(qs[0].Prompt, "since_ms "):], "since_ms %d", &ms); err != nil {
		t.Fatalf("no since_ms:\n%s", qs[0].Prompt)
	}
	if want := closedAt.Milliseconds() - 5000; ms < want-3000 || ms > want+1000 { // a few seconds before the close
		t.Fatalf("since_ms %d, want about %d: when the panel closed", ms, want)
	}
	snapshot(t, f.bar, "q1.10-live-answer")
	// A citation opens the session's Transcript in Capy's window.
	f.clickBar(t, "00:31") // the citation's link
	if len(f.h.shown) != 1 || f.h.shown[0] != f.session.ID+"@31s" {
		t.Fatalf("shown %v", f.h.shown)
	}
}

func TestCmdOptKOpensThePanelFromTheCapsuleAndEscClosesIt(t *testing.T) {
	f := newBar(t, claudeLike(fake.Script{}, false))
	press := f.h.keys[liveShortcut]
	if press == nil {
		t.Fatal("⌥⌘K is not registered")
	}
	f.h.capsule = true
	press()
	f.settleBar(t)
	if !f.a.liveOpen || f.h.capsule {
		t.Fatalf("⌥⌘K from the capsule: open %v, capsule %v; want the bar expanded with the panel open", f.a.liveOpen, f.h.capsule)
	}
	f.barHas(t, "Ask about this session")
	press()
	f.settleBar(t)
	if f.a.liveOpen {
		t.Fatal("⌥⌘K did not close the panel")
	}
	press()
	f.settleBar(t)
	if err := f.bar.Click("Question"); err != nil {
		t.Fatal(err)
	}
	f.bar.Key(0, ui.KeyEscape)
	f.settleBar(t)
	if f.a.liveOpen {
		t.Fatal("Esc did not close the panel")
	}
	// With no recording, ⌥⌘K does nothing.
	f.h.live = nil
	press()
	f.settleBar(t)
	if f.a.liveOpen {
		t.Fatal("⌥⌘K opened Ask with nothing recording")
	}
}

// TestALiveAnswerFinishesInTheInspector keeps the live thread the
// session's: once recording stops, the inspector shows it, and an answer
// still running finishes there.
func TestALiveAnswerFinishesInTheInspector(t *testing.T) {
	f := newBar(t, claudeLike(fake.Script{Calls: readIt, Stall: true}, false))
	f.clickBar(t, "Ask")
	if err := f.bar.Click("Question"); err != nil {
		t.Fatal(err)
	}
	f.bar.Type("What are the action items so far?")
	f.bar.Key(0, ui.KeyEnter)
	f.settleBar(t)
	f.barHas(t, "What are the action items so far?", "Stop")

	f.h.live = nil // the recording stops
	f.settleBar(t)
	f.has(t, "What are the action items so far?", "Stop")
	id := f.a.data.open["session:"+f.session.ID]
	if id == "" {
		t.Fatal("the live thread is not the session's")
	}
	f.click(t, "Stop") // the inspector's
	f.settleBar(t)
	f.has(t, "The answer stopped early.")
}

// TestCatchMeUpThroughConsentStillCatchesUp keeps its starting point on
// the first Catch me up with an agent, which asks consent and resends.
func TestCatchMeUpThroughConsentStillCatchesUp(t *testing.T) {
	f := newBar(t, claudeLike(fake.Script{Answer: "Pricing."}, true))
	f.clickBar(t, "Ask")
	if err := f.bar.Click("Question"); err != nil {
		t.Fatal(err)
	}
	f.bar.Key(0, ui.KeyEscape) // closed with Esc: Catch me up starts here
	f.settleBar(t)
	if f.a.liveOpen {
		t.Fatal("Esc did not close the panel")
	}
	f.clickBar(t, "Ask")
	f.clickBar(t, "Catch me up")
	f.barHas(t, "Ask with Claude Code?")
	f.clickBar(t, "Allow")
	qs := f.agent.Queries()
	if len(qs) != 1 || !strings.Contains(qs[0].Prompt, "since_ms ") || !strings.Contains(qs[0].Prompt, "get_live_session") {
		t.Fatalf("Catch me up after Allow lost its hint: %+v", qs)
	}
}

func TestEscStopsALiveAnswerAndKeepsThePanel(t *testing.T) {
	f := newBar(t, claudeLike(fake.Script{Calls: readIt, Stall: true}, false))
	f.clickBar(t, "Ask")
	if err := f.bar.Click("Question"); err != nil {
		t.Fatal(err)
	}
	f.bar.Type("Action items so far?")
	f.bar.Key(0, ui.KeyEnter)
	f.settleBar(t)
	f.bar.Key(0, ui.KeyEscape)
	f.settleBar(t)
	if !f.a.liveOpen {
		t.Fatal("Esc during an answer closed the panel")
	}
	f.barHas(t, "The answer stopped early.")
}

func TestThePanelsWidthIsItsOwnAndSaved(t *testing.T) {
	f := newBar(t, claudeLike(fake.Script{}, false))
	f.clickBar(t, "Ask")
	if f.pane == nil || f.pane.Resized == nil || f.pane.MaxWidth < 600 {
		t.Fatalf("panel %+v; want it resizable to 600", f.pane)
	}
	f.pane.Resized(600)
	f.settleBar(t)
	if f.pane.Width != 600 {
		t.Fatalf("panel %v wide after a drag to 600", f.pane.Width)
	}
	if w := f.a.width(&f.a.inspectorSize); w != prefs.InspectorWidth {
		t.Fatalf("the inspector took the panel's width: %v", w)
	}
	deadline := time.Now().Add(5 * time.Second)
	for p := f.prefs.Get(); p.LivePanelWidth != 600 || p.InspectorWidth != prefs.InspectorWidth; p = f.prefs.Get() {
		if time.Now().After(deadline) {
			t.Fatalf("saved panel %v, inspector %v; want 600, %v", p.LivePanelWidth, p.InspectorWidth, prefs.InspectorWidth)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
