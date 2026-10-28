package session_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/audio"
	"github.com/onyx-dot-app/Capy/internal/fakezoom"
	"github.com/onyx-dot-app/Capy/internal/profile"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

var listener = profile.Profile{Name: "Alex Rivera", Aliases: []string{"Alex"}}

func highlightsCfg(every time.Duration) session.Config {
	return session.Config{Source: store.SourceReplay, LangA: "en-US", LangB: "ja-JP", Highlights: session.HighlightConfig{
		On: true, Every: every, Kinds: store.HighlightKinds, Profile: listener,
	}}
}

// newTurns are the new turns of each Decisions call, "[t1 00:02 S1] text".
func newTurns(calls []string) [][]string {
	var out [][]string
	for _, in := range calls {
		_, news, _ := strings.Cut(in, "\nNEW:\n")
		out = append(out, strings.Split(news, "\n"))
	}
	return out
}

type kindAt struct {
	seq  int
	kind store.HighlightKind
}

func kinds(hs []store.Highlight) []kindAt {
	var out []kindAt
	for _, h := range hs {
		out = append(out, kindAt{h.Seq, h.Kind})
	}
	return out
}

var pricing = []fakezoom.Decision{
	{Kind: "decision", Text: "annual goes to forty-nine", P: 0.93},
	{Kind: "asked_you", Text: "update the pricing slide", P: 0.95},
}

func TestHighlightsAtStopAreSavedAndShown(t *testing.T) {
	restore := session.SetHighlightTiming(time.Hour, time.Hour, 10*time.Second)
	defer restore()
	h := newHarness(t, fakezoom.Script{Decisions: pricing, Turns: turns(
		"Okay, annual goes to forty-nine.", "Alex, can you update the pricing slide?", "Nice weather today.")})
	snap, d := h.runToEnd(t, highlightsCfg(time.Hour), 40)

	want := []kindAt{{0, store.KindDecision}, {1, store.KindAskedYou}}
	if got := kinds(snap.Highlights); !slices.Equal(got, want) || !slices.Equal(kinds(d.Highlights), want) {
		t.Fatalf("snapshot %v, saved %v; want %v", got, kinds(d.Highlights), want)
	}
	if snap.Status != session.StatusDone || d.Recap == "" {
		t.Fatalf("status %s, recap %q: the recap must not wait on Highlights", snap.Status, d.Recap)
	}
	calls := h.fake.DecideCalls()
	if len(calls) != 1 || !strings.Contains(calls[0], "The listener: Alex Rivera. Also called: Alex.") {
		t.Fatalf("calls %q: want one, at Stop, naming the listener", calls)
	}
}

func TestHighlightsCheckANameLineEarlyAndOnlyOnce(t *testing.T) {
	restore := session.SetHighlightTiming(150*time.Millisecond, 0, 10*time.Second)
	defer restore()
	h := newHarness(t, fakezoom.Script{Decisions: pricing, Turns: turns(
		"Hello everyone.", "Alex, can you update the pricing slide?", "Sure thing.", "Then on to the budget.", "Okay, annual goes to forty-nine.")})
	h.realtime = true // the turns come a second apart: the name line's check comes before the next
	snap, _ := h.runToEnd(t, highlightsCfg(time.Hour), 60)

	calls := newTurns(h.fake.DecideCalls())
	if len(calls) != 2 {
		t.Fatalf("%d calls %q: want the name line's, then the last at Stop", len(calls), calls)
	}
	if early := strings.Join(calls[0], "\n"); !strings.Contains(early, "[t1 ") || strings.Contains(early, "[t3 ") {
		t.Fatalf("the early check's new turns: %q", calls[0])
	}
	if last := strings.Join(calls[1], "\n"); strings.Contains(last, "[t1 ") || !strings.Contains(last, "[t4 ") {
		t.Fatalf("the last check's new turns: %q; a confirmed line is judged once", calls[1])
	}
	if got, want := kinds(snap.Highlights), []kindAt{{1, store.KindAskedYou}, {4, store.KindDecision}}; !slices.Equal(got, want) {
		t.Fatalf("highlights %v, want %v", got, want)
	}
}

func TestHighlightsJudgeAnUnconfirmedNameLineOnceMore(t *testing.T) {
	restore := session.SetHighlightTiming(150*time.Millisecond, 0, 10*time.Second)
	defer restore()
	h := newHarness(t, fakezoom.Script{Turns: turns(
		"Hello everyone.", "Alex already checked it.", "Right.", "Then on to the budget.", "And the next item.")})
	h.realtime = true
	snap, _ := h.runToEnd(t, highlightsCfg(time.Hour), 60)
	calls := newTurns(h.fake.DecideCalls())
	if len(calls) != 2 || !strings.HasPrefix(calls[1][0], "[t1 ") {
		t.Fatalf("calls %q: want the name line new again in the next check, with what followed", calls)
	}
	if len(snap.Highlights) != 0 {
		t.Fatalf("highlights %v: Decisions confirmed none", snap.Highlights)
	}
}

func TestHighlightsCheckEachIntervalOnlyWhatIsNew(t *testing.T) {
	h := newHarness(t, fakezoom.Script{Turns: turns("One.", "Two.", "Three.", "Four.", "Five.", "Six.")})
	h.realtime = true
	h.runToEnd(t, highlightsCfg(1500*time.Millisecond), 90) // three seconds of silence after the last turn
	calls := newTurns(h.fake.DecideCalls())
	if len(calls) < 2 {
		t.Fatalf("%d calls: want checks while recording, then the last", len(calls))
	}
	seen := map[string]int{}
	for _, news := range calls {
		if len(news) == 0 || news[0] == "" {
			t.Fatalf("a check of no new turn: %q", calls)
		}
		for _, l := range news {
			id, _, _ := strings.Cut(l, " ")
			seen[id]++
		}
	}
	if len(seen) != 6 {
		t.Fatalf("turns judged %v, want all six", seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("%s judged %d times: %q", id, n, calls)
		}
	}
}

func TestHighlightsStopForAKeyWithoutDecisions(t *testing.T) {
	restore := session.SetHighlightTiming(50*time.Millisecond, 0, 10*time.Second)
	defer restore()
	h := newHarness(t, fakezoom.Script{DecideStatus: http.StatusForbidden, Decisions: pricing, Turns: turns(
		"Alex, can you update the pricing slide?", "Sure.", "Alex, and the deck?", "Okay, annual goes to forty-nine.")})
	h.realtime = true
	snap, d := h.runToEnd(t, highlightsCfg(500*time.Millisecond), 60)
	if !errors.Is(snap.HighlightsErr, zoomai.ErrUnauthorized) || snap.Err != nil {
		t.Fatalf("highlights err %v, session err %v", snap.HighlightsErr, snap.Err)
	}
	if n := len(h.fake.DecideCalls()); n != 1 {
		t.Fatalf("%d calls: want none after the key was refused", n)
	}
	if snap.Status != session.StatusDone || len(d.Segments) != 4 || len(d.Highlights) != 0 {
		t.Fatalf("status %s, %d turns, %d highlights: captions and the recap go on", snap.Status, len(d.Segments), len(d.Highlights))
	}
}

func TestHighlightsTurnOffAndOnWhileRecording(t *testing.T) {
	restore := session.SetHighlightTiming(time.Hour, time.Hour, 10*time.Second)
	defer restore()
	h := newHarness(t, fakezoom.Script{Decisions: pricing, Turns: turns(
		"Alex, can you update the pricing slide?", "Sure.", "Okay, annual goes to forty-nine.")})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := highlightsCfg(time.Hour)
	cfg.Highlights.On = false
	changed := make(chan struct{}, 64)
	run, err := session.Start(ctx, h.deps, cfg, audio.NewWAV(silence(t, 40), true), func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	// On after the first turn: it marks the turns from then on.
	for len(run.Snapshot().Lines) < 1 {
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal("no turn")
		}
	}
	on := cfg.Highlights
	on.On = true
	run.SetHighlights(on)
	<-run.Done()
	snap := run.Snapshot()
	if got, want := kinds(snap.Highlights), []kindAt{{2, store.KindDecision}}; !slices.Equal(got, want) {
		t.Fatalf("highlights %v, want %v: the turn before Highlights was on is not judged", got, want)
	}
	if !snap.Config.Highlights.On {
		t.Fatal("the snapshot's config does not follow SetHighlights")
	}
	// Dismissed, it is saved so, and shown so.
	id := snap.Highlights[0].ID
	if err := run.Dismiss(ctx, id); err != nil {
		t.Fatal(err)
	}
	d, err := h.store.GetSession(ctx, snap.SessionID)
	if err != nil || !d.Highlights[0].Dismissed || !run.Snapshot().Highlights[0].Dismissed {
		t.Fatalf("after Dismiss: saved %+v (%v), shown %+v", d.Highlights, err, run.Snapshot().Highlights)
	}
}

// said is a saved turn: what was said, when, and its translation, if any.
type said struct {
	ms          int64
	text, trans string
}

// savedSession stores an ended session of turns, "" a gap, translated to
// ja-JP when one has a translation.
func savedSession(t *testing.T, st store.Store, turns ...said) string {
	t.Helper()
	ctx := context.Background()
	s := store.Session{ID: uuid.NewString(), Mode: store.ModeCaptions, Source: store.SourceMic, LangA: "en-US", LangB: "ja-JP",
		StartedAt: time.Now().Add(-time.Hour)}
	if err := st.CreateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	for i, tn := range turns {
		seg := store.Segment{ID: uuid.NewString(), SessionID: s.ID, Seq: i, Lang: "en-US", Text: tn.text, StartMs: tn.ms, EndMs: tn.ms + 5000}
		if tn.text == "" {
			seg.Lang = store.GapLang
		}
		if tn.trans != "" {
			seg.Translation, seg.TranslationLang = tn.trans, "ja-JP"
		}
		if err := st.AppendSegment(ctx, seg); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.EndSession(ctx, s.ID, s.StartedAt.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	return s.ID
}

var findCfg = session.HighlightConfig{Kinds: store.HighlightKinds, Profile: listener}

func TestFindHighlightsReplaysASavedSessionsChecks(t *testing.T) {
	h := newHarness(t, fakezoom.Script{Decisions: pricing})
	ctx := context.Background()
	id := savedSession(t, h.store,
		said{ms: 0, text: "Hello everyone."},
		said{ms: 60_000, text: "Okay, annual goes to forty-nine."},
		said{ms: 120_000}, // the connection dropped
		said{ms: 400_000, text: "Alex, can you update the pricing slide?"},
		said{ms: 420_000, text: "Sure."},
	)
	n, err := session.FindHighlights(ctx, h.deps, id, findCfg)
	if err != nil || n != 2 {
		t.Fatalf("found %d, %v; want 2", n, err)
	}
	// As a recording would have checked them: every 5 minutes of what was
	// said, each turn once, the gap left out.
	calls := newTurns(h.fake.DecideCalls())
	if len(calls) != 2 || len(calls[0]) != 2 || !strings.HasPrefix(calls[0][1], "[t1 01:00 ") ||
		len(calls[1]) != 2 || !strings.HasPrefix(calls[1][0], "[t3 06:40 ") {
		t.Fatalf("new turns of each call %q: want t0 and t1, then t3 and t4", calls)
	}
	d, err := h.store.GetSession(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := kinds(d.Highlights), []kindAt{{1, store.KindDecision}, {3, store.KindAskedYou}}; !slices.Equal(got, want) {
		t.Fatalf("saved %v, want %v", got, want)
	}

	// Found again, a turn marked or dismissed before stays as it is.
	if err := h.store.DismissHighlight(ctx, d.Highlights[0].ID); err != nil {
		t.Fatal(err)
	}
	if n, err := session.FindHighlights(ctx, h.deps, id, findCfg); err != nil || n != 0 {
		t.Fatalf("found again %d, %v; want none new", n, err)
	}
	d, err = h.store.GetSession(ctx, id)
	if err != nil || len(d.Highlights) != 2 || !d.Highlights[0].Dismissed {
		t.Fatalf("after finding again: %+v, %v; want the two, the decision still dismissed", d.Highlights, err)
	}
}

func TestFindHighlightsReadsATranslatedSessionInItsTranslation(t *testing.T) {
	h := newHarness(t, fakezoom.Script{Decisions: []fakezoom.Decision{{Kind: "decision", Text: "年額は49ドル", P: 0.93}}})
	id := savedSession(t, h.store,
		said{ms: 0, text: "Okay, annual goes to forty-nine.", trans: "では、年額は49ドルにします。"},
		said{ms: 5_000, text: "Great.", trans: "いいですね。"},
	)
	n, err := session.FindHighlights(context.Background(), h.deps, id, findCfg)
	if err != nil || n != 1 {
		t.Fatalf("found %d, %v; want the decision, in Japanese", n, err)
	}
	if calls := h.fake.DecideCalls(); len(calls) != 1 || strings.Contains(calls[0], "forty-nine") {
		t.Fatalf("calls %q: want the translation sent, as the recap is written", calls)
	}
}

func TestFindHighlightsStopsAtAKeyWithoutDecisions(t *testing.T) {
	h := newHarness(t, fakezoom.Script{DecideStatus: http.StatusForbidden, Decisions: pricing})
	id := savedSession(t, h.store,
		said{ms: 0, text: "Okay, annual goes to forty-nine."},
		said{ms: 400_000, text: "Alex, can you update the pricing slide?"},
	)
	n, err := session.FindHighlights(context.Background(), h.deps, id, findCfg)
	if !errors.Is(err, zoomai.ErrUnauthorized) || n != 0 {
		t.Fatalf("found %d, %v; want none and ErrUnauthorized", n, err)
	}
	if calls := h.fake.DecideCalls(); len(calls) != 1 {
		t.Fatalf("%d calls: want none after the key was refused", len(calls))
	}
}

// heard is what Deps.Highlighted heard: each call, with how many
// highlights the store had then.
type heard struct {
	final bool
	saved int
}

func (h *harness) listen(t *testing.T) func() []heard {
	t.Helper()
	var mu sync.Mutex
	var calls []heard
	h.deps.Highlighted = func(id string, final bool) {
		d, err := h.store.GetSession(context.Background(), id)
		if err != nil {
			t.Errorf("Highlighted for a session the store has not: %v", err)
		}
		mu.Lock()
		calls = append(calls, heard{final, len(d.Highlights)})
		mu.Unlock()
	}
	return func() []heard { mu.Lock(); defer mu.Unlock(); return slices.Clone(calls) }
}

func TestHighlightedHearsEachSaveThenTheLastCheck(t *testing.T) {
	restore := session.SetHighlightTiming(time.Hour, time.Hour, 10*time.Second)
	defer restore()
	h := newHarness(t, fakezoom.Script{Decisions: pricing, Turns: turns(
		"Okay, annual goes to forty-nine.", "Alex, can you update the pricing slide?")})
	calls := h.listen(t)
	h.runToEnd(t, highlightsCfg(time.Hour), 40)
	// The check at Stop saved two, which the store had when it was heard;
	// then the last check was over.
	if got, want := calls(), []heard{{false, 2}, {true, 2}}; !slices.Equal(got, want) {
		t.Fatalf("heard %v, want %v", got, want)
	}
}

func TestHighlightedHearsFindHighlightsOnlyWhenItFinds(t *testing.T) {
	h := newHarness(t, fakezoom.Script{Decisions: pricing})
	calls := h.listen(t)
	id := savedSession(t, h.store, said{ms: 0, text: "Okay, annual goes to forty-nine."})
	if n, err := session.FindHighlights(context.Background(), h.deps, id, findCfg); err != nil || n != 1 {
		t.Fatalf("found %d, %v", n, err)
	}
	if n, err := session.FindHighlights(context.Background(), h.deps, id, findCfg); err != nil || n != 0 {
		t.Fatalf("found again %d, %v", n, err)
	}
	if got, want := calls(), []heard{{true, 1}}; !slices.Equal(got, want) {
		t.Fatalf("heard %v, want %v: once, after what it found was saved", got, want)
	}
}

func TestMergePolishShowsWhatTheAgentSaved(t *testing.T) {
	restore := session.SetHighlightTiming(time.Hour, time.Hour, 10*time.Second)
	defer restore()
	h := newHarness(t, fakezoom.Script{Decisions: pricing, Turns: turns(
		"Okay, annual goes to forty-nine.", "Alex, can you update the pricing slide?", "Should we wait?", "No, ship it.")})
	ctx := context.Background()
	run, err := session.Start(ctx, h.deps, highlightsCfg(time.Hour), audio.NewWAV(silence(t, 40), false), nil)
	if err != nil {
		t.Fatal(err)
	}
	<-run.Done()
	snap := run.Snapshot()
	if len(snap.Highlights) != 2 {
		t.Fatalf("highlights %v, want two", kinds(snap.Highlights))
	}
	decision := snap.Highlights[0]
	debate := store.Highlight{ID: uuid.NewString(), SessionID: snap.SessionID, Seq: 2, Kind: store.KindDebate, Confidence: 0.9, CreatedAt: time.Now(),
		Polish: &store.Polish{ByAgent: "Claude Code", Confidence: 0.9, Debate: &store.Debate{Topic: "Wait or ship", Status: store.DebateDecided,
			Decision: decision.ID, Sides: []store.Side{{Position: "Wait"}, {Position: "Ship"}}}}}
	err = h.store.PolishHighlights(ctx, store.Polishing{SessionID: snap.SessionID, At: time.Now(),
		Polish:  map[string]store.Polish{decision.ID: {ByAgent: "Claude Code", Text: "Annual pricing goes to $49.", Confidence: 0.9}},
		Debates: []store.Highlight{debate}})
	if err != nil {
		t.Fatal(err)
	}
	if err := run.MergePolish(ctx); err != nil {
		t.Fatal(err)
	}
	got := run.Snapshot().Highlights
	if len(got) != 3 || got[0].Polish == nil || got[0].Polish.Text != "Annual pricing goes to $49." || got[1].Polish != nil ||
		got[2].ID != debate.ID || got[2].Polish.Debate.Decision != decision.ID {
		t.Fatalf("after merging: %+v", got)
	}
}

// racingStore is a store where an agent's debate lands on turn seq just
// before the first highlights are saved, as a polish pass may while a
// check waits on Decisions.
type racingStore struct {
	store.Store
	seq  int
	once sync.Once
}

func (s *racingStore) AddHighlights(ctx context.Context, hs []store.Highlight) error {
	s.once.Do(func() {
		debate := store.Highlight{ID: uuid.NewString(), SessionID: hs[0].SessionID, Seq: s.seq, Kind: store.KindDebate,
			Confidence: 0.9, CreatedAt: time.Now(), Polish: &store.Polish{ByAgent: "Claude Code", Confidence: 0.9,
				Debate: &store.Debate{Topic: "Forty-nine or fifty", Status: store.DebateOpen,
					Sides: []store.Side{{Position: "Forty-nine"}, {Position: "Fifty"}}}}}
		if err := s.Store.PolishHighlights(ctx, store.Polishing{SessionID: debate.SessionID, At: time.Now(), Debates: []store.Highlight{debate}}); err != nil {
			panic(err)
		}
	})
	return s.Store.AddHighlights(ctx, hs)
}

func TestACheckKeepsItsMarksWhenADebateTookATurnMeanwhile(t *testing.T) {
	restore := session.SetHighlightTiming(time.Hour, time.Hour, 10*time.Second)
	defer restore()
	h := newHarness(t, fakezoom.Script{Decisions: pricing, Turns: turns(
		"Okay, annual goes to forty-nine.", "Alex, can you update the pricing slide?")})
	h.deps.Store = &racingStore{Store: h.store, seq: 0} // the decision's turn
	snap, d := h.runToEnd(t, highlightsCfg(time.Hour), 40)
	want := []kindAt{{0, store.KindDebate}, {1, store.KindAskedYou}}
	if got := kinds(d.Highlights); !slices.Equal(got, want) {
		t.Fatalf("saved %v, want the debate and the request: the check's other mark kept", got)
	}
	if got := kinds(snap.Highlights); !slices.Equal(got, []kindAt{{1, store.KindAskedYou}}) {
		t.Fatalf("shown %v, want the request the check saved", got)
	}
}
