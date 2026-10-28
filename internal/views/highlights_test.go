package views

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/fakezoom"
	"github.com/onyx-dot-app/Capy/internal/features"
	"github.com/onyx-dot-app/Capy/internal/profile"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

// seedHighlights stores highlights of session s's turns, by seq, and
// returns their IDs.
func seedHighlights(t *testing.T, st store.Store, s store.Session, kinds map[int]store.HighlightKind) map[int]string {
	t.Helper()
	ids := map[int]string{}
	var hs []store.Highlight
	for seq, k := range kinds {
		ids[seq] = uuid.NewString()
		hs = append(hs, store.Highlight{ID: ids[seq], SessionID: s.ID, Seq: seq, Kind: k, Confidence: 0.9, CreatedAt: time.Now()})
	}
	if err := st.AddHighlights(context.Background(), hs); err != nil {
		t.Fatal(err)
	}
	return ids
}

func TestSummaryListsWhatWasForYouFirstAndOpensItsLine(t *testing.T) {
	a, w, q := storeApp(t, filepath.Join(t.TempDir(), "capy.db"))
	s := seed(t, a.store, "Launch sync", 40)
	ids := seedHighlights(t, a.store, s, map[int]store.HighlightKind{
		3: store.KindDecision, 7: store.KindAskedYou, 12: store.KindHeadsUp, 20: store.KindMentionedYou,
	})
	if err := a.store.DismissHighlight(context.Background(), ids[20]); err != nil {
		t.Fatal(err)
	}
	tt := ui.NewTester(w.view, 1040, 700)
	w.selectSession(s.ID)
	w.reload()
	q.run(t, 3) // the session, then the list and the session again
	tt.Frame()
	snapshot(t, tt, "highlights-summary")

	if !tt.HasText("3 highlights · 1 for you") || tt.HasText("4 highlights · 2 for you") {
		t.Fatalf("meta line: texts %q; want the dismissed one left out", tt.Texts())
	}
	forYou, recap, others := find(t, tt, "For you"), find(t, tt, "Recap"), find(t, tt, "Highlights")
	asked, decision := find(t, tt, "Turn 7."), find(t, tt, "Turn 3.")
	if !(forYou.Y < asked.Y && asked.Y < recap.Y && recap.Y < others.Y && others.Y < decision.Y) {
		t.Fatalf("For you %v, its line %v, Recap %v, Highlights %v, the decision %v: want For you, the recap, then the rest",
			forYou, asked, recap, others, decision)
	}
	if tt.HasText("Turn 20.") {
		t.Fatalf("the dismissed highlight is listed; texts %q", tt.Texts())
	}
	// A row opens the Transcript at its line.
	if err := tt.Click("Turn 12."); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if w.tab != tabTranscript || w.jump.atMs != 120_000 || !visible(tt, "Turn 12.", 700) {
		t.Fatalf("tab %d, jump %+v: want the Transcript at 02:00", w.tab, w.jump)
	}
}

func TestTranscriptShowsInsightsBesideTheirLinesWithoutRepeatingThem(t *testing.T) {
	a, w, q := storeApp(t, filepath.Join(t.TempDir(), "capy.db"))
	ext := &stubAddOn{asks: true}
	a.ext = ext
	s := seed(t, a.store, "Launch sync", 6)
	ids := seedHighlights(t, a.store, s, map[int]store.HighlightKind{2: store.KindAskedYou, 4: store.KindAction})
	tt := ui.NewTester(w.view, 1040, 700)
	w.selectSession(s.ID)
	w.reload()
	q.run(t, 3)
	w.tab = tabTranscript
	tt.Frame()
	tt.Frame() // the pane's width, as the first frame laid it out, gives the cards their column
	snapshot(t, tt, "highlights-transcript")

	// Each card is beside its line, right of the text, never over it.
	card, line := find(t, tt, "Asked you"), find(t, tt, "Turn 2.")
	if card.X < line.X+line.W || card.Y > line.Y+line.H || card.Y+card.H < line.Y {
		t.Fatalf("card at %v, its line at %v: want it beside the line, in the column", card, line)
	}
	if !tt.HasText("Someone asked you to do something.") || !tt.HasText("Action") || tt.HasText("Ask about this") {
		t.Fatalf("collapsed cards: texts %q", tt.Texts())
	}
	// Expanded, it offers what can be done, and repeats no turn: the
	// transcript shows it.
	if err := tt.Click("Asked you"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	snapshot(t, tt, "highlights-card")
	for _, want := range []string{"Ask about this", "Copy", "Not important"} {
		if !tt.HasText(want) {
			t.Errorf("expanded card lacks %q; texts %q", want, tt.Texts())
		}
	}
	if n := slices.Index(tt.Texts(), "Turn 2."); n < 0 || slices.Contains(tt.Texts()[n+1:], "Turn 2.") || tt.HasText("After: “Turn 1.”") {
		t.Fatalf("the card repeats the transcript: texts %q", tt.Texts())
	}
	if line2 := find(t, tt, "Turn 2."); line2.X != line.X || line2.W != line.W {
		t.Fatalf("the line moved from %v to %v as its card expanded", line, line2)
	}
	if err := tt.Click("Ask about this"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if want := s.ID + "|What does [00:20] mean for me, and what should I do?|false"; len(ext.asked) != 1 || ext.asked[0] != want {
		t.Fatalf("asked %q, want %q", ext.asked, want)
	}
	if tt.HasText("Not important") {
		t.Fatal("the card stayed expanded after Ask about this")
	}
	// A mark has no insight of its own: Copy copies its line.
	if err := tt.Click("Asked you"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click("Copy"); err != nil {
		t.Fatal(err)
	}
	if tt.Clipboard() != "Turn 2." {
		t.Fatalf("copied %q", tt.Clipboard())
	}
	// Not important takes it away, for good.
	if err := tt.Click("Not important"); err != nil {
		t.Fatal(err)
	}
	q.run(t, 3) // dismissed, the window reads the list and the session again
	tt.Frame()
	if tt.HasText("Asked you") || !tt.HasText("Action") {
		t.Fatalf("after Not important: texts %q", tt.Texts())
	}
	d, err := a.store.GetSession(context.Background(), s.ID)
	if err != nil || !d.Highlights[0].Dismissed || d.Highlights[0].ID != ids[2] {
		t.Fatalf("saved %+v, %v", d.Highlights, err)
	}
}

func TestANarrowPaneOpensACardUnderItsLine(t *testing.T) {
	a, w, q := storeApp(t, filepath.Join(t.TempDir(), "capy.db"))
	s := seed(t, a.store, "Launch sync", 4)
	seedHighlights(t, a.store, s, map[int]store.HighlightKind{1: store.KindHeadsUp})
	tt := ui.NewTester(w.view, 900, 700)
	w.selectSession(s.ID)
	w.reload()
	q.run(t, 3)
	w.tab = tabTranscript
	tt.Frame()
	tt.Frame()
	if glyph := find(t, tt, "Heads-up"); glyph.W > glyphColumn {
		t.Fatalf("a narrow pane shows the card, %v; want its glyph", glyph)
	}
	if err := tt.Click("Heads-up"); err != nil { // the glyph, by its label
		t.Fatal(err)
	}
	tt.Frame()
	line, why := find(t, tt, "Turn 1."), find(t, tt, "Someone raised a risk or a blocker.")
	if why.Y < line.Y+line.H || !tt.HasText("Not important") || tt.HasText("Ask about this") {
		t.Fatalf("line %v, card %v; texts %q: want the card under its line, no Ask without an add-on that answers", line, why, tt.Texts())
	}
}

func find(t *testing.T, tt *ui.Tester, text string) ui.Rect {
	t.Helper()
	r, ok := tt.Find(text)
	if !ok {
		t.Fatalf("no %q; texts %q", text, strings.Join(tt.Texts(), " | "))
	}
	return r
}

func TestSettingsHighlightsApplyToTheRecording(t *testing.T) {
	t.Setenv("ZOOM_API_KEY", "from-env") // keep the test away from the real Keychain
	a := testApp(t)
	a.profile = profile.Profile{Name: "Alex"}
	run := &fakeRun{snap: session.Snapshot{Config: captionsCfg, Status: session.StatusLive, StartedAt: time.Now()}}
	a.captions = &captionBar{app: a, cfg: captionsCfg, sessionSlot: sessionSlot{run: run}}
	w := &settingsPage{app: a, keyState: readKeyState()}
	tt := ui.NewTester(settingsShown(w).view, 1040, 1400)
	if tt.HasText("Highlight what matters while recording") {
		t.Fatal("Settings shows Highlights with its feature off")
	}
	a.gate = gate{features.Highlights: true}
	tt.Frame()
	snapshot(t, tt, "settings-highlights")
	if err := tt.Click("5 minutes"); err != nil { // Check every's pop-up
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("2 minutes"); err != nil {
		t.Fatalf("%v; menu %q", err, tt.Menu())
	}
	if !slices.Contains(a.highlightConfig().Kinds, store.KindDebate) {
		t.Fatal("Debates is not on by default")
	}
	for _, label := range []string{"Actions", "Debates", "More"} {
		if err := tt.Click(label); err != nil {
			t.Fatal(err)
		}
	}
	got := run.snap.Config.Highlights
	if !got.On || got.Every != 2*time.Minute || got.Threshold != session.ThresholdMore || got.Profile.Name != "Alex" ||
		slices.Contains(got.Kinds, store.KindAction) || slices.Contains(got.Kinds, store.KindDebate) || !slices.Contains(got.Kinds, store.KindAskedYou) {
		t.Fatalf("the recording's highlights %+v", got)
	}
	if p := LoadPrefs(a.prefsPath); p.HighlightEvery != 2 || p.HighlightActions || p.HighlightDebates || !p.HighlightMore {
		t.Fatalf("saved %+v", p)
	}
	// Off, it is off for the recording, and the rest waits.
	label, detail := find(t, tt, "Highlight what matters while recording"), find(t, tt, "Capy sends what was said since its last check to Zoom Decisions and marks the lines that matter.")
	tt.ClickAt(label.X-12+contentWidth-30, (label.Y+detail.Y+detail.H)/2) // the switch, at the row's right end
	if run.snap.Config.Highlights.On {
		t.Fatal("the recording still highlights")
	}
	if err := tt.Click("Decisions"); err != nil {
		t.Fatal(err)
	}
	if !a.prefs.HighlightDecisions {
		t.Fatal("a kind changed with Highlights off")
	}
}

func TestFindHighlightsMarksASavedSession(t *testing.T) {
	t.Setenv("ZOOM_API_KEY", "from-env") // keep the test away from the real Keychain
	fake := fakezoom.New(fakezoom.Script{APIKey: "from-env", Decisions: []fakezoom.Decision{{Kind: "decision", Text: "Turn 3.", P: 0.93}}})
	t.Cleanup(fake.Close)
	a, w, q := storeApp(t, filepath.Join(t.TempDir(), "capy.db"))
	a.newClient = func(key string) (*zoomai.Client, error) { return zoomai.NewClient(key, zoomai.WithBaseURL(fake.URL)) }
	s := seed(t, a.store, "Launch sync", 6)
	tt := ui.NewTester(w.view, 1040, 700)
	w.selectSession(s.ID)
	w.reload()
	q.run(t, 3)
	tt.Frame()
	if err := tt.Click("Find Highlights"); err == nil {
		t.Fatal("Find Highlights offered with the highlights feature off")
	}

	a.gate = gate{features.Highlights: true}
	tt.Frame()
	if err := tt.Click("Find Highlights"); err != nil {
		t.Fatal(err)
	}
	q.run(t, 1) // found
	q.settle(t) // the session again
	tt.Frame()
	snapshot(t, tt, "find-highlights")
	if !tt.HasText("Found 1 highlight.") || !tt.HasText("1 highlight") {
		t.Fatalf("texts %q: want what was found told, and counted", tt.Texts())
	}
	if err := tt.Click("Find Highlights"); err == nil {
		t.Fatal("Find Highlights still offered beside the count, to find them again")
	}
	if hs := w.detail.Highlights; len(hs) != 1 || hs[0].Seq != 3 || hs[0].Kind != store.KindDecision {
		t.Fatalf("highlights %+v: want the decision of turn 3", hs)
	}
}

// polishHighlights saves an agent's polish of session s's highlights, by
// seq, and debates.
func polishHighlights(t *testing.T, st store.Store, s store.Session, polish map[string]store.Polish, debates ...store.Highlight) {
	t.Helper()
	if err := st.PolishHighlights(context.Background(), store.Polishing{SessionID: s.ID, At: time.Now(), Polish: polish, Debates: debates}); err != nil {
		t.Fatal(err)
	}
}

func TestPolishedHighlightsShowTheirSentencesGrouped(t *testing.T) {
	a, w, q := storeApp(t, filepath.Join(t.TempDir(), "capy.db"))
	a.ext = &stubAddOn{}
	s := seed(t, a.store, "Launch sync", 30)
	ids := seedHighlights(t, a.store, s, map[int]store.HighlightKind{
		3: store.KindAction, 7: store.KindDecision, 9: store.KindDecision, 12: store.KindHeadsUp, 20: store.KindAnnouncement,
	})
	debate := store.Highlight{ID: uuid.NewString(), SessionID: s.ID, Seq: 5, Kind: store.KindDebate, Confidence: 0.9, CreatedAt: time.Now(),
		Polish: &store.Polish{ByAgent: "Claude Code", Confidence: 0.9, Debate: &store.Debate{Topic: "Ship Monday or wait a week",
			Status: store.DebateDecided, Decision: ids[7], Sides: []store.Side{
				{Position: "Ship Monday, the fixes are in", Speakers: []string{"Tom"}, Seqs: []int{6, 4}},
				{Position: "Wait for the load test", Speakers: []string{"Priya"}, Seqs: []int{8}},
				{Position: "Ask support first"}}}}}
	polishHighlights(t, a.store, s, map[string]store.Polish{
		ids[3]:  {ByAgent: "Claude Code", Text: "You were asked to own the pricing copy.", Kind: store.KindAskedYou, Owner: "Alex", Due: "Friday", Confidence: 0.9},
		ids[7]:  {ByAgent: "Claude Code", Text: "The beta ships Monday.", DecidedBy: "group", Condition: "if the load test passes", Confidence: 0.9},
		ids[9]:  {ByAgent: "Claude Code", Text: "Monday it is.", Confidence: 0.9, FoldedInto: ids[7]},
		ids[12]: {ByAgent: "Claude Code", Text: "Chat staffing may not be ready.", Confidence: 0.5},
	}, debate)
	tt := ui.NewTester(w.view, 1040, 900)
	w.selectSession(s.ID)
	w.reload()
	q.run(t, 3)
	tt.Frame()
	snapshot(t, tt, "polish-summary")

	// The corrected kind is for the user, above the recap; the rest are
	// grouped below it, the restatement folded into the first.
	forYou, asked, recap := find(t, tt, "For you"), find(t, tt, "You were asked to own the pricing copy."), find(t, tt, "Recap")
	decisions, debates := find(t, tt, "Decisions"), find(t, tt, "Debates")
	if !(forYou.Y < asked.Y && asked.Y < recap.Y && recap.Y < decisions.Y && decisions.Y < debates.Y) {
		t.Fatalf("For you %v, the ask %v, Recap %v, Decisions %v, Debates %v", forYou, asked, recap, decisions, debates)
	}
	for _, want := range []string{"The beta ships Monday.", "also at 01:30", "Ship Monday or wait a week · decided",
		"Tom  Ship Monday, the fixes are in", "Priya  Wait for the load test", "→ The beta ships Monday. · 01:10", "Heads-ups",
		"Chat staffing may not be ready.", "Announcements", "Turn 20."} {
		if !tt.HasText(want) {
			t.Errorf("Summary lacks %q; texts %q", want, tt.Texts())
		}
	}
	for _, gone := range []string{"Turn 3.", "Monday it is.", "Actions"} {
		if tt.HasText(gone) {
			t.Errorf("Summary shows %q: an unpolished line, a restatement, or an empty group", gone)
		}
	}
	if !tt.HasText("5 highlights · 1 for you") {
		t.Errorf("meta line: texts %q; want the debate counted, not the restatement, the corrected ask for you", tt.Texts())
	}

	// The cards say what the agent wrote: collapsed, its sentence and facts,
	// a debate's sides a line each; expanded, its fields and the debate's
	// sides with their times. No card repeats a turn.
	w.tab = tabTranscript
	tt.Frame()
	tt.Frame()
	for _, want := range []string{"You were asked to own the pricing copy.", "Owner Alex · Due Friday", "Ship Monday or wait a week",
		"Tom  Ship Monday, the fixes are in", "Priya  Wait for the load test", "Someone  Ask support first", "→ decision at 01:10",
		"tentative", "Chat staffing may not be ready.",
		// VoiceOver reads the card's label, not its texts.
		"Debate: Ship Monday or wait a week; Tom: Ship Monday, the fixes are in; Priya: Wait for the load test; Someone: Ask support first"} {
		if !tt.HasText(want) {
			t.Errorf("collapsed cards lack %q; texts %q", want, tt.Texts())
		}
	}
	if tt.HasText("sides") || tt.HasText("Where Tom said it") {
		t.Errorf("a collapsed debate counts its sides, or times them; texts %q", tt.Texts())
	}
	toggle := func(card string) {
		t.Helper()
		if err := tt.Click(card); err != nil {
			t.Fatal(err)
		}
		tt.Frame()
	}
	toggle("Asked you")
	for _, want := range []string{"✦ Claude Code", "Owner", "Alex", "Due", "Friday"} {
		if !tt.HasText(want) {
			t.Errorf("the ask's card lacks %q; texts %q", want, tt.Texts())
		}
	}
	if tt.HasText("Someone asked you to do something.") || tt.HasText("After: “Turn 2.”") {
		t.Errorf("the polished card says the kind's sentence, or repeats the transcript; texts %q", tt.Texts())
	}
	if err := tt.Click("Copy"); err != nil {
		t.Fatal(err)
	}
	if tt.Clipboard() != "You were asked to own the pricing copy.\nOwner: Alex\nDue: Friday" {
		t.Errorf("copied %q, want the insight", tt.Clipboard())
	}
	toggle("Asked you")
	toggle("Debate")
	snapshot(t, tt, "polish-debate-card")
	for _, want := range []string{"Ship Monday or wait a week", "Ship Monday, the fixes are in", "Tom", "Wait for the load test", "Priya",
		"decided", "→ The beta ships Monday. · 01:10", "Where Priya said it, 01:20"} {
		if !tt.HasText(want) {
			t.Errorf("the debate's card lacks %q; texts %q", want, tt.Texts())
		}
	}
	if tt.HasText("Where Someone said it") {
		t.Errorf("a side of no turns has a time; texts %q", tt.Texts())
	}
	if err := tt.Click("Copy"); err != nil {
		t.Fatal(err)
	}
	if want := "Ship Monday or wait a week (decided)\n- Tom: Ship Monday, the fixes are in\n- Priya: Wait for the load test\n- Someone: Ask support first"; tt.Clipboard() != want {
		t.Errorf("copied %q, want %q", tt.Clipboard(), want)
	}

	// A side's time is of its earliest turn, and goes to it.
	if err := tt.Click("Where Tom said it, 00:40"); err != nil {
		t.Fatalf("%v; texts %q", err, tt.Texts())
	}
	tt.Frame()
	if w.jump.session != s.ID || w.jump.atMs != 40_000 {
		t.Errorf("jumped to %+v, want turn 4 of %s", w.jump, s.ID)
	}
}

// A debate's card above the caption bar is as tall as its sides wrap,
// for its actions to fit under them.
func TestADebateCardIsAsTallAsItsSides(t *testing.T) {
	card := func(sides ...store.Side) int {
		return cardHeight(store.Highlight{Kind: store.KindDebate, Polish: &store.Polish{Debate: &store.Debate{Sides: sides}}})
	}
	short := []store.Side{{Position: "Ship it", Speakers: []string{"Tom"}}, {Position: "Wait", Speakers: []string{"Priya"}}}
	long := strings.Repeat("a long position ", 7)
	if card(short...) >= card(store.Side{Position: long, Speakers: []string{"Tom"}}, short[1]) {
		t.Error("a side that wraps does not make the card taller")
	}
	// 24 Chinese characters fill more than the card's line; 24 letters do not.
	han, latin := strings.Repeat("文档必须人工维护", 3), strings.Repeat("abcdefgh", 3)
	if card(store.Side{Position: han, Speakers: []string{"Tom"}}, short[1]) <= card(store.Side{Position: latin, Speakers: []string{"Tom"}}, short[1]) {
		t.Error("a Chinese side as long as a line is counted as short as letters")
	}
}

func TestAFoldShowsItsHighlightOnceItsFirstIsDismissed(t *testing.T) {
	first := store.Highlight{ID: uuid.NewString(), Seq: 1, Kind: store.KindDecision,
		Polish: &store.Polish{ByAgent: "Claude Code", Text: "Ship Monday.", Confidence: 0.9}}
	again := store.Highlight{ID: uuid.NewString(), Seq: 4, Kind: store.KindDecision,
		Polish: &store.Polish{ByAgent: "Claude Code", Text: "Monday it is.", Confidence: 0.9, FoldedInto: first.ID}}
	said := func(seq int) (string, int64) { return "", int64(seq) * 10_000 }
	shown := shownHighlights([]store.Highlight{first, again})
	if text, _ := countText(shown); text != "1 highlight" {
		t.Fatalf("count %q, want the restatement left out", text)
	}
	if _, others, also := splitForYou(shown, said); len(others) != 1 || fmt.Sprint(also[first.ID]) != "[40000]" {
		t.Fatalf("listed %d, also %v; want the first, its restatement's time beside it", len(others), also)
	}
	first.Dismissed = true
	shown = shownHighlights([]store.Highlight{first, again})
	if text, _ := countText(shown); text != "1 highlight" {
		t.Fatalf("count %q after dismissing the first, want its restatement counted", text)
	}
	if _, others, _ := splitForYou(shown, said); len(others) != 1 || others[0].ID != again.ID {
		t.Fatalf("listed %+v after dismissing the first, want its restatement", others)
	}
}

// An hour's time in the transcript's time column ends clear of the rule a
// highlighted turn draws left of its text.
func TestAnHoursTimeClearsAHighlightsRule(t *testing.T) {
	var width float32
	tt := ui.NewTester(func(c *ui.Context) {
		kit.Theme(c)
		width, _ = c.MeasureText(0, ui.Span{Text: clock(time.Hour + 7*time.Minute + 10*time.Second), Size: 12, Features: kit.Tabular})
	}, 200, 100)
	tt.Frame()
	const gap = 8
	if width == 0 || width+gap > stampColumn-markInset {
		t.Errorf("a time %.1f wide runs into the rule at %d", width, stampColumn-markInset)
	}
}
