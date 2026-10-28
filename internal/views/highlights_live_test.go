package views

import (
	"fmt"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/features"
	"github.com/onyx-dot-app/Capy/internal/profile"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

var hlCfg = func() session.Config {
	c := captionsCfg
	c.Highlights = session.HighlightConfig{On: true, Kinds: store.HighlightKinds, Threshold: session.ThresholdFewer}
	return c
}()

func mark(id string, seq int, k store.HighlightKind, p float64, at time.Time) store.Highlight {
	return store.Highlight{ID: id, SessionID: "s1", Seq: seq, Kind: k, Confidence: p, CreatedAt: at}
}

func TestAttentionNudgesOnceForWhatIsSurelyForYou(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 11, 0, 0, 0, time.UTC)
	snap := session.Snapshot{Config: hlCfg}
	var a attention
	add := func(at time.Duration, hs ...store.Highlight) {
		snap.Highlights = append(snap.Highlights, hs...)
		a.follow(t0.Add(at), snap, true)
	}
	// θ is 0.85: a mark for the user needs 0.90 to be one, 0.95 to nudge.
	add(0, mark("a", 1, store.KindAskedYou, 0.93, t0), mark("d", 2, store.KindDecision, 0.99, t0))
	if a.pill != nil || a.badge || a.edge != "" {
		t.Fatalf("nudged for %+v: under θ+0.1, or not for the user", a)
	}
	add(time.Minute, mark("b", 5, store.KindAskedYou, 0.96, t0))
	if len(a.pill) != 1 || a.pill[0] != "b" || !a.glows || !a.badge || a.edge != "b" {
		t.Fatalf("after a sure one: %+v; want the pill, the glow, the badge and the edge", a)
	}
	// Within 2 min, the next joins the pill without a glow of its own.
	add(2*time.Minute, mark("c", 9, store.KindMentionedYou, 0.97, t0))
	if len(a.pill) != 2 || !a.since.Equal(t0.Add(time.Minute)) || a.edge != "c" {
		t.Fatalf("within 2 min: %+v; want it to join the pill", a)
	}
	// Seen once, a highlight never nudges again, and the pill folds after
	// 3 min.
	add(4 * time.Minute)
	if a.pill != nil {
		t.Fatalf("after 3 min: pill %v", a.pill)
	}
	add(7*time.Minute, mark("e", 12, store.KindAskedYou, 0.98, t0))
	if len(a.pill) != 1 || !a.glows {
		t.Fatalf("2 min after the last: %+v; want a new pill, glowing", a)
	}
	// With When something's for me off, nothing nudges.
	var off attention
	off.follow(t0, session.Snapshot{Config: hlCfg, Highlights: []store.Highlight{mark("x", 1, store.KindAskedYou, 1, t0)}}, false)
	if off.pill != nil || off.badge {
		t.Fatalf("nudge off: %+v", off)
	}
}

func liveHighlights(a *App, lines int, hs ...store.Highlight) *captionBar {
	snap := session.Snapshot{SessionID: "s1", Config: hlCfg, Status: session.StatusLive, StartedAt: time.Now(), Highlights: hs}
	for i := range lines {
		snap.Lines = append(snap.Lines, session.Line{Seq: i, StartMs: int64(i) * 10_000, Lang: "en-US", Text: fmt.Sprintf("Line %d.", i),
			Speaker: "Speaker 1"})
	}
	b := &captionBar{app: a, cfg: hlCfg, sessionSlot: sessionSlot{run: &fakeRun{snap: snap}}}
	a.captions = b
	return b
}

func TestCaptionBarMarksLinesAndOffersTheLineForYou(t *testing.T) {
	a := testApp(t)
	tt := ui.NewTester(func(c *ui.Context) {}, barWidth, barHeight)
	now := tt.Now()
	b := liveHighlights(a, 6, mark("a", 4, store.KindAskedYou, 0.97, now.Add(-time.Second)), mark("d", 5, store.KindDecision, 0.9, now))
	tt = ui.NewTester(b.view, barWidth, barHeight)
	tt.Advance(time.Second) // mid-glow
	tt.Frame()
	snapshot(t, tt, "captions-highlights")
	for _, want := range []string{"Asked you · 00:40", "2 highlights", "Decision"} {
		if !tt.HasText(want) {
			t.Errorf("bar lacks %q; texts %q", want, tt.Texts())
		}
	}
	img := tt.Image()
	scale := img.Bounds().Dx() / barWidth
	if px := img.RGBAAt(barWidth/2*scale, 0); px.R < px.B+60 {
		t.Errorf("top edge %+v mid-glow: want terracotta", px)
	}
	// The glow is one breath: then the bar is as before.
	tt.Advance(glowFor)
	tt.Frame()
	if px := tt.Image().RGBAAt(barWidth/2*scale, 0); px.R > px.B+60 {
		t.Errorf("top edge %+v after the glow", px)
	}
	if err := tt.Click("Dismiss"); err != nil { // the pill's ×
		t.Fatal(err)
	}
	if tt.HasText("Asked you · 00:40") || !tt.HasText("2 highlights") {
		t.Fatalf("after ×: texts %q; want the pill gone, the count left", tt.Texts())
	}
	// Narrow, the count stands in for the pill, tinted.
	b.att.pill = []string{"a"}
	tt.SetSize(barPillMin-20, barHeight)
	tt.Frame()
	if tt.HasText("Asked you · 00:40") || !tt.HasText("2 highlights") {
		t.Fatalf("narrow: texts %q", tt.Texts())
	}
}

func TestCapsuleBadgeShowsUntilTheBarIs(t *testing.T) {
	a := testApp(t)
	b := liveHighlights(a, 3)
	b.minimized = true
	tt := ui.NewTester(b.capsuleView, capsuleWidth, capsuleHeight)
	if tt.HasText("For you") {
		t.Fatal("a badge with nothing for the user")
	}
	run := b.run.(*fakeRun)
	run.snap.Highlights = []store.Highlight{mark("a", 1, store.KindAskedYou, 0.97, tt.Now())}
	tt.Frame()
	snapshot(t, tt, "capsule-for-you")
	if !tt.HasText("For you") {
		t.Fatalf("capsule: texts %q", tt.Texts())
	}
	// The bar shown, it is seen.
	b.minimized = false
	bar := ui.NewTester(b.view, barWidth, barHeight)
	bar.Frame()
	if b.att.badge {
		t.Fatal("the badge outlived the bar showing")
	}
}

func TestWindowPointsAtALineForYouOutOfView(t *testing.T) {
	a := testApp(t)
	tt := ui.NewTester(func(c *ui.Context) {}, 1040, 700)
	b := liveHighlights(a, 40)
	summary := store.SessionSummary{ID: "s1", Mode: store.ModeCaptions, LangA: "en-US", LangB: "ja-JP", StartedAt: time.Now()}
	w := &mainWindow{app: a, split: sidebarWidth, loaded: true, sessions: []store.SessionSummary{summary}, selected: "s1",
		detail: &store.SessionDetail{SessionSummary: summary}}
	tt = ui.NewTester(w.view, 1040, 700)
	tt.Frame()
	run := b.run.(*fakeRun)
	run.snap.Highlights = []store.Highlight{mark("a", 3, store.KindAskedYou, 0.97, tt.Now())}
	tt.Frame()
	tt.Frame()
	snapshot(t, tt, "window-edge-pill")
	pill := "Asked you · Speaker 1 · just now"
	if !tt.HasText(pill) {
		t.Fatalf("no edge pill; texts %q", tt.Texts())
	}
	if err := tt.Click(pill); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if tt.HasText(pill) || w.flash.seq != 3 || !visible(tt, "Line 3.", 700) {
		t.Fatalf("after the pill: flash %+v, Line 3 shown %v; texts %q", w.flash, visible(tt, "Line 3.", 700), tt.Texts())
	}
	// Scrolled up, what is highlighted meanwhile waits in a ribbon.
	run.snap.Highlights = append(run.snap.Highlights, mark("d", 38, store.KindDecision, 0.9, tt.Now().Add(time.Second)))
	tt.Advance(time.Second)
	tt.Frame()
	tt.Frame()
	if !tt.HasText("1 highlight") || !tt.HasText(" since you scrolled up") {
		t.Fatalf("no ribbon; texts %q", tt.Texts())
	}
	if err := tt.Click("Jump to Latest"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	tt.Frame()
	if tt.HasText(" since you scrolled up") || !visible(tt, "Line 39.", 700) {
		t.Fatalf("after Jump to Latest: texts %q", tt.Texts())
	}
}

func TestNameTipSavesTheProfileOnce(t *testing.T) {
	a := testApp(t)
	a.profile = profile.Profile{}
	b := liveHighlights(a, 1)
	b.tip = true
	tt := ui.NewTester(b.view, barWidth, barHeight+40)
	snapshot(t, tt, "captions-name-tip")
	if err := tt.Click("Your name"); err != nil {
		t.Fatalf("%v; texts %q", err, tt.Texts())
	}
	tt.Type("Alex Rivera")
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	if a.profile.Name != "Alex Rivera" || profile.Read(a.profilePath).Name != "Alex Rivera" || b.tip {
		t.Fatalf("profile %+v, tip %v", a.profile, b.tip)
	}
}

func TestSessionInfoTellsWhyHighlightsStopped(t *testing.T) {
	a := testApp(t)
	b := liveHighlights(a, 2)
	summary := store.SessionSummary{ID: "s1", Mode: store.ModeCaptions, LangA: "en-US", LangB: "ja-JP", StartedAt: time.Now()}
	w := &mainWindow{app: a, split: sidebarWidth, loaded: true, sessions: []store.SessionSummary{summary}, selected: "s1",
		detail: &store.SessionDetail{SessionSummary: summary}}
	tt := ui.NewTester(w.view, 1040, 700)
	if err := tt.Click("Session Info"); err != nil {
		t.Fatal(err)
	}
	if tt.HasText("Highlights") {
		t.Fatalf("Session Info tells of Highlights working; texts %q", tt.Texts())
	}
	tt.Key(0, ui.KeyEscape)
	b.run.(*fakeRun).snap.HighlightsErr = fmt.Errorf("check: %w", zoomai.ErrUnauthorized)
	if err := tt.Click("Session Info"); err != nil {
		t.Fatal(err)
	}
	if !tt.HasText("Your API key can’t use Zoom Decisions.") {
		t.Fatalf("Session Info: texts %q", tt.Texts())
	}
}

func TestAttentionForgetsADismissedOneButNotTheBadgesLine(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 11, 0, 0, 0, time.UTC)
	snap := session.Snapshot{Config: hlCfg, Highlights: []store.Highlight{mark("a", 4, store.KindAskedYou, 0.97, t0)}}
	var a attention
	a.follow(t0, snap, true)
	// Its line seen in Capy's window, the badge still opens the bar at it.
	a.edge = ""
	a.follow(t0.Add(4*time.Minute), snap, true)
	if !a.badge || a.badgeID != "a" {
		t.Fatalf("badge %v for %q", a.badge, a.badgeID)
	}
	var b attention
	b.follow(t0, snap, true)
	snap.Highlights[0].Dismissed = true
	b.follow(t0.Add(time.Second), snap, true)
	if b.pill != nil {
		t.Fatalf("pill %v names a dismissed highlight", b.pill)
	}
}

func TestAttachingHandsTheRecordingTheProfileSavedWhileItConnected(t *testing.T) {
	a := testApp(t)
	a.gate = gate{features.Highlights: true}
	b := liveHighlights(a, 1)
	a.profile = profile.Profile{Name: "Alex"} // saved by the tip as the recording connected
	b.attached()
	if got := b.run.(*fakeRun).snap.Config.Highlights; !got.On || got.Profile.Name != "Alex" {
		t.Fatalf("the recording's highlights %+v", got)
	}
}
