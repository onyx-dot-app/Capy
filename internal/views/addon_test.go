package views

import (
	"context"
	"fmt"
	"image/color"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/features"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/store/sqlite"
	"github.com/onyx-dot-app/Capy/internal/views/addon"
)

// stubAddOn fills every slot with something labelled, to find where the
// views put it.
type stubAddOn struct {
	inspector, panel *addon.Pane // nil: none
	toolbarFor       *addon.Session
	shortcuts        int
	resized          []float32
	batchChosen      []addon.Batch
	asks             bool     // CanAsk's answer
	asked            []string // "session|question|bar" per Ask
}

func (s *stubAddOn) Start(addon.Host) {}

func (s *stubAddOn) Toolbar(c *ui.Context, sess *addon.Session) {
	s.toolbarFor = sess
	ui.Box(c).Size(30, 28).Label("slot:toolbar")
}

func (s *stubAddOn) Shortcuts(c *ui.Context, _ *addon.Session) {
	if c.Shortcut(ui.Cmd, ui.KeyJ) {
		s.shortcuts++
	}
}

func (s *stubAddOn) Inspector(c *ui.Context, _ *addon.Session) *addon.Pane {
	if s.inspector == nil {
		return nil
	}
	p := *s.inspector
	p.View = func() { ui.Box(c).Fill().Label("slot:inspector") }
	p.Resized = func(w float32) { s.resized = append(s.resized, w) }
	return &p
}

func (s *stubAddOn) CaptionHeader(c *ui.Context, _ *addon.Session) {
	ui.Box(c).Size(40, 20).Label("slot:caption-header")
}

func (s *stubAddOn) CaptionPanel(c *ui.Context, _ *addon.Session) *addon.Pane {
	if s.panel == nil {
		return nil
	}
	p := *s.panel
	p.View = func() { ui.Box(c).Fill().Label("slot:caption-panel") }
	p.Resized = func(w float32) { s.resized = append(s.resized, w) }
	return &p
}

func (s *stubAddOn) Settings(c *ui.Context) { ui.Text(c, "slot:settings") }

func (s *stubAddOn) HighlightsSettings(c *ui.Context) { ui.Text(c, "slot:highlights-settings") }

func (s *stubAddOn) Highlighted(string, bool) {}

func (s *stubAddOn) Export(sess addon.Session) []string {
	return []string{"## slot:export\n\nFor " + sess.Title}
}

func (s *stubAddOn) BatchMenu(m *ui.Menu, b addon.Batch) {
	if m.Item("slot:batch " + b.Name).Chosen() {
		s.batchChosen = append(s.batchChosen, b)
	}
}

func (s *stubAddOn) CanAsk() bool { return s.asks }

func (s *stubAddOn) Ask(sess addon.Session, question string, bar bool) {
	s.asked = append(s.asked, fmt.Sprintf("%s|%s|%v", sess.ID, question, bar))
}

// fakeFrame is a window's frame on a display, as docking sees it.
type fakeFrame struct {
	bounds, area mygo.Rectangle
	sets         int
	maximized    bool
}

func (f *fakeFrame) Bounds() mygo.Rectangle     { return f.bounds }
func (f *fakeFrame) SetBounds(r mygo.Rectangle) { f.bounds, f.sets = r, f.sets+1 }
func (f *fakeFrame) WorkArea() mygo.Rectangle   { return f.area }
func (f *fakeFrame) IsMaximized() bool          { return f.maximized }
func (f *fakeFrame) IsFullScreen() bool         { return false }

func stubApp(t *testing.T) (*App, *stubAddOn) {
	t.Helper()
	a := testApp(t)
	ext := &stubAddOn{}
	a.ext = ext
	return a, ext
}

// endedWindow is Capy's window showing an ended session, Launch rehearsal.
func endedWindow(a *App) *mainWindow {
	started := time.Date(2026, 10, 7, 9, 30, 0, 0, time.Local)
	summary := store.SessionSummary{ID: "s1", Mode: store.ModeCaptions, LangA: "en-US", LangB: "zh-CN",
		Title: "Launch rehearsal", RecapStatus: store.RecapReady, StartedAt: started, EndedAt: started.Add(95 * time.Second)}
	return &mainWindow{app: a, split: sidebarWidth, sessions: []store.SessionSummary{summary}, selected: "s1", loaded: true,
		detail: &store.SessionDetail{SessionSummary: summary, Recap: "# Launch rehearsal\n\nShip on Friday.",
			Segments: []store.Segment{{ID: "g1", Lang: "en-US", Text: "We ship Friday.", StartMs: 42_000, EndMs: 44_000}}}}
}

func near(got, want float32) bool { return math.Abs(float64(got-want)) <= 1.5 }

func TestAddOnSlotsShowAtTheirPlacesInTheWindow(t *testing.T) {
	a, ext := stubApp(t)
	w := endedWindow(a)
	tt := ui.NewTester(w.view, 1040, 700)

	// Toolbar: in the toolbar's row, left of Record.
	slot, ok := tt.Find("slot:toolbar")
	record, _ := tt.Find("Record")
	if !ok || slot.X+slot.W > record.X || slot.Y+slot.H/2 < record.Y || slot.Y+slot.H/2 > record.Y+record.H {
		t.Fatalf("toolbar slot at %+v, %v; want it left of Record at %+v", slot, ok, record)
	}
	if s := ext.toolbarFor; s == nil || s.ID != "s1" || s.Title != "Launch rehearsal" || s.Live {
		t.Errorf("toolbar slot given %+v, want the session in view", s)
	}
	// Shortcuts: the window's keys reach the add-on.
	tt.Key(ui.Cmd, ui.KeyJ)
	if ext.shortcuts != 1 {
		t.Errorf("⌘J reached the add-on %d times, want 1", ext.shortcuts)
	}
	// Export: after the transcript, in Copy as Markdown.
	if err := tt.Click("Copy as Markdown"); err != nil {
		t.Fatal(err)
	}
	md := tt.Clipboard()
	if tr, ex := strings.Index(md, "## Transcript"), strings.Index(md, "## slot:export\n\nFor Launch rehearsal"); tr < 0 || ex < tr {
		t.Errorf("copied Markdown lacks the add-on's section after the transcript:\n%s", md)
	}
	// Inspector: none until the add-on gives one, then on the right.
	if _, ok := tt.Find("slot:inspector"); ok {
		t.Fatal("an inspector shows that the add-on did not give")
	}
	ext.inspector = &addon.Pane{Width: 320, MinWidth: 280, MaxWidth: 420}
	tt.Frame()
	snapshot(t, tt, "window-addon-slots")
	insp, ok := tt.Find("slot:inspector")
	title, _ := tt.Find("Launch rehearsal")
	if !ok || !near(insp.W, 320) || !near(insp.X+insp.W, 1040) || title.X+title.W > insp.X {
		t.Fatalf("inspector at %+v, %v; want 320 wide on the right, the title %+v left of it", insp, ok, title)
	}
	// Record stays in the detail's toolbar, left of the inspector.
	if rec, ok := tt.Find("Record"); !ok || rec.X+rec.W > insp.X {
		t.Errorf("Record at %+v, %v; want it left of the inspector at x=%v", rec, ok, insp.X)
	}
}

func TestInspectorWidthIsClampedAndKeepsTheDetailWide(t *testing.T) {
	for _, tc := range []struct {
		name          string
		want, window  float32
		width         float32
		detailAtLeast float32
	}{
		{"its own", 320, 1400, 320, detailMin},
		{"at most its max", 500, 1400, 420, detailMin},
		{"at least its min", 100, 1400, 280, detailMin},
		// 1040 less the sidebar and two dividers leaves 778: 420 for the
		// detail, 358 for the inspector.
		{"narrowed for the detail", 400, 1040, 358, detailMin},
		{"not below its min", 320, 900, 280, 0},
	} {
		a, ext := stubApp(t)
		w := endedWindow(a)
		ext.inspector = &addon.Pane{Width: tc.want, MinWidth: 280, MaxWidth: 420}
		tt := ui.NewTester(w.view, int(tc.window), 700)
		insp, ok := tt.Find("slot:inspector")
		detail := insp.X - sidebarWidth - 2
		if !ok || !near(insp.W, tc.width) || detail < tc.detailAtLeast-1.5 {
			t.Errorf("%s: inspector %+v, %v, detail %v wide; want it %v wide, the detail at least %v",
				tc.name, insp, ok, detail, tc.width, tc.detailAtLeast)
		}
	}
}

func TestInspectorDividerResizesWithinItsBounds(t *testing.T) {
	a, ext := stubApp(t)
	w := endedWindow(a)
	ext.inspector = &addon.Pane{Width: 320, MinWidth: 280, MaxWidth: 420}
	tt := ui.NewTester(w.view, 1400, 700)
	insp, _ := tt.Find("slot:inspector")
	for _, tc := range []struct{ dx, want float32 }{{-60, 380}, {-300, 420}, {300, 280}} {
		x := insp.X - 1 // the divider, right of the detail
		tt.Press(x, 300)
		tt.Move(x+tc.dx, 300)
		tt.Release(x+tc.dx, 300)
		if n := len(ext.resized); n == 0 || !near(ext.resized[n-1], tc.want) {
			t.Fatalf("dragged by %v: resized to %v, want %v", tc.dx, ext.resized, tc.want)
		}
		ext.resized = nil
	}
}

// moveWindow runs the frame change docking posted, if any, and has tt
// lay out at the size the window takes, as the system would.
func moveWindow(tt *ui.Tester, q posted, f *fakeFrame) {
	select {
	case fn := <-q:
		fn()
		tt.SetSize(f.bounds.Width, f.bounds.Height)
	default:
	}
}

func TestInspectorGrowsTheWindowAndClosingRestoresIt(t *testing.T) {
	open := func(f *fakeFrame) (*stubAddOn, func(on bool)) {
		a, ext := stubApp(t)
		w := endedWindow(a)
		q := make(posted, 1)
		w.inspector.frame, w.inspector.min, w.inspector.post = f, mygo.Rectangle{Width: 720, Height: 460}, q.post
		tt := ui.NewTester(w.view, f.bounds.Width, f.bounds.Height)
		return ext, func(on bool) {
			ext.inspector = nil
			if on {
				ext.inspector = &addon.Pane{Width: 320, MinWidth: 280, MaxWidth: 420}
			}
			tt.Frame()
			moveWindow(tt, q, f)
		}
	}
	area := mygo.Rectangle{X: 0, Y: 25, Width: 1600, Height: 975}

	// Room on the right: the window grows by the inspector, the detail
	// keeps its width.
	f := &fakeFrame{bounds: mygo.Rectangle{X: 100, Y: 100, Width: 1040, Height: 700}, area: area}
	_, toggle := open(f)
	toggle(true)
	if want := (mygo.Rectangle{X: 100, Y: 100, Width: 1360, Height: 700}); f.bounds != want {
		t.Fatalf("opened: frame %+v, want %+v", f.bounds, want)
	}
	toggle(true) // another frame with the inspector open changes nothing
	toggle(false)
	if want := (mygo.Rectangle{X: 100, Y: 100, Width: 1040, Height: 700}); f.bounds != want || f.sets != 2 {
		t.Fatalf("closed: frame %+v after %d changes, want %+v after 2", f.bounds, f.sets, want)
	}

	// Room only further left: the window moves to grow; a drag meanwhile
	// stays as the inspector closes.
	f = &fakeFrame{bounds: mygo.Rectangle{X: 500, Y: 100, Width: 1040, Height: 700}, area: area}
	_, toggle = open(f)
	toggle(true)
	if want := (mygo.Rectangle{X: 240, Y: 100, Width: 1360, Height: 700}); f.bounds != want {
		t.Fatalf("opened at the right edge: frame %+v, want %+v", f.bounds, want)
	}
	f.bounds.Y = 160
	toggle(false)
	if want := (mygo.Rectangle{X: 500, Y: 160, Width: 1040, Height: 700}); f.bounds != want {
		t.Fatalf("closed after a drag: frame %+v, want %+v", f.bounds, want)
	}

	// Narrowed by the user meanwhile, it shrinks back no narrower than its
	// minimum; maximized meanwhile, it keeps its frame.
	f = &fakeFrame{bounds: mygo.Rectangle{X: 100, Y: 100, Width: 1040, Height: 700}, area: area}
	ext, toggle := open(f)
	_ = ext
	toggle(true)
	f.bounds.Width = 900
	toggle(false)
	if f.bounds.Width != 720 {
		t.Fatalf("closed after narrowing to 900: width %d, want the minimum 720", f.bounds.Width)
	}
	toggle(true)
	f.maximized, f.bounds = true, area
	sets := f.sets
	toggle(false)
	if f.sets != sets {
		t.Fatalf("closing the inspector of a maximized window changed its frame to %+v", f.bounds)
	}

	// No room: the window keeps its frame and the detail narrows.
	f = &fakeFrame{bounds: mygo.Rectangle{X: 0, Y: 25, Width: 1100, Height: 700}, area: mygo.Rectangle{X: 0, Y: 25, Width: 1280, Height: 775}}
	_, toggle = open(f)
	toggle(true)
	toggle(false)
	if f.sets != 0 {
		t.Fatalf("a window with no room to grow changed its frame %d times", f.sets)
	}
}

func TestInspectorShowsOnlyOnceTheWindowHasItsNewFrame(t *testing.T) {
	// A frame lays out at the size the window had as it began: the frame
	// that opens or closes the inspector shows what the last did, and the
	// window takes its new frame after it, for the next frame to lay out.
	a, ext := stubApp(t)
	w := endedWindow(a)
	q := make(posted, 1)
	f := &fakeFrame{bounds: mygo.Rectangle{X: 100, Y: 100, Width: 1040, Height: 700},
		area: mygo.Rectangle{X: 0, Y: 25, Width: 1600, Height: 975}}
	w.inspector.frame, w.inspector.min, w.inspector.post = f, mygo.Rectangle{Width: 720, Height: 460}, q.post
	tt := ui.NewTester(w.view, 1040, 700)
	title, _ := tt.Find("Launch rehearsal")

	ext.inspector = &addon.Pane{Width: 320, MinWidth: 280, MaxWidth: 420}
	tt.Frame()
	if _, ok := tt.Find("slot:inspector"); ok || f.sets != 0 {
		t.Fatalf("the frame opening the inspector shows it, the frame changed %d times; want neither", f.sets)
	}
	moveWindow(tt, q, f)
	insp, ok := tt.Find("slot:inspector")
	if again, _ := tt.Find("Launch rehearsal"); !ok || !near(insp.W, 320) || !near(insp.X+insp.W, 1360) || again != title {
		t.Fatalf("grown to %+v: inspector at %+v, %v, title %+v; want it 320 wide on the right, the title still at %+v",
			f.bounds, insp, ok, again, title)
	}

	ext.inspector = nil
	tt.Frame()
	if now, ok := tt.Find("slot:inspector"); !ok || now != insp || f.bounds.Width != 1360 {
		t.Fatalf("the frame closing the inspector: inspector at %+v, %v, frame %+v; want both as they were", now, ok, f.bounds)
	}
	moveWindow(tt, q, f)
	if _, ok := tt.Find("slot:inspector"); ok || f.bounds.Width != 1040 {
		t.Fatalf("closed: frame %+v, inspector shown %v; want 1040 wide without it", f.bounds, ok)
	}

	// Closing the window meanwhile restores its frame at once: the frame
	// change posted before does nothing.
	ext.inspector = &addon.Pane{Width: 320, MinWidth: 280, MaxWidth: 420}
	tt.Frame()
	w.inspector.restore()
	sets := f.sets
	(<-q)()
	if f.sets != sets || f.bounds.Width != 1040 {
		t.Fatalf("a window closed before it grew: frame %+v after %d more changes", f.bounds, f.sets-sets)
	}
}

func liveBar(a *App) *captionBar {
	run := &fakeRun{snap: session.Snapshot{SessionID: "s1", Config: captionsCfg, Status: session.StatusLive, StartedAt: time.Now()}}
	b := &captionBar{app: a, cfg: captionsCfg, sessionSlot: sessionSlot{run: run}}
	a.captions = b
	return b
}

func TestCaptionBarShowsTheAddOnsHeaderAndPanel(t *testing.T) {
	a, ext := stubApp(t)
	b := liveBar(a)
	f := &fakeFrame{bounds: mygo.Rectangle{X: 420, Y: 800, Width: barWidth, Height: barHeight},
		area: mygo.Rectangle{X: 0, Y: 25, Width: 1600, Height: 975}}
	q := make(posted, 1)
	b.panel.frame, b.panel.post = f, q.post
	tt := ui.NewTester(b.view, barWidth, barHeight)
	slot, ok := tt.Find("slot:caption-header")
	stop, _ := tt.Find("Stop")
	if !ok || slot.X+slot.W > stop.X {
		t.Fatalf("caption header slot at %+v, %v; want it left of Stop at %+v", slot, ok, stop)
	}
	if s := (host{a}).LiveSession(); s == nil || s.ID != "s1" || !s.Live {
		t.Errorf("live session %+v, want s1", s)
	}

	// The panel widens the bar on the right and grows it to its height,
	// keeping the bottom edge; the bar's own controls stay left of it.
	// It shows once the bar has its new frame.
	ext.panel = &addon.Pane{Width: 320, MinWidth: 320, MaxWidth: 320, MinHeight: 340}
	tt.Frame()
	if _, ok := tt.Find("slot:caption-panel"); ok {
		t.Fatal("the frame docking the panel shows it in the bar's old frame")
	}
	moveWindow(tt, q, f)
	if want := (mygo.Rectangle{X: 420, Y: 636, Width: barWidth + 320, Height: 340}); f.bounds != want {
		t.Fatalf("docked: frame %+v, want %+v", f.bounds, want)
	}
	snapshot(t, tt, "captions-addon-panel")
	panel, ok := tt.Find("slot:caption-panel")
	stop, _ = tt.Find("Stop")
	// Inside its 1-DIP border.
	if !ok || !near(panel.X, barWidth+1) || !near(panel.X+panel.W, barWidth+320-1) || stop.X+stop.W > panel.X {
		t.Fatalf("panel at %+v, %v, Stop at %+v; want the panel 320 wide on the right, Stop left of it", panel, ok, stop)
	}
	ext.panel = nil
	tt.Frame()
	moveWindow(tt, q, f)
	if want := (mygo.Rectangle{X: 420, Y: 800, Width: barWidth, Height: barHeight}); f.bounds != want {
		t.Fatalf("closed: frame %+v, want %+v", f.bounds, want)
	}
}

func TestCaptionPanelDividerResizesWithinItsBounds(t *testing.T) {
	a, ext := stubApp(t)
	b := liveBar(a)
	ext.panel = &addon.Pane{Width: 320, MinWidth: 280, MaxWidth: 720, MinHeight: 340}
	tt := ui.NewTester(b.view, barWidth+320, 340)
	panel, _ := tt.Find("slot:caption-panel")
	for _, tc := range []struct{ dx, want float32 }{{-200, 520}, {-600, 720}, {200, 280}} {
		x := panel.X - 2 // the divider, left of the panel's border
		tt.Press(x, 200)
		tt.Move(x+tc.dx, 200)
		tt.Release(x+tc.dx, 200)
		if n := len(ext.resized); n == 0 || !near(ext.resized[n-1], tc.want) {
			t.Fatalf("dragged by %v: resized to %v, want %v", tc.dx, ext.resized, tc.want)
		}
		ext.resized = nil
	}
}

func TestCaptionPanelNarrowsToKeepTheBarWide(t *testing.T) {
	a, ext := stubApp(t)
	b := liveBar(a)
	ext.panel = &addon.Pane{Width: 720, MinWidth: 280, MaxWidth: 720, MinHeight: 340}
	const window = barWidth + 320
	tt := ui.NewTester(b.view, window, 340)
	panel, ok := tt.Find("slot:caption-panel")
	stop, _ := tt.Find("Stop")
	// The bar and the divider keep barMin+1; the panel takes the rest.
	if !ok || panel.X < barMin+1 || !near(panel.X+panel.W, window-1) || stop.X+stop.W > panel.X {
		t.Fatalf("panel at %+v, %v, Stop at %+v; want the bar kept %v wide", panel, ok, stop, barMin)
	}
}

func TestCaptionPanelDocksLeftWithNoRoomOnTheRight(t *testing.T) {
	area := mygo.Rectangle{X: 0, Y: 25, Width: 1600, Height: 975}
	p := &addon.Pane{Width: 320, MinHeight: 340}
	got, ok := docked(false)(mygo.Rectangle{X: 800, Y: 800, Width: barWidth, Height: barHeight}, area, p)
	if want := (mygo.Rectangle{X: 480, Y: 636, Width: barWidth + 320, Height: 340}); !ok || got != want {
		t.Fatalf("docked %+v, %v; want %+v", got, ok, want)
	}
	// A bar as tall as the panel needs keeps its height.
	got, _ = docked(false)(mygo.Rectangle{X: 0, Y: 100, Width: barWidth, Height: 400}, area, p)
	if got.Y != 100 || got.Height != 400 {
		t.Fatalf("a tall bar: %+v", got)
	}
	// A bar about as wide as the screen narrows to keep the pane on it.
	got, _ = docked(false)(mygo.Rectangle{X: 200, Y: 800, Width: 1500, Height: barHeight}, area, p)
	if got.X < area.X || got.X+got.Width > area.X+area.Width {
		t.Fatalf("a wide bar docked off the screen: %+v in %+v", got, area)
	}
	if _, ok := docked(true)(mygo.Rectangle{Width: 1600, Height: 975}, area, p); ok {
		t.Fatal("a maximized bar changed its frame")
	}
}

func TestSettingsShowTheAddOnsSectionAfterCaptionsAndHighlights(t *testing.T) {
	t.Setenv("ZOOM_API_KEY", "from-env") // keep the test away from the real Keychain
	a, _ := stubApp(t)
	a.gate = gate{features.Highlights: true}
	w := &settingsPage{app: a, keyState: readKeyState()}
	tt := ui.NewTester(settingsShown(w).view, 1040, 1600)
	slot, ok := tt.Find("slot:settings")
	captions, _ := tt.Find("0% is a solid bar; at 100% only the captions show.")
	highlights, hok := tt.Find("Highlight what matters while recording")
	profile, _ := tt.Find("Your name")
	if !ok || !hok || !(profile.Y < captions.Y && captions.Y < highlights.Y && highlights.Y < slot.Y) {
		t.Fatalf("Your name at %v, Captions' last note at %v, Highlights at %v (%v), the add-on at %v (%v): "+
			"want Profile, Captions, Highlights, then the add-on", profile, captions, highlights, hok, slot, ok)
	}
}

func TestNoAddOnLeavesNoTrace(t *testing.T) {
	a := testApp(t) // addon.None
	w := endedWindow(a)
	tt := ui.NewTester(w.view, 1040, 700)
	if err := tt.Click("Copy as Markdown"); err != nil {
		t.Fatal(err)
	}
	if md := tt.Clipboard(); !strings.HasSuffix(md, "- We ship Friday.\n") {
		t.Errorf("copied Markdown does not end with the transcript:\n%q", md)
	}
}

func TestTurnAtIsTheTurnSaidAtThatSecond(t *testing.T) {
	segs := []store.Segment{
		{ID: "a", Lang: "en-US", StartMs: 5_000},
		{ID: "b", Lang: "en-US", StartMs: 42_500},
		{ID: "gap", Lang: store.GapLang, StartMs: 60_000},
		{ID: "c", Lang: "en-US", StartMs: 65_000},
	}
	for atMs, want := range map[int64]string{
		0:      "a", // before the first: the first
		5_000:  "a",
		42_000: "b", // [00:42] cites the turn that reads 00:42
		50_000: "b",
		60_000: "b", // a gap is never a turn
		99_000: "c",
	} {
		if got := turnAt(segs, atMs); got != want {
			t.Errorf("turnAt(%d) = %q, want %q", atMs, got, want)
		}
	}
	if got := turnAt([]store.Segment{{ID: "gap", Lang: store.GapLang}}, 0); got != "" {
		t.Errorf("only a gap: %q", got)
	}
}

// posted queues what the window's goroutines post, to run on the test's
// goroutine as the main thread would.
type posted chan func()

func (q posted) post(fn func()) { q <- fn }

// run runs the next n functions posted.
func (q posted) run(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case fn := <-q:
			fn()
		case <-time.After(5 * time.Second):
			t.Fatal("nothing was posted")
		}
	}
}

// seed stores an ended session of turns, one every 10 s, and returns it.
func seed(t *testing.T, st store.Store, title string, turns int) store.Session {
	t.Helper()
	ctx := context.Background()
	s := store.Session{ID: uuid.NewString(), Mode: store.ModeCaptions, Source: store.SourceMic, LangA: "en-US", LangB: "zh-CN",
		StartedAt: time.Now().Add(-time.Hour)}
	if err := st.CreateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	for i := range turns {
		seg := store.Segment{ID: uuid.NewString(), SessionID: s.ID, Seq: i, Lang: "en-US", Text: fmt.Sprintf("Turn %d.", i),
			StartMs: int64(i) * 10_000, EndMs: int64(i)*10_000 + 9_000}
		if err := st.AppendSegment(ctx, seg); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.EndSession(ctx, s.ID, s.StartedAt.Add(time.Duration(turns)*10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRecap(ctx, s.ID, store.Recap{Status: store.RecapReady, Title: title, Text: "# " + title}); err != nil {
		t.Fatal(err)
	}
	return s
}

// storeApp is an app over a real library at path, with Capy's window on
// it whose goroutines post to q.
func storeApp(t *testing.T, path string) (*App, *mainWindow, posted) {
	t.Helper()
	db, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a := testApp(t)
	a.store = db
	q := make(posted, 16)
	w := &mainWindow{app: a, split: sidebarWidth, loaded: true, keyState: keyStored, post: q.post}
	a.window = w
	return a, w, q
}

// visible reports whether text shows in the tester's window: a text
// scrolled away has an empty box at the edge of its scroll.
func visible(tt *ui.Tester, text string, height float32) bool {
	r, ok := tt.Find(text)
	return ok && r.H > 0 && r.Y >= 0 && r.Y+r.H <= height
}

// highlight is the light palette's highlight, which a jump paints.
var highlight = ui.Hex("#f4e9dc")

func isHighlight(c color.RGBA) bool {
	return c.R == highlight.R && c.G == highlight.G && c.B == highlight.B
}

func TestShowTranscriptSelectsJumpsToTheTurnAndHighlightsIt(t *testing.T) {
	a, w, q := storeApp(t, filepath.Join(t.TempDir(), "capy.db"))
	s := seed(t, a.store, "Weekly sync", 60)
	tt := ui.NewTester(w.view, 1040, 700)

	w.showTranscript(s.ID, 305_000) // [05:05]: the turn at 05:00
	q.run(t, 1)                     // the session loads
	tt.Frame()
	if w.selected != s.ID || w.tab != tabTranscript {
		t.Fatalf("selected %q at tab %d; want %s's Transcript", w.selected, w.tab, s.ID)
	}
	if !visible(tt, "Turn 30.", 700) || visible(tt, "Turn 0.", 700) {
		r30, _ := tt.Find("Turn 30.")
		r0, _ := tt.Find("Turn 0.")
		t.Fatalf("the turn jumped to is not in view: Turn 30 at %+v, Turn 0 at %+v", r30, r0)
	}
	snapshot(t, tt, "window-transcript-jump")
	turn, _ := tt.Find("Turn 30.")
	img := tt.Image()
	scale := float32(img.Bounds().Dx()) / 1040
	at := func() color.RGBA { return img.RGBAAt(int((turn.X+turn.W+4)*scale), int((turn.Y+turn.H/2)*scale)) }
	if px := at(); !isHighlight(px) {
		t.Fatalf("pixel right of the turn %+v, want the highlight %+v", px, highlight)
	}
	// Two seconds on, the highlight is gone, and the turn stays in view.
	tt.Advance(highlightTime)
	tt.Frame()
	img = tt.Image()
	if px := at(); isHighlight(px) || w.jump != (jump{}) {
		t.Fatalf("after %v: pixel %+v, jump %+v; want no highlight", highlightTime, px, w.jump)
	}
	if !visible(tt, "Turn 30.", 700) {
		t.Fatal("the turn scrolled away as the highlight ended")
	}
	// Selecting another session forgets a jump not yet shown.
	w.showTranscript(s.ID, 0)
	w.selectSession("other")
	if w.jump != (jump{}) || w.tab != tabSummary {
		t.Fatalf("after selecting another session: jump %+v, tab %d", w.jump, w.tab)
	}
}
