package views

import (
	"context"
	"errors"
	"fmt"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/audio"
	"github.com/onyx-dot-app/Capy/internal/features"
	"github.com/onyx-dot-app/Capy/internal/keychain"
	"github.com/onyx-dot-app/Capy/internal/profile"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/views/addon"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

// fakeRun is a session whose state the test sets.
type fakeRun struct {
	snap    session.Snapshot
	stopped bool
	spoken  []string // SetSpoken calls
}

func (f *fakeRun) Snapshot() session.Snapshot { return f.snap }
func (f *fakeRun) Stop()                      { f.stopped = true }
func (f *fakeRun) Done() <-chan struct{}      { return make(chan struct{}) }

func (f *fakeRun) SetTranslate(on bool) { f.snap.Config.Translate = on }

func (f *fakeRun) SetTarget(code string) error {
	f.snap.Config.LangB = code
	return nil
}

func (f *fakeRun) SetHighlights(cfg session.HighlightConfig) { f.snap.Config.Highlights = cfg }

// Dismiss dismisses as a session does: the highlight stays, dismissed.
func (f *fakeRun) Dismiss(_ context.Context, id string) error {
	for i := range f.snap.Highlights {
		if f.snap.Highlights[i].ID == id {
			f.snap.Highlights[i].Dismissed = true
		}
	}
	return nil
}

func (f *fakeRun) MergePolish(context.Context) error { return nil }

// SetSpoken switches as a session does: the translation stays.
func (f *fakeRun) SetSpoken(code string) error {
	f.snap.Config.LangA = code
	f.spoken = append(f.spoken, code)
	return nil
}

func testApp(t *testing.T) *App {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	return NewApp(nil, nil, path, slog.New(slog.DiscardHandler), nil, addon.None, gate{})
}

// gate is a feature gate of the features turned on in it.
type gate map[features.Feature]bool

func (g gate) Enabled(f features.Feature) bool { return g[f] }

// snapshot saves the tester's frame as a PNG when $CAPY_SNAPSHOT_DIR is
// set, to review the look of the views.
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

var captionsCfg = session.Config{Source: store.SourceMic, LangA: "en-US", LangB: "ja-JP", Translate: true}

// shown reports whether text is in the tester's window, not scrolled away.
func shown(tt *ui.Tester, text string, height float32) bool {
	r, ok := tt.Find(text)
	return ok && r.Y >= 0 && r.Y+r.H <= height
}

func TestCaptionBarShowsEachTurnAtItsTimeWithItsTranslation(t *testing.T) {
	run := &fakeRun{snap: session.Snapshot{Config: captionsCfg, Status: session.StatusLive, StartedAt: time.Now().Add(-75 * time.Second),
		Lines: []session.Line{
			{Seq: 0, StartMs: 0, Lang: "en-US", Text: "An earlier turn.", Translation: "以前のターン。", TranslationLang: "ja-JP"},
			{Seq: 1, StartMs: 65_400, Lang: "en-US", Text: "Welcome to the launch.", Translation: "ローンチへようこそ。", TranslationLang: "ja-JP"},
		},
		Draft: "Today we will",
	}}
	b := &captionBar{app: testApp(t), cfg: captionsCfg, sessionSlot: sessionSlot{run: run}}
	// A bar resized taller shows the earlier turns too.
	tt := ui.NewTester(b.view, barWidth, 400)
	snapshot(t, tt, "captions")
	b.maximized = true
	tt.SetSize(1280, 800)
	snapshot(t, tt, "captions-maximized")
	b.maximized = false
	tt.SetSize(barWidth, 400)

	var lastY float32 = -1
	for _, want := range []string{"An earlier turn.", "以前のターン。", "Welcome to the launch.", "ローンチへようこそ。", "Today we will"} {
		r, _ := tt.Find(want)
		if !shown(tt, want, 400) || r.Y <= lastY {
			t.Fatalf("caption bar does not show %q below the text before it; texts %q", want, tt.Texts())
		}
		lastY = r.Y
	}
	// Each turn has its time from the session start, on its first line.
	for stamp, text := range map[string]string{"00:00": "An earlier turn.", "01:05": "Welcome to the launch."} {
		s, ok := tt.Find(stamp)
		r, _ := tt.Find(text)
		if !ok || s.X >= r.X || s.Y+s.H < r.Y || s.Y > r.Y+r.H {
			t.Errorf("time %q at %+v, want it left of %q at %+v", stamp, s, text, r)
		}
	}
	// The time listened shows, ticking.
	if !slices.ContainsFunc(tt.Texts(), func(s string) bool { return strings.HasPrefix(s, "01:1") }) {
		t.Errorf("status lacks the time listened; texts %q", tt.Texts())
	}
}

func TestCaptionBarSideBySideShowsTheTranslationRightOfWhatWasSaid(t *testing.T) {
	run := &fakeRun{snap: session.Snapshot{Config: captionsCfg, Status: session.StatusLive,
		Lines: []session.Line{
			{Seq: 0, StartMs: 1000, Lang: "en-US", Text: "Welcome to the launch.", Translation: "ローンチへようこそ。", TranslationLang: "ja-JP"},
			{Seq: 1, StartMs: 4000, Lang: "en-US", Text: "Thanks for coming.", Translating: true, TranslationLang: "ja-JP"},
		},
		Draft: "Today we will",
	}}
	a := testApp(t)
	b := &captionBar{app: a, cfg: captionsCfg, sessionSlot: sessionSlot{run: run}}
	tt := ui.NewTester(b.view, barWidth, 300)
	said, _ := tt.Find("Welcome to the launch.")
	trans, _ := tt.Find("ローンチへようこそ。")
	if trans.Y <= said.Y {
		t.Fatalf("inline: translation at %+v, want it below %+v", trans, said)
	}

	a.prefs.CaptionsDisplay = DisplaySideBySide
	tt.Frame() // the open bar follows the setting
	snapshot(t, tt, "captions-side-by-side")
	said, _ = tt.Find("Welcome to the launch.")
	trans, _ = tt.Find("ローンチへようこそ。")
	if trans.X < said.X+said.W || trans.Y > said.Y+said.H || trans.Y+trans.H < said.Y {
		t.Fatalf("side by side: translation at %+v, want it on the row of %+v, to its right", trans, said)
	}
	// What is being said stays in the left column, clear of the translations.
	if draft, ok := tt.Find("Today we will"); !ok || draft.X+draft.W > trans.X {
		t.Fatalf("draft at %+v, %v: want it left of the translations at x=%v", draft, ok, trans.X)
	}
}

func TestCaptionBarFollowsTheLatestTurn(t *testing.T) {
	run := &fakeRun{snap: session.Snapshot{Config: captionsCfg, Status: session.StatusLive}}
	add := func() {
		seq := len(run.snap.Lines)
		run.snap.Lines = append(run.snap.Lines, session.Line{Seq: seq, StartMs: int64(seq) * 1000, Lang: "en-US",
			Text: fmt.Sprintf("Turn %d.", seq), Translation: fmt.Sprintf("ターン %d。", seq), TranslationLang: "ja-JP"})
	}
	for range 30 {
		add()
	}
	b := &captionBar{app: testApp(t), cfg: captionsCfg, sessionSlot: sessionSlot{run: run}}
	tt := ui.NewTester(b.view, barWidth, barHeight)
	if !shown(tt, "Turn 29.", barHeight) || shown(tt, "Turn 0.", barHeight) {
		t.Fatalf("want the latest turn in view and the first scrolled away; texts %q", tt.Texts())
	}
	add()
	tt.Frame()
	if !shown(tt, "Turn 30.", barHeight) {
		t.Fatalf("a new turn is not followed; texts %q", tt.Texts())
	}
}

// TestCaptionBarGlidesAsTheDraftGrows keeps the turns where they were as
// the turn being recognized takes another line, and glides them up until
// all of it shows.
func TestCaptionBarGlidesAsTheDraftGrows(t *testing.T) {
	run := &fakeRun{snap: session.Snapshot{Config: captionsCfg, Status: session.StatusLive, Draft: "Today we"}}
	for seq := range 30 {
		run.snap.Lines = append(run.snap.Lines, session.Line{Seq: seq, StartMs: int64(seq) * 1000, Lang: "en-US", Text: fmt.Sprintf("Turn %d.", seq)})
	}
	b := &captionBar{app: testApp(t), cfg: captionsCfg, sessionSlot: sessionSlot{run: run}}
	tt := ui.NewTester(b.view, barWidth, barHeight)
	before, _ := tt.Find("Turn 29.")
	run.snap.Draft += strings.Repeat(" will talk about the launch", 6) // wraps
	tt.Frame()
	if at, _ := tt.Find("Turn 29."); at.Y != before.Y {
		t.Fatalf("Turn 29 jumped from y=%v to %v; want it to glide", before.Y, at.Y)
	}
	tt.Advance(time.Second)
	if at, _ := tt.Find("Turn 29."); at.Y >= before.Y || !shown(tt, run.snap.Draft, barHeight) {
		t.Fatalf("after the glide: Turn 29 at y=%v (was %v); want it up, and the draft in view; texts %q", at.Y, before.Y, tt.Texts())
	}
}

func TestCaptionBarWithoutTranslationShowsTheCaptionsAlone(t *testing.T) {
	cfg := session.Config{Source: store.SourceMic, LangA: "en-US", LangB: "en-US"}
	run := &fakeRun{snap: session.Snapshot{Config: cfg, Status: session.StatusLive,
		Lines: []session.Line{{Seq: 0, Lang: "en-US", Text: "Only captions."}}}}
	a := testApp(t)
	a.prefs.CaptionsDisplay = DisplaySideBySide // nothing to show beside the captions
	b := &captionBar{app: a, cfg: cfg, sessionSlot: sessionSlot{run: run}}
	tt := ui.NewTester(b.view, barWidth, barHeight)
	header, ok := tt.Find("Mic · English")
	caption, shown := tt.Find("Only captions.")
	if !ok || !shown {
		t.Fatalf("texts %q", tt.Texts())
	}
	// The captions take the large type a translation would have.
	if caption.H < 1.5*header.H {
		t.Errorf("caption %v high, header %v: want the caption large", caption.H, header.H)
	}
}

func TestCaptionBarStopFinishesThenGivesWayToTheWindow(t *testing.T) {
	a := testApp(t)
	run := &fakeRun{snap: session.Snapshot{SessionID: "s1", Config: captionsCfg, Status: session.StatusLive}}
	b := &captionBar{app: a, cfg: captionsCfg, sessionSlot: sessionSlot{run: run}}
	a.captions = b
	tt := ui.NewTester(b.view, barWidth, barHeight)
	if err := tt.Click("Stop"); err != nil {
		t.Fatal(err)
	}
	if !run.stopped {
		t.Fatal("Stop did not stop the session")
	}
	// Scribe still flushes, so the session reports live for a while: the bar
	// shows it finishing, with nothing to press, and ⌥⌘C waits for the window.
	tt.Frame()
	if !tt.HasText("Finishing…") || tt.HasText("Close") || tt.HasText("Stop") {
		t.Fatalf("while finishing: texts %q", tt.Texts())
	}
	if _, ok := tt.Find("Minimize"); ok {
		t.Error("Minimize offered while finishing")
	}
	a.ToggleCaptions()
	if a.captions != b {
		t.Fatal("toggling while finishing replaced the bar: a new session would start")
	}
	run.snap.Status = session.StatusFinishing
	if _, ok := b.finished(); ok {
		t.Fatal("the window shows the session before the transcript is saved")
	}
	run.snap.Status = session.StatusSummarizing
	if id, ok := b.finished(); !ok || id != "s1" {
		t.Fatalf("transcript saved: finished %q, %v; want the window to show s1", id, ok)
	}
}

func TestCaptionBarEndedOnItsOwnOffersClose(t *testing.T) {
	run := &fakeRun{snap: session.Snapshot{SessionID: "s1", Config: captionsCfg, Status: session.StatusSummarizing, Err: zoomai.ErrUnauthorized}}
	b := &captionBar{app: testApp(t), cfg: captionsCfg, sessionSlot: sessionSlot{run: run}}
	tt := ui.NewTester(b.view, barWidth, barHeight)
	if !tt.HasText("Close") {
		t.Fatalf("a session that failed offers no Close; texts %q", tt.Texts())
	}
	if _, ok := b.finished(); ok {
		t.Fatal("a session the user did not stop opens the window")
	}
}

func TestCaptionBarExplainsMissingPermission(t *testing.T) {
	b := &captionBar{app: testApp(t), cfg: captionsCfg, sessionSlot: sessionSlot{
		err: &audio.PermissionError{What: "system audio recording", Pane: audio.SystemAudioPane}}}
	tt := ui.NewTester(b.view, barWidth, barHeight)
	snapshot(t, tt, "captions-permission")
	if !tt.HasText("Capy needs System Audio Recording permission to hear This Mac.") || !tt.HasText("Open System Settings") {
		t.Fatalf("texts %q", tt.Texts())
	}
}

// alphaAt is the opacity of the tester's last frame at (x, y), in DIPs.
func alphaAt(tt *ui.Tester, x, y int) uint8 {
	img := tt.Image()
	scale := img.Bounds().Dx() / barWidth
	return img.RGBAAt(x*scale, y*scale).A
}

func TestCaptionBarBackgroundFollowsTheTransparencySetting(t *testing.T) {
	a := testApp(t)
	b := &captionBar{app: a, cfg: captionsCfg, sessionSlot: sessionSlot{run: &fakeRun{snap: session.Snapshot{Config: captionsCfg, Status: session.StatusLive}}}}
	tt := ui.NewTester(b.view, barWidth, barHeight)
	// A point of the background, inside the border, away from any text.
	x, y := barWidth-8, barHeight/2
	for _, tc := range []struct {
		transparency int
		lo, hi       uint8
	}{{0, 250, 255}, {50, 120, 135}, {100, 0, 5}} {
		a.prefs.CaptionsTransparency = tc.transparency
		tt.Frame() // the open bar follows the setting as it changes
		snapshot(t, tt, fmt.Sprintf("captions-transparency-%d", tc.transparency))
		if got := alphaAt(tt, x, y); got < tc.lo || got > tc.hi {
			t.Errorf("transparency %d%%: background alpha %d, want %d–%d", tc.transparency, got, tc.lo, tc.hi)
		}
	}
}

func TestCaptionBarShowsItsStatus(t *testing.T) {
	line := session.Line{Seq: 0, StartMs: 31_000, Lang: "en-US", Text: "Pricing still shows last quarter's numbers.",
		Translation: "価格は前四半期のままです。", TranslationLang: "ja-JP"}
	live := func(st session.Status, lines ...session.Line) sessionSlot {
		return sessionSlot{run: &fakeRun{snap: session.Snapshot{Config: captionsCfg, Status: st, StartedAt: time.Now().Add(-3 * time.Second), Lines: lines}}}
	}
	for _, tc := range []struct {
		name     string
		slot     sessionSlot
		stopping bool
		want     string
	}{
		{"starting", sessionSlot{starting: true}, false, "Getting the microphone ready…"},
		{"waiting", live(session.StatusLive), false, "Start speaking…"},
		{"reconnecting", live(session.StatusReconnecting, line), false, "Reconnecting…"},
		{"finishing", live(session.StatusLive, line), true, "Finishing…"},
		{"no-key", sessionSlot{err: keychain.ErrNotFound}, false, "Add your Zoom API key in Settings."},
	} {
		b := &captionBar{app: testApp(t), cfg: captionsCfg, sessionSlot: tc.slot, stopping: tc.stopping}
		tt := ui.NewTester(b.view, barWidth, barHeight)
		snapshot(t, tt, "captions-"+tc.name)
		if !tt.HasText(tc.want) {
			t.Errorf("%s: texts %q, want %q", tc.name, tt.Texts(), tc.want)
		}
	}
}

func TestCaptionBarBacksTheTextOnAClearBackground(t *testing.T) {
	a := testApp(t)
	a.prefs.CaptionsTransparency = 100
	run := &fakeRun{snap: session.Snapshot{Config: captionsCfg, Status: session.StatusLive,
		Lines: []session.Line{{Seq: 0, Lang: "en-US", Text: "Welcome.", Translation: "ようこそ。", TranslationLang: "ja-JP"}}}}
	b := &captionBar{app: a, cfg: captionsCfg, sessionSlot: sessionSlot{run: run}}
	tt := ui.NewTester(b.view, barWidth, barHeight)
	r, ok := tt.Find("ようこそ。")
	if !ok {
		t.Fatalf("texts %q", tt.Texts())
	}
	// Left of the glyphs, inside the text's box: its backing, not the
	// clear bar, so that the caption reads over a white page.
	if got := alphaAt(tt, int(r.X+2), int(r.Y+r.H/2)); got < 80 {
		t.Fatalf("alpha behind the caption %d, want a dark backing", got)
	}
}

func TestCaptionBarOffersMinimizeWhileListeningAndMaximize(t *testing.T) {
	run := &fakeRun{snap: session.Snapshot{Config: captionsCfg, Status: session.StatusLive}}
	b := &captionBar{app: testApp(t), cfg: captionsCfg, sessionSlot: sessionSlot{run: run}}
	tt := ui.NewTester(b.view, barWidth, barHeight)
	if _, ok := tt.Find("Minimize"); !ok {
		t.Error("no Minimize while listening")
	}
	if _, ok := tt.Find("Maximize"); !ok {
		t.Error("no Maximize")
	}
	b.maximized = true
	tt.Frame()
	if _, ok := tt.Find("Restore"); !ok {
		t.Error("a maximized bar offers no Restore")
	}
	// Stopped, there is nothing to keep listening in a capsule.
	run.snap.Status = session.StatusFailed
	tt.Frame()
	if _, ok := tt.Find("Minimize"); ok {
		t.Error("Minimize offered once the session stopped")
	}
}

func TestCapsuleGivesWayToTheBarWhenCaptureStopsOrFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		slot sessionSlot
		want bool
	}{
		{"starting", sessionSlot{starting: true}, true},
		{"live", sessionSlot{run: &fakeRun{snap: session.Snapshot{Status: session.StatusLive}}}, true},
		{"reconnecting", sessionSlot{run: &fakeRun{snap: session.Snapshot{Status: session.StatusReconnecting}}}, true},
		{"live with a problem", sessionSlot{run: &fakeRun{snap: session.Snapshot{Status: session.StatusLive, Err: zoomai.ErrUnauthorized}}}, false},
		{"writing the recap", sessionSlot{run: &fakeRun{snap: session.Snapshot{Status: session.StatusSummarizing}}}, false},
		{"failed to start", sessionSlot{err: errors.New("no microphone")}, false},
		{"stopped, finishing", sessionSlot{run: &fakeRun{snap: session.Snapshot{Status: session.StatusFinishing}}}, true},
	} {
		b := &captionBar{app: testApp(t), cfg: captionsCfg, sessionSlot: tc.slot, stopping: strings.HasPrefix(tc.name, "stopped")}
		if got := b.capsuleWanted(); got != tc.want {
			t.Errorf("%s: capsule wanted %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCapsuleShowsTheTimeListenedOrTheStatus(t *testing.T) {
	run := &fakeRun{snap: session.Snapshot{Config: captionsCfg, Status: session.StatusLive, StartedAt: time.Now().Add(-65 * time.Second)}}
	b := &captionBar{app: testApp(t), cfg: captionsCfg, sessionSlot: sessionSlot{run: run}}
	tt := ui.NewTester(b.capsuleView, capsuleWidth, capsuleHeight)
	snapshot(t, tt, "captions-capsule")
	if !slices.ContainsFunc(tt.Texts(), func(s string) bool { return strings.HasPrefix(s, "01:0") }) {
		t.Errorf("capsule lacks the time listened; texts %q", tt.Texts())
	}
	for _, button := range []string{"Show Caption Bar", "Stop Recording"} {
		if r, ok := tt.Find(button); !ok || r.X+r.W > capsuleWidth {
			t.Errorf("%s at %+v, %v: want it inside the capsule", button, r, ok)
		}
	}
	run.snap.Status = session.StatusReconnecting
	tt.Frame()
	snapshot(t, tt, "captions-capsule-reconnecting")
	status, ok := tt.Find("Reconnecting…")
	show, _ := tt.Find("Show Caption Bar")
	if !ok || status.X+status.W > show.X {
		t.Errorf("capsule hides the reconnection, or it overlaps the buttons; texts %q", tt.Texts())
	}
}

func TestCapsuleCloseStopsTheSession(t *testing.T) {
	a := testApp(t)
	run := &fakeRun{snap: session.Snapshot{Config: captionsCfg, Status: session.StatusLive}}
	b := &captionBar{app: a, cfg: captionsCfg, sessionSlot: sessionSlot{run: run}}
	a.captions = b
	tt := ui.NewTester(b.capsuleView, capsuleWidth, capsuleHeight)
	if err := tt.Click("Stop Recording"); err != nil {
		t.Fatal(err)
	}
	if !run.stopped || !b.stopping {
		t.Fatalf("Stop Recording: stopped %v, stopping %v", run.stopped, b.stopping)
	}
	tt.Frame() // Scribe still flushes: the session reports live
	if _, ok := tt.Find("Stop Recording"); ok || !tt.HasText("Finishing…") {
		t.Fatalf("while finishing: texts %q; want the status and no second stop", tt.Texts())
	}
}

func TestSettingsSavesTheCaptionTransparency(t *testing.T) {
	t.Setenv("ZOOM_API_KEY", "from-env") // keep the test away from the real Keychain
	a := testApp(t)
	w := &settingsPage{app: a, keyState: readKeyState()}
	tt := ui.NewTester(settingsShown(w).view, 1040, 900)
	value, ok := tt.Find("18%")
	if !ok {
		t.Fatalf("no transparency shown; texts %q", tt.Texts())
	}
	// The slider's right end, left of the value.
	tt.ClickAt(value.X-14, value.Y+value.H/2)
	got := LoadPrefs(a.prefsPath).CaptionsTransparency
	if got < 90 || !tt.HasText(fmt.Sprintf("%d%%", got)) {
		t.Fatalf("saved transparency %d, texts %q: want near 100 and shown", got, tt.Texts())
	}
}

func TestSessionInfoShowsAFilesTrackingID(t *testing.T) {
	const tid = "v=2.0;clid=us01;rid=WEB_f2a7c3164c9c0aa134f59fedda450c5e"
	started := time.Date(2026, 10, 7, 9, 30, 0, 0, time.Local)
	summary := store.SessionSummary{ID: "s1", Mode: store.ModeCaptions, Source: store.SourceFile, FileName: "ES2010d.wav",
		LangA: "en-US", LangB: "en-US", Title: "ES2010d", RecapStatus: store.RecapReady, StartedAt: started,
		EndedAt: started.Add(30 * time.Minute)}
	w := &mainWindow{app: testApp(t), split: sidebarWidth, sessions: []store.SessionSummary{summary}, selected: "s1", loaded: true,
		detail: &store.SessionDetail{SessionSummary: summary, Recap: "# ES2010d", TrackingID: tid,
			Segments: []store.Segment{{ID: "g1", Lang: "en-US", Text: "Hello.", EndMs: 1000}}}}
	tt := ui.NewTester(w.view, 1040, 700)
	if err := tt.Click("Session Info"); err != nil {
		t.Fatal(err)
	}
	snapshot(t, tt, "window-info-file")
	// Whole, to send to Zoom: it wraps rather than being cut, within the
	// popover, as wide as its footer.
	id, ok := tt.Find(tid)
	if !tt.HasText("Tracking ID") || !ok {
		t.Fatalf("Session Info lacks the tracking ID; texts %q", tt.Texts())
	}
	if footer, _ := tt.Find("Capy keeps the transcript, never the audio."); id.X+id.W > footer.X+footer.W {
		t.Errorf("tracking ID at %+v overflows the popover, %+v wide", id, footer)
	}
}

func TestWindowShowsRecapTranscriptAndCopiesMarkdown(t *testing.T) {
	started := time.Date(2026, 10, 7, 9, 30, 0, 0, time.Local)
	summary := store.SessionSummary{ID: "s1", Mode: store.ModeCaptions, LangA: "en-US", LangB: "zh-CN", Translated: true,
		Title: "Launch rehearsal", RecapStatus: store.RecapReady, StartedAt: started, EndedAt: started.Add(95 * time.Second)}
	w := &mainWindow{app: testApp(t), split: sidebarWidth, sessions: []store.SessionSummary{summary}, selected: "s1", loaded: true,
		detail: &store.SessionDetail{SessionSummary: summary, Recap: "# Launch rehearsal\n\n## Action items\n\n- Ship on Friday",
			Segments: []store.Segment{
				{ID: "g1", Lang: "en-US", Text: "We ship Friday.", Translation: "我们周五发布。", TranslationLang: "zh-CN", StartMs: 42_000, EndMs: 44_000},
				{ID: "g2", Lang: store.GapLang},
			}}}
	tt := ui.NewTester(w.view, 1040, 700)
	snapshot(t, tt, "window")
	tt.SetDark(true)
	snapshot(t, tt, "window-dark")
	tt.SetDark(false)

	// The summary shows first, as a document under the session's title,
	// which it does not repeat; the transcript is a tab away. The header
	// says when and how long; the rest is in Session Info.
	for _, want := range []string{"Launch rehearsal", startedText(started, time.Now()), "1 min 35 sec", "Action items", "Ship on Friday"} {
		if _, ok := tt.Find(want); !ok {
			t.Errorf("window lacks %q; texts %q", want, tt.Texts())
		}
	}
	if tt.HasText("Microphone") || tt.HasText("中文 (简体)") {
		t.Errorf("the header shows the source or languages; texts %q", tt.Texts())
	}
	// The session's actions sit on the meta line with Session Info, not in
	// the toolbar beside Record.
	info, _ := tt.Find("Session Info")
	for _, action := range []string{"Copy as Markdown", "Export…", "Delete"} {
		if r, ok := tt.Find(action); !ok || r.Y != info.Y || r.X <= info.X || r.Y < titleBarHeight {
			t.Errorf("%s at %+v, want on the meta line after Session Info at %+v", action, r, info)
		}
	}
	if err := tt.Click("Session Info"); err != nil {
		t.Fatal(err)
	}
	snapshot(t, tt, "window-info")
	for _, want := range []string{"Wednesday, Oct 7, 2026 at 09:30", "Microphone", "English", "中文 (简体)", "1 line"} { // the gap is not a line
		if _, ok := tt.Find(want); !ok {
			t.Errorf("Session Info lacks %q; texts %q", want, tt.Texts())
		}
	}
	if tt.HasText("Tracking ID") { // a recording made no request to find
		t.Errorf("a recording's Session Info shows a tracking ID; texts %q", tt.Texts())
	}
	tt.Key(0, ui.KeyEscape)
	if tt.HasText("Microphone") {
		t.Fatalf("Escape left Session Info open; texts %q", tt.Texts())
	}
	tt.Key(ui.Cmd, ui.KeyI)
	if !tt.HasText("Microphone") {
		t.Fatalf("⌘I did not open Session Info; texts %q", tt.Texts())
	}
	tt.Key(0, ui.KeyEscape)
	if n := strings.Count(strings.Join(tt.Texts(), "\n"), "Launch rehearsal"); n != 2 { // the row and the header
		t.Errorf("the title shows %d times, want the row and the header only; texts %q", n, tt.Texts())
	}
	if tt.HasText("We ship Friday.") {
		t.Errorf("the summary tab shows the transcript; texts %q", tt.Texts())
	}
	if err := tt.Click("Transcript"); err != nil {
		t.Fatal(err)
	}
	snapshot(t, tt, "window-transcript")
	for _, want := range []string{"We ship Friday.", "我们周五发布。", "00:42", "Connection lost"} {
		if !tt.HasText(want) {
			t.Errorf("transcript tab lacks %q; texts %q", want, tt.Texts())
		}
	}
	if tt.HasText("Ship on Friday") {
		t.Errorf("the transcript tab shows the summary; texts %q", tt.Texts())
	}
	if err := tt.Click("Copy as Markdown"); err != nil {
		t.Fatal(err)
	}
	if md := tt.Clipboard(); !strings.HasPrefix(md, "# Launch rehearsal") || !strings.Contains(md, "我们周五发布。") {
		t.Fatalf("clipboard %q", md)
	}
}

func TestStartedTextSaysTheDayInWords(t *testing.T) {
	now := time.Date(2026, 10, 8, 15, 0, 0, 0, time.Local)
	for _, c := range []struct {
		t    time.Time
		want string
	}{
		{time.Date(2026, 10, 8, 0, 0, 0, 0, time.Local), "Today at 00:00"},
		{time.Date(2026, 10, 7, 23, 59, 0, 0, time.Local), "Yesterday at 23:59"},
		{time.Date(2026, 10, 5, 20, 12, 0, 0, time.Local), "Monday, Oct 5 at 20:12"},
		{time.Date(2025, 12, 30, 9, 5, 0, 0, time.Local), "Tuesday, Dec 30, 2025 at 09:05"},
	} {
		if got := startedText(c.t, now); got != c.want {
			t.Errorf("startedText(%v) = %q, want %q", c.t, got, c.want)
		}
	}
}

func TestLengthTextLeavesOutZeroParts(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                                    "0 sec",
		45 * time.Second:                     "45 sec",
		95 * time.Second:                     "1 min 35 sec",
		2 * time.Minute:                      "2 min",
		time.Hour + 2*time.Minute:            "1 hr 2 min",
		time.Hour + 3*time.Second:            "1 hr 3 sec",
		25*time.Hour + 1500*time.Millisecond: "25 hr 1 sec",
	} {
		if got := lengthText(d); got != want {
			t.Errorf("lengthText(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestWindowRecordsAndStops(t *testing.T) {
	a := testApp(t)
	w := &mainWindow{app: a, split: sidebarWidth, loaded: true, keyState: keyStored}
	tt := ui.NewTester(w.view, 1040, 700)
	snapshot(t, tt, "window-ready")
	// Before the first session, Record shows large, with what it listens to.
	if n := strings.Count(strings.Join(tt.Texts(), "\n"), "Record\n"); n < 2 || !tt.HasText("Microphone · Auto-detect") ||
		!tt.HasText("never the audio") {
		t.Fatalf("ready window: texts %q", tt.Texts())
	}
	// Captions started from the menu or ⌥⌘C: the window offers to stop them,
	// with the time listened.
	b := &captionBar{app: a, cfg: captionsCfg, sessionSlot: sessionSlot{starting: true}}
	a.captions = b
	tt.Frame()
	if !tt.HasText("Starting…") || tt.HasText("Stop") {
		t.Fatalf("while connecting: texts %q", tt.Texts())
	}
	run := &fakeRun{snap: session.Snapshot{SessionID: "s1", Config: captionsCfg, Status: session.StatusLive, StartedAt: time.Now().Add(-65 * time.Second)}}
	b.attach(run, nil)
	tt.Frame()
	if !slices.ContainsFunc(tt.Texts(), func(s string) bool { return strings.HasPrefix(s, "Stop · 01:0") }) {
		t.Fatalf("live: texts %q; want Stop with the time listened", tt.Texts())
	}
	if err := tt.Click("Stop Recording"); err != nil {
		t.Fatalf("%v; texts %q", err, tt.Texts())
	}
	if !run.stopped || !b.stopping {
		t.Fatal("Stop did not stop the session")
	}
	tt.Frame()
	if _, ok := tt.Find("Record"); ok || !tt.HasText("Finishing…") {
		t.Fatalf("while the transcript is saved: texts %q", tt.Texts())
	}
}

func TestWindowWelcomesAFirstLaunchWithoutAKey(t *testing.T) {
	w := &mainWindow{app: testApp(t), split: sidebarWidth, loaded: true, keyState: keyNone}
	tt := ui.NewTester(w.view, 1040, 700)
	snapshot(t, tt, "window-welcome")
	if !tt.HasText("Welcome to Capy") || !tt.HasText("Add API Key…") || !tt.HasText("No Sessions") {
		t.Fatalf("first launch: texts %q", tt.Texts())
	}
}

func TestWindowShowsTheSessionBeingRecorded(t *testing.T) {
	a := testApp(t)
	started := time.Now().Add(-134 * time.Second)
	run := &fakeRun{snap: session.Snapshot{SessionID: "s1", Config: captionsCfg, Status: session.StatusLive, StartedAt: started,
		Lines: []session.Line{
			{Seq: 0, Lang: "en-US", Text: "Welcome to the launch.", Translation: "ローンチへようこそ。", TranslationLang: "ja-JP"},
			{Seq: 1, StartMs: 4000, Lang: "en-US", Text: "Thanks for coming.", Translating: true, TranslationLang: "ja-JP"},
		},
		Draft: "Today we will"}}
	a.captions = &captionBar{app: a, cfg: captionsCfg, sessionSlot: sessionSlot{run: run}, minimized: true}
	summary := store.SessionSummary{ID: "s1", Mode: store.ModeCaptions, LangA: "en-US", LangB: "ja-JP", StartedAt: started}
	w := &mainWindow{app: a, split: sidebarWidth, loaded: true, sessions: []store.SessionSummary{summary}, selected: "s1",
		detail: &store.SessionDetail{SessionSummary: summary}}
	tt := ui.NewTester(w.view, 1040, 700)
	snapshot(t, tt, "window-recording")
	// The lines come from the running session, not the saved transcript.
	for _, want := range []string{"Recording…", "Welcome to the launch.", "ローンチへようこそ。", "Thanks for coming.", "Today we will", "Show Caption Bar"} {
		if !tt.HasText(want) {
			t.Errorf("recording view lacks %q; texts %q", want, tt.Texts())
		}
	}
	// A session being recorded has nothing to copy, export or delete yet.
	_, canCopy := tt.Find("Copy as Markdown")
	if _, canDelete := tt.Find("Delete"); canCopy || canDelete || tt.HasText("Summary") {
		t.Errorf("recording view offers the session's actions or tabs; texts %q", tt.Texts())
	}
	// Its header ticks the time listened; Session Info has no line count yet.
	if !tt.HasText("Started "+started.Format("15:04")) ||
		!slices.ContainsFunc(tt.Texts(), func(s string) bool { return strings.HasPrefix(s, "2 min 1") }) {
		t.Errorf("recording header: texts %q", tt.Texts())
	}
	tt.Key(ui.Cmd, ui.KeyI)
	if _, ok := tt.Find("日本語"); !ok ||
		!slices.ContainsFunc(tt.Texts(), func(s string) bool { return strings.HasSuffix(s, " so far") }) ||
		tt.HasText("Transcript") {
		t.Errorf("Session Info while recording: texts %q", tt.Texts())
	}
}

func TestReadyWindowOffersRecordAndImport(t *testing.T) {
	a := testApp(t)
	w := &mainWindow{app: a, split: sidebarWidth, loaded: true, keyState: keyStored}
	tt := ui.NewTester(w.view, 1040, 700)
	snapshot(t, tt, "window-ready")
	for _, want := range []string{"Import…", "Record", "or press ⌥⌘C", "Have recordings already?", "Import Audio Files…",
		"or drop them anywhere in this window. Each file gets a transcript and a recap.",
		"Capy keeps the transcript, never the audio.", "Sessions you record or import appear here."} {
		if !tt.HasText(want) {
			t.Errorf("ready window lacks %q; texts %q", want, tt.Texts())
		}
	}
	// While recording, Record stops it; Import stays.
	a.captions = &captionBar{app: a, cfg: captionsCfg, sessionSlot: sessionSlot{run: &fakeRun{snap: session.Snapshot{
		SessionID: "s1", Config: captionsCfg, Status: session.StatusLive, StartedAt: time.Now()}}}}
	tt.Frame()
	if _, ok := tt.Find("Stop Recording"); !ok || !tt.HasText("Import…") {
		t.Fatalf("live: texts %q", tt.Texts())
	}
}

func TestRecordSetupMenuSavesTheSourceAndLanguages(t *testing.T) {
	a := testApp(t)
	w := &mainWindow{app: a, split: sidebarWidth, loaded: true, keyState: keyStored}
	tt := ui.NewTester(w.view, 1040, 700)
	choose := func(path ...string) {
		t.Helper()
		if err := tt.Click("Recording setup"); err != nil {
			t.Fatal(err)
		}
		if err := tt.ChooseMenuItem(path...); err != nil {
			t.Fatalf("%v; menu %q", err, tt.Menu())
		}
	}
	if err := tt.Click("Recording setup"); err != nil {
		t.Fatal(err)
	}
	snapshot(t, tt, "record-setup-menu")
	want := []string{"Listen To", "Microphone", "This Mac", "-", "Spoken Language", "Translate", "Translate To", "-", "Caption Settings…"}
	if menu := tt.Menu(); !slices.Equal(menu, want) {
		t.Fatalf("menu %q, want %q", menu, want)
	}
	tt.CloseMenu()
	choose("This Mac")
	choose("Spoken Language", "English")
	choose("Translate To", "日本語")
	choose("Translate")
	got := LoadPrefs(a.prefsPath)
	if got.CaptionsSource != store.SourceSystem || got.CaptionsFrom != "en-US" || got.CaptionsTo != "ja-JP" || !got.CaptionsTranslate {
		t.Fatalf("saved %+v; want this Mac, English, translated into 日本語", got)
	}
	// What Record will listen to shows before recording.
	if !tt.HasText("This Mac · English → 日本語") {
		t.Fatalf("texts %q", tt.Texts())
	}
}

func TestRecordKeepsItsPlaceAndWidthAsItRecords(t *testing.T) {
	a := testApp(t)
	tt := ui.NewTester(endedWindow(a).view, 1040, 700)
	snapshot(t, tt, "record-split")
	chevron, ok := tt.Find("Recording setup")
	if !ok {
		t.Fatalf("no chevron beside Record; texts %q", tt.Texts())
	}
	right := chevron.X + chevron.W
	if rec, _ := tt.Find("Record"); !near(rec.X, right-recordWidth) || rec.X+rec.W > chevron.X {
		t.Fatalf("Record at %+v, chevron at %+v; want them %v wide together", rec, chevron, recordWidth)
	}
	// Once pressed, Record is one button as wide, in the same place, and
	// the setup cannot change.
	b := &captionBar{app: a, cfg: captionsCfg, sessionSlot: sessionSlot{starting: true}}
	a.captions = b
	tt.Frame()
	snapshot(t, tt, "record-starting")
	if _, ok := tt.Find("Recording setup"); ok {
		t.Fatal("the chevron shows while starting")
	}
	b.attach(&fakeRun{snap: session.Snapshot{SessionID: "s1", Config: captionsCfg, Status: session.StatusLive, StartedAt: time.Now()}}, nil)
	tt.Frame()
	snapshot(t, tt, "record-live")
	stop, ok := tt.Find("Stop Recording")
	if _, chevron := tt.Find("Recording setup"); !ok || chevron || !near(stop.W, recordWidth) || !near(stop.X+stop.W, right) {
		t.Fatalf("Stop at %+v (chevron shown %v); want %v wide, ending at x=%v", stop, chevron, recordWidth, right)
	}
}

func TestWindowOffersRetryForAFailedRecap(t *testing.T) {
	started := time.Date(2026, 10, 7, 9, 30, 0, 0, time.Local)
	summary := store.SessionSummary{ID: "s1", Mode: store.ModeConversation, LangA: "zh-CN", LangB: "en-US",
		RecapStatus: store.RecapFailed, StartedAt: started, EndedAt: started.Add(time.Minute)}
	w := &mainWindow{app: testApp(t), split: sidebarWidth, sessions: []store.SessionSummary{summary}, selected: "s1", loaded: true,
		detail: &store.SessionDetail{SessionSummary: summary,
			Segments: []store.Segment{
				{ID: "g1", Lang: "zh-CN", Text: "你好", TranslationLang: "en-US"},
				{ID: "g2", Seq: 1, Lang: "zh-CN", Text: "翻译关着的时候说的。"}, // said with translation off
			}}}
	tt := ui.NewTester(w.view, 1040, 700)
	snapshot(t, tt, "window-recap-failed")
	if !tt.HasText("The recap couldn’t be written.") || !tt.HasText("Retry") {
		t.Errorf("failed recap: texts %q", tt.Texts())
	}
	if err := tt.Click("Transcript"); err != nil {
		t.Fatal(err)
	}
	notTranslated := 0
	for _, s := range tt.Texts() {
		if s == "Not translated" {
			notTranslated++
		}
	}
	if notTranslated != 1 || !tt.HasText("Retry Translations") {
		t.Errorf("one turn awaits its translation, the other was said with translation off: texts %q", tt.Texts())
	}
}

func TestWindowOffersRetryForARecapSavedEmpty(t *testing.T) {
	started := time.Date(2026, 10, 7, 9, 30, 0, 0, time.Local)
	summary := store.SessionSummary{ID: "s1", Mode: store.ModeCaptions, LangA: "en-US", LangB: "en-US",
		RecapStatus: store.RecapReady, StartedAt: started, EndedAt: started.Add(time.Minute), SegmentCount: 1}
	w := &mainWindow{app: testApp(t), split: sidebarWidth, sessions: []store.SessionSummary{summary}, selected: "s1", loaded: true,
		detail: &store.SessionDetail{SessionSummary: summary,
			Segments: []store.Segment{{ID: "g1", Lang: "en-US", Text: "Something was said."}}}}
	tt := ui.NewTester(w.view, 1040, 700)
	if tt.HasText("Nothing was said in this session.") || !tt.HasText("The recap couldn’t be written.") {
		t.Fatalf("an empty recap of a transcript: texts %q", tt.Texts())
	}
	// Captions in the spoken language have nothing to translate.
	if err := tt.Click("Transcript"); err != nil {
		t.Fatal(err)
	}
	if tt.HasText("Not translated") {
		t.Fatalf("captions without translation: texts %q", tt.Texts())
	}
	// Session Info names the spoken language only.
	tt.Key(ui.Cmd, ui.KeyI)
	if _, ok := tt.Find("English"); !ok || tt.HasText("Translated to") {
		t.Fatalf("Session Info of captions without translation: texts %q", tt.Texts())
	}
}

func TestMenuFollowsTheRecordingAndListsRecentSessions(t *testing.T) {
	a := testApp(t)
	a.recent = []store.SessionSummary{{ID: "s1", Title: "Launch rehearsal", EndedAt: time.Now()}}
	labels := func() (out []string, record *mygo.MenuItem) {
		for _, it := range a.trayMenu().Items() {
			out = append(out, it.Label)
			if strings.Contains(it.Label, "Record") || it.Label == "Finishing…" {
				record = it
			}
		}
		return out, record
	}
	got, record := labels()
	if record == nil || record.Label != "Record" || record.Disabled || !slices.Contains(got, "Launch rehearsal") {
		t.Fatalf("idle menu %q", got)
	}
	run := &fakeRun{snap: session.Snapshot{Config: captionsCfg, Status: session.StatusLive, StartedAt: time.Now()}}
	a.captions = &captionBar{app: a, cfg: captionsCfg, sessionSlot: sessionSlot{run: run}}
	if _, record = labels(); record == nil || record.Label != "Stop Recording" {
		t.Fatalf("live: record item %+v", record)
	}
	a.captions.stopping = true
	if _, record = labels(); record == nil || record.Label != "Finishing…" || !record.Disabled {
		t.Fatalf("finishing: record item %+v; want it disabled", record)
	}
}

func TestCapyIsInTheDockWhileItsWindowIsOpen(t *testing.T) {
	a := testApp(t)
	if got := a.dockPolicy(); got != mygo.ActivationPolicyAccessory {
		t.Fatalf("nothing open: %s; want the menu bar item alone", got)
	}
	// The caption bar floats over other apps: it must not take the Dock
	// and the menu bar from the one being captioned.
	a.captions = &captionBar{app: a, cfg: captionsCfg}
	if got := a.dockPolicy(); got != mygo.ActivationPolicyAccessory {
		t.Fatalf("caption bar alone: %s; want the menu bar item alone", got)
	}
	a.window = &mainWindow{app: a}
	if got := a.dockPolicy(); got != mygo.ActivationPolicyRegular {
		t.Fatalf("window open: %s; want the Dock", got)
	}
}

func TestDeleteShowsTheNextOlderSession(t *testing.T) {
	list := []store.SessionSummary{{ID: "new"}, {ID: "mid"}, {ID: "old"}}
	for id, want := range map[string]string{"new": "mid", "mid": "old", "old": "mid", "gone": ""} {
		if got := nextAfterDelete(list, id); got != want {
			t.Errorf("after deleting %q: %q, want %q", id, got, want)
		}
	}
	if got := nextAfterDelete(list[:1], "new"); got != "" {
		t.Errorf("after deleting the only session: %q, want none", got)
	}
}

func TestSidebarGroupsSessionsByDay(t *testing.T) {
	now := time.Date(2026, 10, 8, 11, 0, 0, 0, time.Local)
	at := func(id string, t time.Time) store.SessionSummary { return store.SessionSummary{ID: id, StartedAt: t} }
	sessions := []store.SessionSummary{
		at("today", now.Add(-time.Hour)),
		at("yesterday", time.Date(2026, 10, 7, 23, 50, 0, 0, time.Local)),
		at("week", time.Date(2026, 10, 3, 10, 0, 0, 0, time.Local)),
		at("week2", time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)),
		at("old", time.Date(2026, 9, 30, 23, 0, 0, 0, time.Local)),
	}
	var got []string
	for _, e := range sidebarEntries(sessions, nil, "", now, nil) {
		if e.header != "" {
			got = append(got, "["+e.header+"]")
			continue
		}
		got = append(got, fmt.Sprintf("%s%v", e.session.ID, map[bool]string{true: "·date"}[e.older]))
	}
	want := []string{"[Today]", "today", "[Yesterday]", "yesterday", "[Previous 7 Days]", "week·date", "week2·date", "[Earlier]", "old·date"}
	if !slices.Equal(got, want) {
		t.Fatalf("entries %q, want %q", got, want)
	}
	// Search results go under their count instead.
	if e := sidebarEntries(sessions[:1], nil, "pricing", now, nil); len(e) != 2 || e[0].header != "1 Result" {
		t.Fatalf("search entries %+v", e)
	}
}

func TestRecapBodyDropsTheTitleHeadingOnly(t *testing.T) {
	for recap, want := range map[string]string{
		"# Launch rehearsal\n\nWe ship Friday.": "\nWe ship Friday.",
		"We ship Friday.\nThen rest.":           "We ship Friday.\nThen rest.", // the first line is content
	} {
		if got := recapBody(recap); got != want {
			t.Errorf("recapBody(%q) = %q, want %q", recap, got, want)
		}
	}
}

// TestRecapShowsItsMarkdownAsItReads sets emphasis, links, numbered and
// nested lists and tables, none of their syntax showing.
func TestRecapShowsItsMarkdownAsItReads(t *testing.T) {
	started := time.Date(2026, 10, 7, 9, 30, 0, 0, time.Local)
	summary := store.SessionSummary{ID: "s1", Title: "Launch rehearsal", RecapStatus: store.RecapReady, StartedAt: started, EndedAt: started.Add(time.Minute)}
	recap := "# Launch rehearsal\n\nWe ship **on Friday**, see [the plan](https://example.com/plan).\n\n## Action items\n\n" +
		"1. Maya: update pricing\n   - the slide\n2. Leo: record a demo\n\n| Owner | Task |\n|---|---|\n| Ana | QA |\n"
	w := &mainWindow{app: testApp(t), split: sidebarWidth, sessions: []store.SessionSummary{summary}, selected: "s1", loaded: true,
		detail: &store.SessionDetail{SessionSummary: summary, Recap: recap}}
	tt := ui.NewTester(w.view, 1040, 700)
	snapshot(t, tt, "window-recap-markdown")
	for _, want := range []string{"We ship on Friday, see the plan.", "the plan", "Action items", "Maya: update pricing", "the slide", "Owner", "QA"} {
		if _, ok := tt.Find(want); !ok {
			t.Errorf("the recap lacks %q; texts %q", want, tt.Texts())
		}
	}
	for _, text := range tt.Texts() {
		if strings.ContainsAny(text, "*|[") || strings.HasPrefix(text, "1. ") {
			t.Errorf("the recap shows Markdown's syntax in %q", text)
		}
	}
}

func TestSettingsSavesTheTranslationLanguage(t *testing.T) {
	t.Setenv("ZOOM_API_KEY", "from-env") // keep the test away from the real Keychain
	a := testApp(t)
	w := &settingsPage{app: a, keyState: readKeyState()}
	tt := ui.NewTester(settingsShown(w).view, 1040, 900)
	snapshot(t, tt, "settings")
	if !tt.HasText("Using $ZOOM_API_KEY") || tt.HasText("Replace…") {
		t.Errorf("key from the environment: texts %q", tt.Texts())
	}
	if err := tt.Click("中文 (简体)"); err != nil { // the Translate to pop-up opens its menu
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("한국어"); err != nil {
		t.Fatalf("%v; menu %q", err, tt.Menu())
	}
	if got := LoadPrefs(a.prefsPath).CaptionsTo; got != "ko-KR" {
		t.Fatalf("saved translation language %q, want ko-KR", got)
	}
}

func TestSettingsOffersSideBySideForTranslatedCaptionsOnly(t *testing.T) {
	t.Setenv("ZOOM_API_KEY", "from-env") // keep the test away from the real Keychain
	a := testApp(t)
	a.prefs.CaptionsTranslate = true
	w := &settingsPage{app: a, keyState: readKeyState()}
	tt := ui.NewTester(settingsShown(w).view, 1040, 900)
	if err := tt.Click("Side by side"); err != nil {
		t.Fatal(err)
	}
	if got := LoadPrefs(a.prefsPath).CaptionsDisplay; got != DisplaySideBySide {
		t.Fatalf("saved display %q, want side by side", got)
	}
	a.prefs.CaptionsTranslate = false // captions without translation
	tt.Frame()
	if err := tt.Click("Inline"); err != nil {
		t.Fatal(err)
	}
	if got := LoadPrefs(a.prefsPath).CaptionsDisplay; got != DisplaySideBySide {
		t.Fatalf("display changed to %q with nothing to show beside the captions", got)
	}
}

func TestSettingsShowsAStoredKeyToReplaceOrDelete(t *testing.T) {
	w := &settingsPage{app: testApp(t), keyState: keyStored}
	tt := ui.NewTester(settingsShown(w).view, 1040, 900)
	snapshot(t, tt, "settings-key")
	tt.SetDark(true)
	snapshot(t, tt, "settings-key-dark")
	tt.SetDark(false)
	if !tt.HasText("Saved in your Keychain") || !tt.HasText("Delete…") {
		t.Fatalf("stored key: texts %q", tt.Texts())
	}
	if _, ok := tt.Find("Save"); ok {
		t.Fatal("a stored key shows the field to paste one")
	}
	if err := tt.Click("Replace…"); err != nil {
		t.Fatal(err)
	}
	if _, ok := tt.Find("Save"); !ok || !tt.HasText("Cancel") {
		t.Fatalf("replacing: texts %q", tt.Texts())
	}
	if err := tt.Click("Cancel"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("Delete…"); err != nil {
		t.Fatal(err)
	}
	if !tt.HasText("Delete the API key?") {
		t.Fatalf("Delete asks no confirmation; texts %q", tt.Texts())
	}
}

func TestSettingsKeepsAFailedKeyChangeToRetry(t *testing.T) {
	w := &settingsPage{app: testApp(t), keyState: keyStored}
	tt := ui.NewTester(settingsShown(w).view, 1040, 900)
	if err := tt.Click("Replace…"); err != nil {
		t.Fatal(err)
	}
	w.busy = "Saving…"
	tt.Frame()
	if err := tt.Click("Cancel"); err != nil || !w.replacing {
		t.Fatalf("Cancel while saving: %v, replacing %v; want it disabled", err, w.replacing)
	}
	w.keyChanged(keyStored, errors.New("refused"), "Could not save the key.")
	tt.Frame()
	snapshot(t, tt, "settings-key-error")
	if _, ok := tt.Find("Save"); !ok || !tt.HasText("Could not save the key.") {
		t.Fatalf("after a failed save: texts %q; want the field and the reason", tt.Texts())
	}
	w.keyChanged(keyStored, nil, "Could not save the key.")
	tt.Frame()
	if !tt.HasText("Saved in your Keychain") || tt.HasText("Could not save the key.") {
		t.Fatalf("after a save: texts %q", tt.Texts())
	}
}

// settingsShown is Capy's window showing Settings, page, whose key changes
// post to a queue no test runs.
func settingsShown(page *settingsPage) *mainWindow {
	if page.post == nil {
		page.post = make(posted, 16).post
	}
	return &mainWindow{app: page.app, split: sidebarWidth, loaded: true, keyState: keyStored, settings: page}
}

func TestSidebarSettingsButtonShowsSettingsInPlaceOfTheSession(t *testing.T) {
	t.Setenv("ZOOM_API_KEY", "from-env") // keep the test away from the real Keychain
	a, w, q := storeApp(t, filepath.Join(t.TempDir(), "capy.db"))
	s := seed(t, a.store, "Launch rehearsal", 2)
	tt := ui.NewTester(w.view, 1040, 700)
	w.reload()
	q.run(t, 2) // the sessions, then the newest, selected
	tt.Frame()
	if err := tt.Click("Settings"); err != nil {
		t.Fatal(err)
	}
	q.run(t, 2) // where the key comes from, then the sessions again, which must not select the newest
	tt.Frame()
	snapshot(t, tt, "settings-page")
	if w.settings == nil || w.selected != "" || !tt.HasText("Using $ZOOM_API_KEY") {
		t.Fatalf("after Settings: selected %q, texts %q; want Settings in place of the session", w.selected, tt.Texts())
	}
	// ⌘, again, or a recording starting or finishing, keeps Settings and
	// what is typed in it.
	w.settings.key = "typed, not saved"
	w.openSettings()
	w.showRecording(s.ID)
	if w.settings == nil || w.settings.key != "typed, not saved" || w.selected != "" {
		t.Fatalf("Settings again or a recording: settings %v, selected %q; want Settings as it was", w.settings != nil, w.selected)
	}
	// Recently Deleted and Settings show in place of each other.
	w.openTrash()
	tt.Frame()
	if w.settings != nil || tt.HasText("Zoom API key") || !tt.HasText("Sessions are erased for good 30 days after they’re deleted.") {
		t.Fatalf("Recently Deleted from Settings: texts %q", tt.Texts())
	}
	w.openSettings()
	q.run(t, 2) // where the key comes from, then the sessions again
	tt.Frame()
	if w.trashOpen || !tt.HasText("Zoom API key") {
		t.Fatalf("Settings from Recently Deleted: texts %q", tt.Texts())
	}
	// Leaving Settings drops a key typed but not saved.
	w.settings.key = "typed, not saved"
	if err := tt.Click("Launch rehearsal"); err != nil {
		t.Fatal(err)
	}
	q.run(t, 1) // the session
	tt.Frame()
	if w.settings != nil || w.selected != s.ID || w.shown() == nil {
		t.Fatalf("after the session: settings %v, selected %q, shown %v", w.settings != nil, w.selected, w.shown() != nil)
	}
	if err := tt.Click("Settings"); err != nil {
		t.Fatal(err)
	}
	if w.settings == nil || w.settings.key != "" {
		t.Fatal("Settings again: want it shown, without the key typed before")
	}
}

func TestGearOrEscapeLeavesSettingsForTheEmptyWindow(t *testing.T) {
	t.Setenv("ZOOM_API_KEY", "from-env") // keep the test away from the real Keychain
	_, w, q := storeApp(t, library(t))
	tt := ui.NewTester(w.view, 1040, 700)
	w.reload()
	q.settle(t)
	tt.Frame()
	if !tt.HasText("Import Audio Files…") {
		t.Fatalf("empty library: texts %q; want the window before the first session", tt.Texts())
	}
	// With no session in the sidebar to go to, the gear clicked again, or
	// Escape, leaves Settings.
	for _, leave := range []func() error{
		func() error { return tt.Click("Settings") },
		func() error { tt.Key(0, ui.KeyEscape); return nil },
	} {
		w.openSettings() // as Caption settings does, without showing a real window
		q.settle(t)
		tt.Frame()
		if w.settings == nil || tt.HasText("Import Audio Files…") {
			t.Fatalf("Settings: texts %q; want Settings", tt.Texts())
		}
		if err := leave(); err != nil {
			t.Fatal(err)
		}
		q.settle(t)
		tt.Frame()
		if w.settings != nil || !tt.HasText("Import Audio Files…") {
			t.Fatalf("leaving Settings: texts %q; want the window before the first session", tt.Texts())
		}
	}
	// Escape in the key being replaced cancels the replacing only.
	w.openSettings()
	q.settle(t)
	w.settings.keyState, w.settings.replacing = keyStored, true
	tt.Frame()
	if err := tt.Click("API key"); err != nil {
		t.Fatal(err)
	}
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	if w.settings == nil || w.settings.replacing {
		t.Fatalf("Escape while replacing the key: settings %v; want Settings, the replacing cancelled", w.settings != nil)
	}
}

func TestLeavingSettingsOrRecentlyDeletedShowsTheSessionShownBefore(t *testing.T) {
	t.Setenv("ZOOM_API_KEY", "from-env") // keep the test away from the real Keychain
	_, w, q, s, _ := libraryWindow(t, library(t), "First", "Second")
	tt := ui.NewTester(w.view, 1040, 700)
	tt.Key(ui.Cmd, ui.KeyBackspace) // Second to Recently Deleted: First shows
	q.settle(t)
	tt.Frame()
	if w.selected != s[0].ID {
		t.Fatalf("after ⌘⌫: selected %q, want First", w.selected)
	}
	// Settings, then Recently Deleted in its place, then the trash button
	// again: First shows, as before either opened.
	for _, button := range []string{"Settings", "Recently Deleted", "Recently Deleted"} {
		if err := tt.Click(button); err != nil {
			t.Fatal(err)
		}
		q.settle(t)
		tt.Frame()
	}
	if w.settings != nil || w.trashOpen || w.selected != s[0].ID || w.shown() == nil {
		t.Fatalf("after leaving: settings %v, Recently Deleted %v, selected %q; want First shown",
			w.settings != nil, w.trashOpen, w.selected)
	}
}

func TestTranslationPrefsAreSaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	p := DefaultPrefs()
	p.CaptionsTranslate, p.CaptionsTo = true, "ja-JP"
	if err := SavePrefs(path, p); err != nil {
		t.Fatal(err)
	}
	if got := LoadPrefs(path); got != p {
		t.Fatalf("loaded %+v, want %+v", got, p)
	}
}

func TestPrefsFallBackToDefaultsForInvalidValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"captionsSource":"camera","captionsFrom":"ko-KR","captionsTo":"zh-CN","captionsDisplay":"stacked"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := LoadPrefs(path)
	d := DefaultPrefs()
	// Korean cannot be transcribed.
	if p.CaptionsSource != d.CaptionsSource || p.CaptionsFrom != d.CaptionsFrom || p.CaptionsDisplay != d.CaptionsDisplay {
		t.Fatalf("prefs %+v, want invalid values replaced by %+v", p, d)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if LoadPrefs(path) != d {
		t.Fatal("unreadable prefs did not give the defaults")
	}
}

func TestPrefsKeepTheDefaultTransparencyWhenUnsetOrInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	d := DefaultPrefs()
	// A file from before the setting existed: 0% would turn the bar solid.
	for _, body := range []string{`{"captionsFrom":"en-US","captionsTo":"zh-CN"}`, `{"captionsTransparency":150}`, `{"captionsTransparency":-1}`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := LoadPrefs(path).CaptionsTransparency; got != d.CaptionsTransparency {
			t.Errorf("%s: transparency %d, want the default %d", body, got, d.CaptionsTransparency)
		}
	}
}

func TestExplainOffersTheRightFix(t *testing.T) {
	if p := explain(errors.Join(errors.New("session: connect"), zoomai.ErrUnauthorized)); !p.setKey {
		t.Errorf("unauthorized: %+v", p)
	}
	if p := explain(&audio.PermissionError{What: "microphone", Pane: audio.MicrophonePane}); p.pane != audio.MicrophonePane {
		t.Errorf("permission: %+v", p)
	}
	if p := explain(&zoomai.RateLimitedError{}); p.setKey || p.pane != "" || p.message == "" {
		t.Errorf("rate limited: %+v", p)
	}
}

func TestClosingWhileStartingCancelsTheStart(t *testing.T) {
	canceled := false
	s := sessionSlot{starting: true, cancel: func() { canceled = true }}
	if !s.capturing() {
		t.Fatal("a starting session must count as capturing, or toggling starts a second one")
	}
	s.windowClosed()
	if !canceled {
		t.Fatal("closing the window did not cancel the start")
	}
	// The start completed anyway, racing the cancel: its run must stop.
	run := &fakeRun{snap: session.Snapshot{Status: session.StatusLive}}
	s.attach(run, nil)
	if !run.stopped || s.run != nil {
		t.Fatalf("run after close: stopped %v, attached %v", run.stopped, s.run != nil)
	}
}

func TestCaptionBarSwitchesTheSpokenLanguageWhileListening(t *testing.T) {
	a := testApp(t)
	run := &fakeRun{snap: session.Snapshot{Config: captionsCfg, Status: session.StatusLive, StartedAt: time.Now()}}
	b := &captionBar{app: a, cfg: captionsCfg, sessionSlot: sessionSlot{run: run}}
	tt := ui.NewTester(b.view, barWidth, barHeight)
	if err := tt.Click("Spoken Language"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("Auto-detect"); err != nil {
		t.Fatalf("%v; menu %q", err, tt.Menu())
	}
	if !slices.Equal(run.spoken, []string{"auto"}) || !tt.HasText("Mic · Auto-detect") {
		t.Fatalf("switched %v; texts %q", run.spoken, tt.Texts())
	}
	// The next captions start in the language chosen; the translation
	// preference stays.
	if got := LoadPrefs(a.prefsPath); got.CaptionsFrom != "auto" || got.CaptionsTo != DefaultPrefs().CaptionsTo {
		t.Fatalf("saved %s → %s, want auto → %s", got.CaptionsFrom, got.CaptionsTo, DefaultPrefs().CaptionsTo)
	}
	// Stopped, the session has nothing to switch.
	b.stopping = true
	tt.Frame()
	if err := tt.Click("Spoken Language"); err == nil {
		t.Fatal("a stopped session offers to switch its language")
	}
}

func TestRecordingWindowSwitchesTheSpokenLanguage(t *testing.T) {
	a := testApp(t)
	a.prefs.CaptionsFrom = "en-US"
	cfg := session.Config{Source: store.SourceMic, LangA: "en-US", LangB: "zh-CN"}
	run := &fakeRun{snap: session.Snapshot{SessionID: "s1", Config: cfg, Status: session.StatusLive, StartedAt: time.Now()}}
	a.captions = &captionBar{app: a, cfg: cfg, sessionSlot: sessionSlot{run: run}}
	summary := store.SessionSummary{ID: "s1", Mode: store.ModeCaptions, LangA: "en-US", LangB: "zh-CN", StartedAt: time.Now()}
	w := &mainWindow{app: a, split: sidebarWidth, loaded: true, sessions: []store.SessionSummary{summary}, selected: "s1",
		detail: &store.SessionDetail{SessionSummary: summary}}
	tt := ui.NewTester(w.view, 1040, 700)
	if err := tt.Click("Spoken language"); err != nil {
		t.Fatalf("%v; texts %q", err, tt.Texts())
	}
	if err := tt.ChooseMenuItem("日本語"); err != nil {
		t.Fatalf("%v; menu %q", err, tt.Menu())
	}
	if !slices.Equal(run.spoken, []string{"ja-JP"}) {
		t.Fatalf("switched %v", run.spoken)
	}
	// The next captions start in the new language, still untranslated.
	if got := LoadPrefs(a.prefsPath); got.CaptionsFrom != "ja-JP" || got.CaptionsTranslate {
		t.Fatalf("saved %+v, want ja-JP captions without translation", got)
	}
	// Off, it offers no target.
	if tt.HasText("中文 (简体)") {
		t.Fatalf("a target shown while translation is off: texts %q", tt.Texts())
	}
	// Its check box turns translation on for the recording and the next ones,
	// and shows the target chosen before.
	if err := tt.Click("Translate"); err != nil {
		t.Fatalf("%v; texts %q", err, tt.Texts())
	}
	if !run.snap.Config.Translate || !LoadPrefs(a.prefsPath).CaptionsTranslate {
		t.Fatalf("translate: session %v, saved %v", run.snap.Config.Translate, LoadPrefs(a.prefsPath).CaptionsTranslate)
	}
	if err := tt.Click("中文 (简体)"); err != nil { // the target pop-up
		t.Fatalf("%v; texts %q", err, tt.Texts())
	}
	if err := tt.ChooseMenuItem("Français"); err != nil {
		t.Fatalf("%v; menu %q", err, tt.Menu())
	}
	if run.snap.Config.LangB != "fr-FR" || LoadPrefs(a.prefsPath).CaptionsTo != "fr-FR" {
		t.Fatalf("target: session %s, saved %s", run.snap.Config.LangB, LoadPrefs(a.prefsPath).CaptionsTo)
	}
}

func TestRecordingLanguageControlsFitNarrowPanes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		width     int
		sidebar   float32
		inspector bool
	}{
		{"narrow window", 720, 280, false},
		{"wide sidebar", 720, sidebarMax, false},
		{"inspector open", 1040, sidebarWidth, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, ext := stubApp(t)
			cfg := session.Config{Source: store.SourceMic, LangA: "auto", LangB: "pt-BR", Translate: true}
			run := &fakeRun{snap: session.Snapshot{SessionID: "s1", Config: cfg, Status: session.StatusLive, StartedAt: time.Now()}}
			a.captions = &captionBar{app: a, cfg: cfg, sessionSlot: sessionSlot{run: run}}
			summary := store.SessionSummary{ID: "s1", Mode: store.ModeCaptions, LangA: cfg.LangA, LangB: cfg.LangB, StartedAt: run.snap.StartedAt}
			w := &mainWindow{app: a, split: tc.sidebar, loaded: true, sessions: []store.SessionSummary{summary}, selected: "s1",
				detail: &store.SessionDetail{SessionSummary: summary}}
			tt := ui.NewTester(w.view, 1040, 700)
			widths := make(map[string]float32)
			for _, label := range []string{"Spoken language", "Translate to", "Translation language"} {
				r, ok := tt.Find(label)
				if !ok {
					t.Fatalf("missing %s", label)
				}
				widths[label] = r.W
			}
			if tc.inspector {
				ext.inspector = &addon.Pane{Width: 420, MinWidth: 280, MaxWidth: 420}
			}
			tt.SetSize(tc.width, 700)
			snapshot(t, tt, "recording-controls-"+strings.ReplaceAll(tc.name, " ", "-"))
			right := float32(tc.width)
			if tc.inspector {
				r, ok := tt.Find("slot:inspector")
				if !ok {
					t.Fatal("missing inspector")
				}
				right = r.X
			}
			for label, width := range widths {
				r, ok := tt.Find(label)
				if !ok || r.W < width || r.X+r.W > right {
					t.Errorf("%s clipped: %+v, found %v; want width %v within pane ending at %v", label, r, ok, width, right)
				}
			}
			if err := tt.Click("Translation language"); err != nil {
				t.Fatal(err)
			}
			if err := tt.ChooseMenuItem("Français"); err != nil {
				t.Fatal(err)
			}
			if run.snap.Config.LangB != "fr-FR" {
				t.Fatalf("translation target %q, want fr-FR", run.snap.Config.LangB)
			}
		})
	}
}

func TestSettingsTurnsTranslationOnAndOffAndKeepsItsLanguage(t *testing.T) {
	t.Setenv("ZOOM_API_KEY", "from-env") // keep the test away from the real Keychain
	a := testApp(t)
	w := &settingsPage{app: a, keyState: readKeyState()}
	tt := ui.NewTester(settingsShown(w).view, 1040, 900)
	snapshot(t, tt, "settings-translate-off")
	saved := func() Prefs { return LoadPrefs(a.prefsPath) }
	if !slices.ContainsFunc(tt.Texts(), func(s string) bool { return strings.HasPrefix(s, "Captions only.") }) {
		t.Fatalf("translation off by default: texts %q", tt.Texts())
	}
	// The language can be chosen while translation is off, and stays off.
	if err := tt.Click("中文 (简体)"); err != nil { // the language pop-up
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("日本語"); err != nil {
		t.Fatalf("%v; menu %q", err, tt.Menu())
	}
	if p := saved(); p.CaptionsTo != "ja-JP" || p.CaptionsTranslate {
		t.Fatalf("saved %+v, want 日本語 chosen, translation still off", p)
	}
	// The check box turns translation on and off; the language stays.
	for _, on := range []bool{true, false} {
		if err := tt.Click("Translate to"); err != nil {
			t.Fatal(err)
		}
		if p := saved(); p.CaptionsTranslate != on || p.CaptionsTo != "ja-JP" {
			t.Fatalf("saved %+v, want translate %v into ja-JP", p, on)
		}
	}
	// A change of spoken language changes neither.
	if err := tt.Click("Auto-detect"); err != nil { // the Spoken language pop-up
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("日本語"); err != nil {
		t.Fatalf("%v; menu %q", err, tt.Menu())
	}
	if p := saved(); p.CaptionsFrom != "ja-JP" || p.CaptionsTo != "ja-JP" || p.CaptionsTranslate {
		t.Fatalf("saved %+v", p)
	}
}

func TestTranslatePillTogglesTheRecordingAndTheNextOnes(t *testing.T) {
	a := testApp(t)
	cfg := session.Config{Source: store.SourceMic, LangA: "en-US", LangB: "ja-JP"}
	run := &fakeRun{snap: session.Snapshot{Config: cfg, Status: session.StatusLive, StartedAt: time.Now()}}
	b := &captionBar{app: a, cfg: cfg, sessionSlot: sessionSlot{run: run}}
	a.captions = b
	tt := ui.NewTester(b.view, barWidth, barHeight)
	snapshot(t, tt, "captions-translate-off")
	for _, on := range []bool{true, false, true} {
		if err := tt.Click("Translate"); err != nil {
			t.Fatal(err)
		}
		if run.snap.Config.Translate != on || b.cfg.Translate != on || LoadPrefs(a.prefsPath).CaptionsTranslate != on {
			t.Fatalf("after a click: session %v, bar %v, saved %v; want %v",
				run.snap.Config.Translate, b.cfg.Translate, LoadPrefs(a.prefsPath).CaptionsTranslate, on)
		}
	}
	snapshot(t, tt, "captions-translate-on")
	// ⌥⌘T does what the pill does.
	a.ToggleTranslation()
	if run.snap.Config.Translate || LoadPrefs(a.prefsPath).CaptionsTranslate {
		t.Fatal("⌥⌘T did not turn translation off")
	}
	// A stopped session keeps what it had.
	b.stopping = true
	a.ToggleTranslation()
	if run.snap.Config.Translate {
		t.Fatal("a stopped session turned translation on")
	}
}

func TestTranscriptsNameSpeakersWhenSeveralSpoke(t *testing.T) {
	lines := []session.Line{
		{Seq: 0, Lang: "en-US", Text: "Shall we start?", Speaker: "Speaker 1"},
		{Seq: 1, Lang: "en-US", Text: "The budget first.", Speaker: "Speaker 1"},
		{Seq: 2, Lang: "en-US", Text: "Agreed.", Speaker: "Speaker 2"},
	}
	count := func(tt *ui.Tester, text string) int {
		n := 0
		for _, s := range tt.Texts() {
			if s == text {
				n++
			}
		}
		return n
	}
	cfg := session.Config{Source: store.SourceMic, LangA: "en-US", LangB: "en-US"}
	run := &fakeRun{snap: session.Snapshot{Config: cfg, Status: session.StatusLive, Lines: lines}}
	b := &captionBar{app: testApp(t), cfg: cfg, sessionSlot: sessionSlot{run: run}}
	tt := ui.NewTester(b.view, barWidth, 400)
	snapshot(t, tt, "captions-speakers")
	// Each speaker heads the turns they said in a row.
	if count(tt, "Speaker 1") != 1 || count(tt, "Speaker 2") != 1 {
		t.Fatalf("caption bar: texts %q", tt.Texts())
	}
	one := slices.Clone(lines[:2])
	run.snap.Lines = one
	tt.Frame()
	if tt.HasText("Speaker 1") {
		t.Fatalf("one speaker is named: texts %q", tt.Texts())
	}

	started := time.Date(2026, 10, 7, 9, 30, 0, 0, time.Local)
	summary := store.SessionSummary{ID: "s1", Mode: store.ModeCaptions, LangA: "auto", LangB: "zh-CN",
		RecapStatus: store.RecapReady, StartedAt: started, EndedAt: started.Add(time.Minute), SegmentCount: 3}
	w := &mainWindow{app: testApp(t), split: sidebarWidth, sessions: []store.SessionSummary{summary}, selected: "s1", loaded: true,
		detail: &store.SessionDetail{SessionSummary: summary, Recap: "# Sync", Segments: []store.Segment{
			{ID: "g1", Lang: "zh-CN", Text: "开始吧。", Speaker: "Speaker 1"},
			{ID: "g2", Lang: "en-US", Text: "Sure.", Speaker: "Speaker 2"},
			{ID: "g3", Lang: "en-US", Text: "Thanks.", Speaker: "Speaker 1"},
		}}}
	tt = ui.NewTester(w.view, 1040, 700)
	if err := tt.Click("Transcript"); err != nil {
		t.Fatal(err)
	}
	if count(tt, "Speaker 1") != 2 || count(tt, "Speaker 2") != 1 {
		t.Fatalf("transcript: texts %q", tt.Texts())
	}
	// Session Info names the languages detected, not Auto-detect.
	tt.Key(ui.Cmd, ui.KeyI)
	if !tt.HasText("中文 (简体), English") {
		t.Fatalf("Session Info: texts %q", tt.Texts())
	}
}

func TestTrayMenuFollowsTheTranslationPreference(t *testing.T) {
	a := testApp(t)
	item := func(label string) *mygo.MenuItem {
		for _, it := range a.trayMenu().Items() {
			if it.Label == label {
				return it
			}
		}
		t.Fatalf("menu lacks %q", label)
		return nil
	}
	if item("Translate").Checked {
		t.Fatal("Translate is checked while off")
	}
	a.prefs.CaptionsTranslate, a.prefs.CaptionsTo = true, "ko-KR"
	if !item("Translate").Checked {
		t.Fatal("Translate is unchecked while on")
	}
	var checked []string
	for _, it := range item("Translate To").Submenu {
		if it.Checked {
			checked = append(checked, it.Label)
		}
	}
	if !slices.Equal(checked, []string{"한국어"}) {
		t.Fatalf("Translate To checks %q, want 한국어 alone", checked)
	}
}

func TestTranslationTurnedOnWhileConnectingReachesTheSession(t *testing.T) {
	a := testApp(t)
	b := &captionBar{app: a, cfg: session.Config{Source: store.SourceMic, LangA: "en-US", LangB: "ja-JP"},
		sessionSlot: sessionSlot{starting: true}}
	a.captions = b
	a.SetTranslate(true)
	run := &fakeRun{snap: session.Snapshot{Config: session.Config{LangA: "en-US", LangB: "ja-JP"}, Status: session.StatusLive}}
	b.attach(run, nil)
	b.attached()
	if !run.snap.Config.Translate {
		t.Fatal("the session connected without the translation turned on meanwhile")
	}
}

func TestTargetChosenWhileConnectingReachesTheSession(t *testing.T) {
	a := testApp(t)
	b := &captionBar{app: a, cfg: session.Config{Source: store.SourceMic, LangA: "en-US", LangB: "ja-JP", Translate: true},
		sessionSlot: sessionSlot{starting: true}}
	a.captions = b
	a.setTarget("de-DE")
	run := &fakeRun{snap: session.Snapshot{Config: session.Config{LangA: "en-US", LangB: "ja-JP", Translate: true}, Status: session.StatusLive}}
	b.attach(run, nil)
	b.attached()
	if run.snap.Config.LangB != "de-DE" {
		t.Fatalf("the session connected translating into %s, want the de-DE chosen meanwhile", run.snap.Config.LangB)
	}
}

func TestSettingsChangesTheTargetOfTheRecording(t *testing.T) {
	t.Setenv("ZOOM_API_KEY", "from-env") // keep the test away from the real Keychain
	a := testApp(t)
	a.prefs.CaptionsTranslate = true
	cfg := session.Config{Source: store.SourceMic, LangA: "en-US", LangB: "zh-CN", Translate: true}
	run := &fakeRun{snap: session.Snapshot{Config: cfg, Status: session.StatusLive, StartedAt: time.Now()}}
	b := &captionBar{app: a, cfg: cfg, sessionSlot: sessionSlot{run: run}}
	a.captions = b
	w := &settingsPage{app: a, keyState: readKeyState()}
	tt := ui.NewTester(settingsShown(w).view, 1040, 900)
	if err := tt.Click("中文 (简体)"); err != nil { // the language pop-up
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("日本語"); err != nil {
		t.Fatalf("%v; menu %q", err, tt.Menu())
	}
	if run.snap.Config.LangB != "ja-JP" || b.cfg.LangB != "ja-JP" || LoadPrefs(a.prefsPath).CaptionsTo != "ja-JP" {
		t.Fatalf("target: session %s, bar %s, saved %s; want ja-JP for all",
			run.snap.Config.LangB, b.cfg.LangB, LoadPrefs(a.prefsPath).CaptionsTo)
	}
	// The pill names the new target at once.
	bar := ui.NewTester(b.view, barWidth, barHeight)
	if !bar.HasText("日本語") {
		t.Fatalf("caption bar: texts %q", bar.Texts())
	}
	// A stopped session keeps its target; the next recording takes the new one.
	b.stopping = true
	a.setTarget("de-DE")
	if run.snap.Config.LangB != "ja-JP" || LoadPrefs(a.prefsPath).CaptionsTo != "de-DE" {
		t.Fatalf("after stop: session %s, saved %s", run.snap.Config.LangB, LoadPrefs(a.prefsPath).CaptionsTo)
	}
}

func TestCaptionBarAfterTranslationTurnedOff(t *testing.T) {
	a := testApp(t)
	a.prefs.CaptionsDisplay = DisplaySideBySide
	cfg := session.Config{Source: store.SourceMic, LangA: "en-US", LangB: "ja-JP"} // turned off
	run := &fakeRun{snap: session.Snapshot{Config: cfg, Status: session.StatusLive, Lines: []session.Line{
		{Seq: 0, Lang: "en-US", Text: "Said while on.", Translation: "オンの間に。", TranslationLang: "ja-JP"},
		{Seq: 1, StartMs: 3000, Lang: "en-US", Text: "Said after."},
	}}}
	b := &captionBar{app: a, cfg: cfg, sessionSlot: sessionSlot{run: run}}
	tt := ui.NewTester(b.view, barWidth, 300)
	// The line translated before keeps its column; the one after spans the bar.
	said, _ := tt.Find("Said while on.")
	trans, _ := tt.Find("オンの間に。")
	if trans.X < said.X+said.W || trans.Y > said.Y+said.H {
		t.Fatalf("translation at %+v, want it right of %+v", trans, said)
	}
	if !tt.HasText("Said after.") {
		t.Fatalf("texts %q", tt.Texts())
	}
}

func TestCaptionsInTheTargetLanguageShowAsCaptions(t *testing.T) {
	draftHeight := func(cfg session.Config) float32 {
		run := &fakeRun{snap: session.Snapshot{Config: cfg, Status: session.StatusLive, Draft: "話しています"}}
		b := &captionBar{app: testApp(t), cfg: cfg, sessionSlot: sessionSlot{run: run}}
		r, ok := ui.NewTester(b.view, barWidth, barHeight).Find("話しています")
		if !ok {
			t.Fatal("no draft")
		}
		return r.H
	}
	translated := draftHeight(session.Config{LangA: "en-US", LangB: "ja-JP", Translate: true})
	// Spoken in the target, nothing is translated: the draft is as large as
	// captions are, not as what is said under a translation.
	if same := draftHeight(session.Config{LangA: "ja-JP", LangB: "ja-JP", Translate: true}); same <= translated {
		t.Fatalf("draft %v high, want taller than under a translation (%v)", same, translated)
	}
}

func TestSettingsSavesTheProfileForHighlightsAndAsk(t *testing.T) {
	t.Setenv("ZOOM_API_KEY", "from-env") // keep the test away from the real Keychain
	a := testApp(t)
	a.profile = profile.Profile{Name: "Alex"}
	w := &settingsPage{app: a, keyState: readKeyState()}
	tt := ui.NewTester(settingsShown(w).view, 1040, 900)
	field := func(label string) {
		t.Helper()
		r, ok := tt.Find(label)
		if !ok {
			t.Fatalf("no row %q; texts %q", label, tt.Texts())
		}
		tt.ClickAt(800, r.Y+r.H/2) // the field, at the row's right end
		tt.Frame()
	}
	field("Your name")
	tt.Key(ui.Cmd, ui.KeyA)
	tt.Type("Alex Rivera")
	tt.Frame()
	field("Also called")
	tt.Type("Capy team")
	tt.Key(0, ui.KeyEnter)
	tt.Frame()
	saved := profile.Read(a.profilePath)
	if saved.Name != "Alex Rivera" || !slices.Equal(saved.Aliases, []string{"Capy team"}) {
		t.Fatalf("saved %+v", saved)
	}
	if a.profile.Name != "Alex Rivera" {
		t.Fatalf("the app holds %+v, not what it saved", a.profile)
	}
}
