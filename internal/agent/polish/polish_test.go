package polish

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/profile"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/store/sqlite"
)

// polisher is an agent whose answer to each run is reply's, by the run's
// number from 0; hold, while set, keeps a run from answering until it is
// closed.
type polisher struct {
	reply func(n int, prompt string) (string, *gateway.Failed)

	mu      sync.Mutex
	queries []gateway.Query
	hold    chan struct{}
	stopped atomic.Int32 // runs stopped before they answered
}

func (a *polisher) Info() gateway.Info { return gateway.Info{ID: "polisher", Name: "Polisher"} }
func (a *polisher) Probe(context.Context) gateway.Probe {
	return gateway.Probe{Found: true, Path: "/bin/polisher", Version: "1.0.0"}
}

func (a *polisher) Run(ctx context.Context, q gateway.Query) (gateway.Run, error) {
	a.mu.Lock()
	n, hold := len(a.queries), a.hold
	a.queries = append(a.queries, q)
	a.mu.Unlock()
	r := &polishRun{events: make(chan gateway.Event, 4), stop: make(chan struct{})}
	go func() {
		defer close(r.events)
		if hold != nil {
			select {
			case <-hold:
			case <-r.stop:
				a.stopped.Add(1)
				r.events <- gateway.Failed{Kind: gateway.Stopped, Code: -1}
				return
			}
		}
		text, failed := a.reply(n, q.Prompt)
		if text != "" {
			r.events <- gateway.Message{Text: text}
		}
		if failed != nil {
			r.events <- *failed
			return
		}
		r.events <- gateway.Done{}
	}()
	return r, nil
}

func (a *polisher) runs() []gateway.Query {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]gateway.Query(nil), a.queries...)
}

type polishRun struct {
	events chan gateway.Event
	stop   chan struct{}
	once   sync.Once
}

func (r *polishRun) Events() <-chan gateway.Event { return r.events }
func (r *polishRun) Stop()                        { r.once.Do(func() { close(r.stop) }) }

// answer is an answer of ops, as an agent writes it.
func answer(ops ...string) string {
	return "Done.\n```json\n{\"ops\": [" + strings.Join(ops, ",") + "]}\n```"
}

type fixture struct {
	p       *Polish
	lib     *sqlite.DB
	agent   *polisher
	session string
	on      atomic.Bool
	chosen  atomic.Value // string

	mu      sync.Mutex
	saved   int
	stopped []Stop
}

// newFixture is Polish on a library holding a session of 20 turns, its
// agent the polisher, on.
func newFixture(t *testing.T, gap time.Duration, reply func(n int, prompt string) (string, *gateway.Failed)) *fixture {
	t.Helper()
	ctx := context.Background()
	lib, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "capy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lib.Close() })
	f := &fixture{lib: lib, agent: &polisher{reply: reply}, session: uuid.NewString()}
	f.on.Store(true)
	f.chosen.Store("polisher")
	if err := lib.CreateSession(ctx, store.Session{ID: f.session, Mode: store.ModeCaptions, Source: store.SourceMic,
		LangA: "en-US", LangB: "zh-CN", StartedAt: at}); err != nil {
		t.Fatal(err)
	}
	for seq := range 20 {
		text := "turn " + string(rune('a'+seq))
		if seq == 3 {
			text = "Alex, can you own the pricing copy by Friday?"
		}
		if err := lib.AppendSegment(ctx, store.Segment{ID: uuid.NewString(), SessionID: f.session, Seq: seq, Lang: "en-US",
			Text: text, Speaker: "Speaker 1", StartMs: int64(seq) * 10_000, EndMs: int64(seq)*10_000 + 9_000}); err != nil {
			t.Fatal(err)
		}
	}
	gw := gateway.New(gateway.Options{}, f.agent)
	t.Cleanup(gw.Close)
	f.p = New(Options{Store: lib, Gateway: gw, Gap: gap,
		Agent:   func() string { return f.chosen.Load().(string) },
		On:      f.on.Load,
		Profile: func() profile.Profile { return profile.Profile{Name: "Alex Rivera"} },
		Saved:   func(string) { f.mu.Lock(); f.saved++; f.mu.Unlock() },
		Stopped: func(s Stop) { f.mu.Lock(); f.stopped = append(f.stopped, s); f.mu.Unlock() },
	})
	t.Cleanup(f.p.Close)
	return f
}

// mark adds a highlight of kind k on turn seq.
func (f *fixture) mark(t *testing.T, seq int, k store.HighlightKind) store.Highlight {
	t.Helper()
	h := store.Highlight{ID: uuid.NewString(), SessionID: f.session, Seq: seq, Kind: k, Confidence: 0.9, CreatedAt: at}
	if err := f.lib.AddHighlights(context.Background(), []store.Highlight{h}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (f *fixture) highlights(t *testing.T) map[int]store.Highlight {
	t.Helper()
	d, err := f.lib.GetSession(context.Background(), f.session)
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]store.Highlight{}
	for _, h := range d.Highlights {
		out[h.Seq] = h
	}
	return out
}

func (f *fixture) stops() []Stop {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Stop(nil), f.stopped...)
}

// eventually fails unless cond holds within a few seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// never fails if cond holds within d.
func never(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			t.Fatalf("yet: %s", what)
		}
	}
}

func TestALivePassPolishesWithNoToolsAndSaves(t *testing.T) {
	f := newFixture(t, time.Hour, func(int, string) (string, *gateway.Failed) {
		return answer(`{"op": "polish", "turn": "t3", "text": "You were asked to own the pricing copy by Friday.", "owner": "Alex", "due": "Friday", "confidence": 0.9}`), nil
	})
	asked := f.mark(t, 3, store.KindAskedYou)
	f.p.Notify(f.session, false)
	eventually(t, "the highlight polished", func() bool { return f.highlights(t)[3].Polish != nil })
	got := f.highlights(t)[3]
	if p := got.Polish; got.ID != asked.ID || p.ByAgent != "Polisher" || p.Owner != "Alex" || p.Due != "Friday" || p.Tentative() {
		t.Fatalf("polish %+v", p)
	}
	runs := f.agent.runs()
	if len(runs) != 1 || !runs[0].NoTools || runs[0].Tools.Command != nil || runs[0].Tools.Open != nil {
		t.Fatalf("runs %+v; want one, with no tools", runs)
	}
	if !strings.Contains(runs[0].Prompt, "t3 asked_you · to polish") || !strings.Contains(runs[0].Prompt, "<listener>Alex Rivera</listener>") {
		t.Fatalf("the prompt lacks the highlight or the listener:\n%s", runs[0].Prompt)
	}
}

func TestLivePassesCoalesceAndWaitTheGap(t *testing.T) {
	const gap = 400 * time.Millisecond
	f := newFixture(t, gap, func(n int, _ string) (string, *gateway.Failed) { return answer(), nil })
	f.mark(t, 3, store.KindDecision)
	f.p.Notify(f.session, false)
	eventually(t, "the first pass", func() bool { return len(f.agent.runs()) == 1 })
	started := time.Now()
	f.mark(t, 9, store.KindAction)
	for range 3 {
		f.p.Notify(f.session, false)
	}
	never(t, "a second pass within the gap", gap/2, func() bool { return len(f.agent.runs()) > 1 })
	eventually(t, "the second pass", func() bool { return len(f.agent.runs()) == 2 })
	if time.Since(started) < gap/2 {
		t.Fatalf("the second pass began %v after the first, within the gap %v", time.Since(started), gap)
	}
	never(t, "a third pass for three notices", gap+gap/2, func() bool { return len(f.agent.runs()) > 2 })
	// The highlight left unpolished by an empty answer is offered again.
	if !strings.Contains(f.agent.runs()[1].Prompt, "t3 decision · to polish") {
		t.Fatalf("the second pass did not offer t3 again:\n%s", f.agent.runs()[1].Prompt)
	}
}

func TestTheFinalPassFollowsTheLiveOneAndPolishesEverything(t *testing.T) {
	f := newFixture(t, time.Hour, func(n int, _ string) (string, *gateway.Failed) {
		if n == 0 {
			return answer(`{"op": "polish", "turn": "t3", "text": "Ship Monday.", "decided_by": "group"}`), nil
		}
		return answer(`{"op": "polish", "turn": "t3", "text": "The beta ships Monday."}`, `{"op": "polish", "turn": "t12", "text": "Tom reruns the test."}`), nil
	})
	f.agent.hold = make(chan struct{})
	f.mark(t, 3, store.KindDecision)
	f.p.Notify(f.session, false)
	eventually(t, "the live pass", func() bool { return len(f.agent.runs()) == 1 })
	f.mark(t, 12, store.KindAction)
	f.p.Notify(f.session, true) // Stop, the live pass still running
	never(t, "the final pass beside the live one", 100*time.Millisecond, func() bool { return len(f.agent.runs()) > 1 })
	f.agent.mu.Lock()
	close(f.agent.hold)
	f.agent.hold = nil
	f.agent.mu.Unlock()
	eventually(t, "the final pass", func() bool { return f.highlights(t)[12].Polish != nil })
	runs := f.agent.runs()
	if len(runs) != 2 || !strings.Contains(runs[1].Prompt, "t3 decision · to polish again") {
		t.Fatalf("%d runs; want the final pass to polish t3 again:\n%s", len(runs), runs[len(runs)-1].Prompt)
	}
	if got := f.highlights(t)[3].Polish.Text; got != "The beta ships Monday." {
		t.Fatalf("t3 reads %q, want the final pass's", got)
	}
}

func TestSignedOutStopsPolishForTheSessionUntilRetry(t *testing.T) {
	var signedIn atomic.Bool
	f := newFixture(t, 0, func(int, string) (string, *gateway.Failed) {
		if !signedIn.Load() {
			return "", &gateway.Failed{Kind: gateway.SignedOut, Code: 1}
		}
		return answer(`{"op": "polish", "turn": "t3", "text": "Ship Monday."}`), nil
	})
	f.mark(t, 3, store.KindDecision)
	f.p.Notify(f.session, false)
	eventually(t, "polish stopped", func() bool { return len(f.stops()) == 1 })
	if s := f.stops()[0]; s.Reason != SignedOut || s.SessionID != f.session || s.Agent != "polisher" {
		t.Fatalf("stop %+v", s)
	}
	if s, ok := f.p.LastStop(); !ok || s.Reason != SignedOut {
		t.Fatalf("LastStop %+v, %v", s, ok)
	}
	f.p.Notify(f.session, true)
	never(t, "a pass after the stop", 150*time.Millisecond, func() bool { return len(f.agent.runs()) > 1 })
	signedIn.Store(true)
	f.p.Retry(f.session)
	eventually(t, "polished after Try Again", func() bool { return f.highlights(t)[3].Polish != nil })
	if _, ok := f.p.LastStop(); ok {
		t.Fatal("the stop is still reported after Try Again")
	}
}

func TestThreeAnswersWithoutOpsStopIt(t *testing.T) {
	f := newFixture(t, 0, func(int, string) (string, *gateway.Failed) { return "I updated the highlights.", nil })
	f.mark(t, 3, store.KindDecision)
	for n := 1; n <= maxFailed; n++ {
		f.p.Notify(f.session, true)
		eventually(t, "a pass", func() bool { return len(f.agent.runs()) == n })
	}
	eventually(t, "polish stopped", func() bool { return len(f.stops()) == 1 })
	if f.stops()[0].Reason != Failing {
		t.Fatalf("stop %+v, want Failing", f.stops()[0])
	}
	f.p.Notify(f.session, true)
	never(t, "a pass after the stop", 150*time.Millisecond, func() bool { return len(f.agent.runs()) > maxFailed })
}

func TestNoPassWhileOffAndAMissingAgentStopsIt(t *testing.T) {
	f := newFixture(t, 0, func(int, string) (string, *gateway.Failed) { return answer(), nil })
	f.mark(t, 3, store.KindDecision)
	f.on.Store(false)
	f.p.Notify(f.session, true)
	never(t, "a pass while off", 150*time.Millisecond, func() bool { return len(f.agent.runs()) > 0 })
	f.on.Store(true)
	f.chosen.Store("gone")
	f.p.Notify(f.session, true)
	eventually(t, "polish stopped", func() bool { return len(f.stops()) == 1 })
	if s := f.stops()[0]; s.Reason != Unavailable || s.Agent != "gone" {
		t.Fatalf("stop %+v, want Unavailable for the agent gone", s)
	}
}

func TestADismissedHighlightIsNeverPolished(t *testing.T) {
	var dismiss func()
	f := newFixture(t, 0, func(int, string) (string, *gateway.Failed) {
		dismiss() // the user dismisses it while the agent answers
		return answer(`{"op": "polish", "turn": "t3", "text": "Ship Monday."}`, `{"op": "polish", "turn": "t12", "text": "Tom tests it."}`), nil
	})
	gone, kept := f.mark(t, 3, store.KindDecision), f.mark(t, 12, store.KindAction)
	dismiss = func() { _ = f.lib.DismissHighlight(context.Background(), gone.ID) }
	f.p.Notify(f.session, true)
	eventually(t, "the pass saved", func() bool { return f.highlights(t)[12].Polish != nil })
	if hs := f.highlights(t); hs[3].Polish != nil || hs[12].ID != kept.ID {
		t.Fatalf("the dismissed highlight was polished: %+v", hs[3].Polish)
	}
}

func TestTurningOffHaltsThePassGoing(t *testing.T) {
	f := newFixture(t, 0, func(int, string) (string, *gateway.Failed) {
		return answer(`{"op": "polish", "turn": "t3", "text": "Ship Monday."}`), nil
	})
	f.agent.hold = make(chan struct{}) // the agent answers only once released
	f.mark(t, 3, store.KindDecision)
	f.p.Notify(f.session, true)
	eventually(t, "the pass", func() bool { return len(f.agent.runs()) == 1 })
	f.on.Store(false)
	f.p.Halt()
	eventually(t, "the run going stopped", func() bool { return f.agent.stopped.Load() == 1 })
	never(t, "polish saved after it was turned off", 300*time.Millisecond, func() bool { return f.highlights(t)[3].Polish != nil })
	if len(f.stops()) != 0 {
		t.Fatalf("stops %+v: turning off is no failure", f.stops())
	}
	// Turned on again, the next pass runs.
	f.agent.mu.Lock()
	f.agent.hold = nil
	f.agent.mu.Unlock()
	f.on.Store(true)
	f.p.Notify(f.session, true)
	eventually(t, "polished once on again", func() bool { return f.highlights(t)[3].Polish != nil })
}
