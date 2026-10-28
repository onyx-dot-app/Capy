package storetest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/store"
)

// highlightCases are the contract of highlights, run by Run.
var highlightCases = []struct {
	name string
	fn   func(t *testing.T, s store.Store)
}{
	{"HighlightsListedBySeqWithTheSession", highlightsListed},
	{"HighlightsAllOrNoneOncePerTurn", highlightsAllOrNone},
	{"HighlightDismissedStaysListed", highlightDismissed},
	{"HighlightsOfATrashedOrPurgedSession", highlightsTrashedAndPurged},
	{"PolishReadsBackWithItsHighlight", polishReadsBack},
	{"PolishAllOrNone", polishAllOrNone},
	{"PolishSkipsADismissedHighlight", polishSkipsDismissed},
	{"DebatesAddedOnAFreeTurnUpdatedOnTheirOwn", debatesOnTheirTurn},
	{"PolishOfATrashedOrPurgedSession", polishTrashedAndPurged},
}

func highlight(sessionID string, seq int, kind store.HighlightKind, p float64) store.Highlight {
	return store.Highlight{ID: uuid.NewString(), SessionID: sessionID, Seq: seq, Kind: kind, Confidence: p, CreatedAt: base}
}

func highlightsOf(t *testing.T, s store.Store, id string) []store.Highlight {
	t.Helper()
	d, err := s.GetSession(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	return d.Highlights
}

func highlightsListed(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := newSession(t, s, base)
	if got := highlightsOf(t, s, sess.ID); got == nil || len(got) != 0 {
		t.Fatalf("no highlights read as %#v, want an empty list", got)
	}
	late, early := highlight(sess.ID, 7, store.KindDecision, 0.88), highlight(sess.ID, 2, store.KindAskedYou, 0.93)
	if err := s.AddHighlights(ctx, []store.Highlight{late, early}); err != nil {
		t.Fatal(err)
	}
	got := highlightsOf(t, s, sess.ID)
	if len(got) != 2 || got[0].ID != early.ID || got[1].ID != late.ID {
		t.Fatalf("highlights %+v, want seq 2 then 7", got)
	}
	if h := got[0]; h.SessionID != sess.ID || h.Kind != store.KindAskedYou || h.Confidence != 0.93 || h.Dismissed || !h.CreatedAt.Equal(base) {
		t.Fatalf("read back %+v, want %+v", h, early)
	}
	// Another session's are its own.
	other := newSession(t, s, base)
	if got := highlightsOf(t, s, other.ID); len(got) != 0 {
		t.Fatalf("another session lists %+v", got)
	}
}

func highlightsAllOrNone(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := newSession(t, s, base)
	first := highlight(sess.ID, 1, store.KindDecision, 0.9)
	if err := s.AddHighlights(ctx, []store.Highlight{first}); err != nil {
		t.Fatal(err)
	}
	// A turn takes one highlight: a second, of any kind, fails the lot.
	wantErr(t, s.AddHighlights(ctx, []store.Highlight{
		highlight(sess.ID, 4, store.KindAction, 0.9), highlight(sess.ID, 1, store.KindHeadsUp, 0.95),
	}), store.ErrConflict)
	dupID := highlight(sess.ID, 5, store.KindAction, 0.9)
	dupID.ID = first.ID
	wantErr(t, s.AddHighlights(ctx, []store.Highlight{dupID}), store.ErrConflict)
	if got := highlightsOf(t, s, sess.ID); len(got) != 1 {
		t.Fatalf("after failed adds: %+v, want only the first", got)
	}
	// Of two sessions at once, or of none, nothing is stored.
	other := newSession(t, s, base)
	wantErr(t, s.AddHighlights(ctx, []store.Highlight{
		highlight(sess.ID, 6, store.KindAction, 0.9), highlight(other.ID, 6, store.KindAction, 0.9),
	}), store.ErrInvalid)
	wantErr(t, s.AddHighlights(ctx, []store.Highlight{highlight(uuid.NewString(), 0, store.KindAction, 0.9)}), store.ErrNotFound)
	for _, bad := range []store.Highlight{
		{ID: uuid.NewString(), SessionID: sess.ID, Seq: 2, Kind: "question", Confidence: 0.9, CreatedAt: base},
		{ID: uuid.NewString(), SessionID: sess.ID, Seq: 2, Kind: store.KindAction, Confidence: 1.2, CreatedAt: base},
		{ID: uuid.NewString(), SessionID: sess.ID, Seq: -1, Kind: store.KindAction, Confidence: 0.9, CreatedAt: base},
		{ID: "h1", SessionID: sess.ID, Seq: 2, Kind: store.KindAction, Confidence: 0.9, CreatedAt: base},
	} {
		wantErr(t, s.AddHighlights(ctx, []store.Highlight{bad}), store.ErrInvalid)
	}
	if got := highlightsOf(t, s, sess.ID); len(got) != 1 {
		t.Fatalf("after invalid adds: %+v, want only the first", got)
	}
}

func highlightDismissed(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := newSession(t, s, base)
	h := highlight(sess.ID, 3, store.KindHeadsUp, 0.9)
	if err := s.AddHighlights(ctx, []store.Highlight{h}); err != nil {
		t.Fatal(err)
	}
	if err := s.DismissHighlight(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	got := highlightsOf(t, s, sess.ID)
	if len(got) != 1 || !got[0].Dismissed {
		t.Fatalf("after dismissing: %+v, want it kept, dismissed", got)
	}
	// Kept, it keeps the turn from being marked again.
	wantErr(t, s.AddHighlights(ctx, []store.Highlight{highlight(sess.ID, 3, store.KindDecision, 0.95)}), store.ErrConflict)
	wantErr(t, s.DismissHighlight(ctx, uuid.NewString()), store.ErrNotFound)
}

func highlightsTrashedAndPurged(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := ended(t, s, base)
	kept := highlight(sess.ID, 0, store.KindDecision, 0.9)
	if err := s.AddHighlights(ctx, []store.Highlight{kept}); err != nil {
		t.Fatal(err)
	}
	mustTrash(t, s, sess.ID)
	// The last check of a session can end after it is trashed: it is kept,
	// to have if restored, but the highlights cannot change meanwhile.
	late := highlight(sess.ID, 4, store.KindAction, 0.9)
	if err := s.AddHighlights(ctx, []store.Highlight{late}); err != nil {
		t.Fatalf("add to a trashed session: %v", err)
	}
	wantErr(t, s.DismissHighlight(ctx, kept.ID), store.ErrNotFound)
	if _, err := s.RestoreSession(ctx, sess.ID, "user", ""); err != nil {
		t.Fatal(err)
	}
	if got := highlightsOf(t, s, sess.ID); len(got) != 2 || got[0].Dismissed {
		t.Fatalf("restored with %+v, want both, not dismissed", got)
	}
	mustTrash(t, s, sess.ID)
	if err := s.PurgeSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	wantErr(t, s.AddHighlights(ctx, []store.Highlight{highlight(sess.ID, 6, store.KindAction, 0.9)}), store.ErrNotFound)
	wantErr(t, s.DismissHighlight(ctx, late.ID), store.ErrNotFound)
}

func polishOf(text string, confidence float64) store.Polish {
	return store.Polish{ByAgent: "Claude Code", Text: text, Confidence: confidence}
}

func debate(sessionID string, seq int, topic string, status store.DebateStatus) store.Highlight {
	return store.Highlight{
		ID: uuid.NewString(), SessionID: sessionID, Seq: seq, Kind: store.KindDebate, Confidence: 0.9, CreatedAt: base,
		Polish: &store.Polish{ByAgent: "Claude Code", Confidence: 0.9, Debate: &store.Debate{
			Topic: topic, Status: status,
			Sides: []store.Side{{Position: "Ship Monday", Speakers: []string{"Tom"}, Seqs: []int{seq}}, {Position: "Wait a week", Speakers: []string{"Priya"}}},
		}},
	}
}

func polishing(sessionID string, polish map[string]store.Polish, debates ...store.Highlight) store.Polishing {
	return store.Polishing{SessionID: sessionID, At: base.Add(time.Minute), Polish: polish, Debates: debates}
}

func polishReadsBack(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := newSession(t, s, base)
	asked, other := highlight(sess.ID, 2, store.KindAction, 0.9), highlight(sess.ID, 5, store.KindDecision, 0.9)
	if err := s.AddHighlights(ctx, []store.Highlight{asked, other}); err != nil {
		t.Fatal(err)
	}
	want := store.Polish{
		ByAgent: "Claude Code", Text: "Ana asked you to own the pricing copy", Owner: "Alex Rivera", Due: "Friday",
		Kind: store.KindAskedYou, Confidence: 0.9, FoldedInto: other.ID,
	}
	if err := s.PolishHighlights(ctx, polishing(sess.ID, map[string]store.Polish{asked.ID: want})); err != nil {
		t.Fatal(err)
	}
	got := highlightsOf(t, s, sess.ID)
	if got[0].Polish == nil || !reflect.DeepEqual(*got[0].Polish, want) || !got[0].PolishedAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("polished %+v at %v, want %+v", got[0].Polish, got[0].PolishedAt, want)
	}
	if got[0].Kind != store.KindAction || got[0].ShownKind() != store.KindAskedYou {
		t.Fatalf("kind %s shown as %s: Decisions' kind must stay, the correction show", got[0].Kind, got[0].ShownKind())
	}
	if got[1].Polish != nil || !got[1].PolishedAt.IsZero() {
		t.Fatalf("the highlight not polished reads %+v at %v", got[1].Polish, got[1].PolishedAt)
	}
	// Polished again, the newer polish wins.
	again := polishOf("Ana asked you to own the pricing copy by Friday", 0.6)
	if err := s.PolishHighlights(ctx, polishing(sess.ID, map[string]store.Polish{asked.ID: again})); err != nil {
		t.Fatal(err)
	}
	if got := highlightsOf(t, s, sess.ID)[0].Polish; got == nil || !reflect.DeepEqual(*got, again) || !got.Tentative() {
		t.Fatalf("polished again: %+v, want %+v, tentative", got, again)
	}
}

func polishAllOrNone(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := newSession(t, s, base)
	h := highlight(sess.ID, 1, store.KindDecision, 0.9)
	taken := highlight(sess.ID, 3, store.KindAction, 0.9)
	if err := s.AddHighlights(ctx, []store.Highlight{h, taken}); err != nil {
		t.Fatal(err)
	}
	good := polishOf("Ship the beta Monday", 0.9)
	// A highlight not of the session fails the lot.
	other := newSession(t, s, base)
	theirs := highlight(other.ID, 1, store.KindDecision, 0.9)
	if err := s.AddHighlights(ctx, []store.Highlight{theirs}); err != nil {
		t.Fatal(err)
	}
	wantErr(t, s.PolishHighlights(ctx, polishing(sess.ID, map[string]store.Polish{h.ID: good, theirs.ID: good})), store.ErrNotFound)
	wantErr(t, s.PolishHighlights(ctx, polishing(sess.ID, map[string]store.Polish{h.ID: good, uuid.NewString(): good})), store.ErrNotFound)
	for name, bad := range map[string]store.Polish{
		"no agent":     {Text: "Ship it", Confidence: 0.9},
		"no text":      {ByAgent: "Claude Code", Confidence: 0.9},
		"two lines":    polishOf("Ship it\nMonday", 0.9),
		"too long":     polishOf(strings.Repeat("é", store.MaxPolishText+1), 0.9),
		"debate kind":  {ByAgent: "Claude Code", Text: "Ship it", Kind: store.KindDebate, Confidence: 0.9},
		"unknown kind": {ByAgent: "Claude Code", Text: "Ship it", Kind: "question", Confidence: 0.9},
		"confidence":   polishOf("Ship it", 1.5),
		"folded into":  {ByAgent: "Claude Code", Text: "Ship it", Confidence: 0.9, FoldedInto: "h1"},
	} {
		if err := s.PolishHighlights(ctx, polishing(sess.ID, map[string]store.Polish{h.ID: good, taken.ID: bad})); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s: err %v, want ErrInvalid", name, err)
		}
	}
	oneSide := debate(sess.ID, 4, "Ship Monday or wait", store.DebateOpen)
	oneSide.Polish.Debate.Sides = oneSide.Polish.Debate.Sides[:1]
	wantErr(t, s.PolishHighlights(ctx, polishing(sess.ID, nil, oneSide)), store.ErrInvalid)
	if got := highlightsOf(t, s, sess.ID); len(got) != 2 || got[0].Polish != nil || got[1].Polish != nil {
		t.Fatalf("after failed polish: %+v, want nothing saved", got)
	}
	wantErr(t, s.PolishHighlights(ctx, polishing(uuid.NewString(), nil)), store.ErrNotFound)
}

func polishSkipsDismissed(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := newSession(t, s, base)
	gone, kept := highlight(sess.ID, 1, store.KindDecision, 0.9), highlight(sess.ID, 2, store.KindAction, 0.9)
	if err := s.AddHighlights(ctx, []store.Highlight{gone, kept}); err != nil {
		t.Fatal(err)
	}
	if err := s.DismissHighlight(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	// Dismissed while the pass ran: the rest of the pass is saved.
	err := s.PolishHighlights(ctx, polishing(sess.ID, map[string]store.Polish{
		gone.ID: polishOf("Ship the beta Monday", 0.9), kept.ID: polishOf("Tom reruns the load test", 0.9),
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := highlightsOf(t, s, sess.ID)
	if got[0].Polish != nil || got[1].Polish == nil {
		t.Fatalf("after polishing: dismissed %+v, kept %+v; want the dismissed one left alone", got[0].Polish, got[1].Polish)
	}
}

func debatesOnTheirTurn(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := newSession(t, s, base)
	decided := highlight(sess.ID, 9, store.KindDecision, 0.9)
	if err := s.AddHighlights(ctx, []store.Highlight{decided}); err != nil {
		t.Fatal(err)
	}
	first := debate(sess.ID, 6, "Ship Monday or wait a week", store.DebateOpen)
	if err := s.PolishHighlights(ctx, polishing(sess.ID, nil, first)); err != nil {
		t.Fatal(err)
	}
	got := highlightsOf(t, s, sess.ID)
	if len(got) != 2 || got[0].ID != first.ID || got[0].Kind != store.KindDebate || got[0].Polish == nil ||
		!reflect.DeepEqual(got[0].Polish.Debate, first.Polish.Debate) || !got[0].PolishedAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("after adding a debate: %+v", got)
	}
	// The same turn again updates the debate: its ID stays.
	settled := debate(sess.ID, 6, "Ship Monday or wait a week", store.DebateDecided)
	settled.Polish.Debate.Decision = decided.ID
	if err := s.PolishHighlights(ctx, polishing(sess.ID, nil, settled)); err != nil {
		t.Fatal(err)
	}
	got = highlightsOf(t, s, sess.ID)
	if len(got) != 2 || got[0].ID != first.ID || got[0].Polish.Debate.Status != store.DebateDecided || got[0].Polish.Debate.Decision != decided.ID {
		t.Fatalf("after updating the debate: %+v", got[0])
	}
	// Dismissed, it is never brought back.
	if err := s.DismissHighlight(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.PolishHighlights(ctx, polishing(sess.ID, nil, debate(sess.ID, 6, "Ship Monday or not", store.DebateOpen))); err != nil {
		t.Fatal(err)
	}
	got = highlightsOf(t, s, sess.ID)
	if len(got) != 2 || !got[0].Dismissed || got[0].Polish.Debate.Status != store.DebateDecided {
		t.Fatalf("after raising a dismissed debate again: %+v", got[0])
	}
	// A turn Highlights marked meanwhile wins: the debate on it is
	// skipped, the rest saved.
	if err := s.PolishHighlights(ctx, polishing(sess.ID, map[string]store.Polish{decided.ID: polishOf("Ship it", 0.9)},
		debate(sess.ID, 9, "Ship or wait", store.DebateOpen))); err != nil {
		t.Fatalf("a debate on a marked turn: %v, want it skipped", err)
	}
	got = highlightsOf(t, s, sess.ID)
	if len(got) != 2 || got[1].Kind != store.KindDecision || got[1].Polish == nil {
		t.Fatalf("after a debate on a marked turn: %+v, want the decision kept and polished", got[1])
	}
	// A debate's polish goes with it, never in Polish; and only a debate
	// has a debate's polish.
	wantErr(t, s.PolishHighlights(ctx, polishing(sess.ID, map[string]store.Polish{first.ID: polishOf("A debate", 0.9)})), store.ErrInvalid)
	notOne := debate(sess.ID, 11, "Ship or wait", store.DebateOpen)
	notOne.Kind = store.KindAction
	wantErr(t, s.AddHighlights(ctx, []store.Highlight{notOne}), store.ErrInvalid)

	// Decisions marks a debate bare; polish enriches it, keeping its ID
	// and Decisions' confidence.
	marked := highlight(sess.ID, 7, store.KindDebate, 0.82)
	if err := s.AddHighlights(ctx, []store.Highlight{marked}); err != nil {
		t.Fatalf("a debate Decisions marked: %v", err)
	}
	enriched := debate(sess.ID, 7, "Ship Monday or wait a week", store.DebateOpen)
	enriched.Confidence = 0.5
	if err := s.PolishHighlights(ctx, polishing(sess.ID, nil, enriched)); err != nil {
		t.Fatal(err)
	}
	for _, h := range highlightsOf(t, s, sess.ID) {
		if h.Seq == 7 && (h.ID != marked.ID || h.Confidence != 0.82 || h.Polish == nil || h.Polish.Debate.Topic != "Ship Monday or wait a week") {
			t.Fatalf("the debate Decisions marked, enriched: %+v", h)
		}
	}
}

func polishTrashedAndPurged(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := ended(t, s, base)
	h := highlight(sess.ID, 0, store.KindDecision, 0.9)
	if err := s.AddHighlights(ctx, []store.Highlight{h}); err != nil {
		t.Fatal(err)
	}
	mustTrash(t, s, sess.ID)
	// The final pass can end after the session is trashed: it is kept.
	if err := s.PolishHighlights(ctx, polishing(sess.ID, map[string]store.Polish{h.ID: polishOf("Ship it", 0.9)})); err != nil {
		t.Fatalf("polish a trashed session: %v", err)
	}
	if err := s.PurgeSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	wantErr(t, s.PolishHighlights(ctx, polishing(sess.ID, map[string]store.Polish{h.ID: polishOf("Ship it", 0.9)})), store.ErrNotFound)
}
