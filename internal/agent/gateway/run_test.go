package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// scripted is an agent whose runs report what the test sends them.
type scripted struct {
	info  Info
	probe Probe
	// reject fails a run that resumes a conversation as ResumeRejected.
	reject bool

	mu      sync.Mutex
	queries []Query
	runs    []*scriptedRun
}

func newScripted(id string) *scripted {
	return &scripted{info: Info{ID: id, Name: id}, probe: Probe{Found: true, Path: "/bin/" + id, Version: "1.0.0"}}
}

func (a *scripted) Info() Info                  { return a.info }
func (a *scripted) Probe(context.Context) Probe { return a.probe }

func (a *scripted) Run(_ context.Context, q Query) (Run, error) {
	r := &scriptedRun{events: make(chan Event, 16)}
	a.mu.Lock()
	a.queries = append(a.queries, q)
	a.runs = append(a.runs, r)
	a.mu.Unlock()
	if a.reject && q.ConversationID != "" {
		r.end(Failed{Kind: ResumeRejected, Code: 1})
	}
	return r, nil
}

// last is the agent's latest run and its query.
func (a *scripted) last(t *testing.T) (*scriptedRun, Query) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.runs) == 0 {
		t.Fatal("the agent never ran")
	}
	return a.runs[len(a.runs)-1], a.queries[len(a.queries)-1]
}

func (a *scripted) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.runs)
}

type scriptedRun struct {
	mu      sync.Mutex
	events  chan Event
	closed  bool
	stopped atomic.Bool
}

func (r *scriptedRun) Events() <-chan Event { return r.events }

// send reports e, unless the run ended.
func (r *scriptedRun) send(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.events <- e
	}
}

// end reports e and closes the run.
func (r *scriptedRun) end(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.events <- e
		r.closed = true
		close(r.events)
	}
}

func (r *scriptedRun) Stop() {
	r.stopped.Store(true)
	r.end(Failed{Kind: Exited, Code: -1})
}

// collect reads a turn's events to their end, failing after a while.
func collect(t *testing.T, turn *Turn) []Event {
	t.Helper()
	var got []Event
	deadline := time.After(10 * time.Second)
	for {
		select {
		case e, ok := <-turn.Events():
			if !ok {
				return got
			}
			got = append(got, e)
		case <-deadline:
			t.Fatalf("the turn never ended; so far %v", got)
		}
	}
}

func last(events []Event) Event {
	if len(events) == 0 {
		return nil
	}
	return events[len(events)-1]
}

func TestSendRoutesToTheAgentOfTheIDWithTheRunsTools(t *testing.T) {
	a, b := newScripted("a"), newScripted("b")
	g := New(Options{Command: func(run string) []string { return []string{"capy", "mcp", "-run", run} }}, a, b)
	turn, err := g.Send(context.Background(), "b", Query{Prompt: "q", ConversationID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	run, q := b.last(t)
	if a.count() != 0 || q.Prompt != "q" || q.ConversationID != "c1" {
		t.Fatalf("a ran %d times; b got %+v", a.count(), q)
	}
	if want := []string{"capy", "mcp", "-run", turn.ID}; strings.Join(q.Tools.Command, " ") != strings.Join(want, " ") {
		t.Fatalf("tools %v, want the run's own %v", q.Tools.Command, want)
	}
	run.send(Started{ConversationID: "c1"})
	run.end(Done{})
	if got := collect(t, turn); len(got) != 2 || got[0] != (Started{ConversationID: "c1"}) || got[1] != (Done{}) {
		t.Fatalf("events %v", got)
	}
}

func TestSendRefusesAnAgentItCannotUse(t *testing.T) {
	missing, old := newScripted("missing"), newScripted("old")
	missing.probe = Probe{}
	old.probe.TooOld = "2.1.0"
	on := atomic.Bool{}
	g := New(Options{Enabled: on.Load}, missing, old, newScripted("ok"))
	for _, id := range []string{"cursor", "missing", "old", "ok"} {
		if _, err := g.Send(context.Background(), id, Query{}); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s with Agent off: %v, want ErrUnavailable", id, err)
		}
	}
	on.Store(true)
	for _, id := range []string{"cursor", "missing", "old"} {
		if _, err := g.Send(context.Background(), id, Query{}); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: %v, want ErrUnavailable", id, err)
		}
	}
	if turn, err := g.Send(context.Background(), "ok", Query{}); err != nil {
		t.Fatalf("a usable agent: %v", err)
	} else {
		turn.Stop()
		collect(t, turn)
	}
}

func TestConsentIsAskedOnceForAnAgentThatSends(t *testing.T) {
	cloud, local := newScripted("cloud"), newScripted("local")
	cloud.info.Sends = &Recipient{Provider: "Anthropic", Account: "your Claude account"}
	g := New(Options{}, cloud, local)
	if _, err := g.Send(context.Background(), "cloud", Query{}); !errors.Is(err, ErrConsentNeeded) || cloud.count() != 0 {
		t.Fatalf("before Allow: %v, %d runs; want ErrConsentNeeded and nothing sent", err, cloud.count())
	}
	if err := g.Allow("cloud"); err != nil {
		t.Fatal(err)
	}
	for _, a := range []*scripted{cloud, local} {
		turn, err := g.Send(context.Background(), a.info.ID, Query{})
		if err != nil {
			t.Fatalf("%s: %v", a.info.ID, err)
		}
		turn.Stop()
		collect(t, turn)
	}
}

func TestOneRunPerConversation(t *testing.T) {
	a := newScripted("a")
	g := New(Options{}, a)
	first, err := g.Send(context.Background(), "a", Query{ConversationID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Send(context.Background(), "a", Query{ConversationID: "c1"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("a second run of c1: %v, want ErrBusy", err)
	}
	// New conversations never wait for each other.
	for range 2 {
		turn, err := g.Send(context.Background(), "a", Query{})
		if err != nil {
			t.Fatalf("a new conversation: %v", err)
		}
		turn.Stop()
		collect(t, turn)
	}
	a.runs[0].end(Done{})
	collect(t, first)
	again, err := g.Send(context.Background(), "a", Query{ConversationID: "c1"})
	if err != nil {
		t.Fatalf("c1 after its run ended: %v", err)
	}
	again.Stop()
	collect(t, again)
}

func TestLimitsStopASilentOrEndlessRunAsStalled(t *testing.T) {
	for name, o := range map[string]Options{
		"silent":  {Idle: 100 * time.Millisecond, Total: time.Minute},
		"endless": {Idle: time.Minute, Total: 300 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			a := newScripted("a")
			g := New(o, a)
			turn, err := g.Send(context.Background(), "a", Query{})
			if err != nil {
				t.Fatal(err)
			}
			run, _ := a.last(t)
			done := make(chan struct{})
			go func() { // an endless run keeps talking
				for name == "endless" {
					select {
					case <-done:
						return
					case <-time.After(30 * time.Millisecond):
						run.send(TextDelta{Text: "."})
					}
				}
			}()
			got := collect(t, turn)
			close(done)
			if f, ok := last(got).(Failed); !ok || f.Kind != Stalled || !run.stopped.Load() {
				t.Fatalf("ended with %v, agent stopped %v; want Stalled and the agent stopped", last(got), run.stopped.Load())
			}
		})
	}
}

func TestStopCloseAndTheGateEndARunAsStopped(t *testing.T) {
	on := atomic.Bool{}
	on.Store(true)
	stoppers := map[string]func(g *Gateway, turn *Turn){
		"stop":     func(_ *Gateway, turn *Turn) { turn.Stop() },
		"close":    func(g *Gateway, _ *Turn) { go g.Close() },
		"gate off": func(*Gateway, *Turn) { on.Store(false) },
	}
	for name, stop := range stoppers {
		t.Run(name, func(t *testing.T) {
			on.Store(true)
			a := newScripted("a")
			g := New(Options{Enabled: on.Load}, a)
			turn, err := g.Send(context.Background(), "a", Query{})
			if err != nil {
				t.Fatal(err)
			}
			run, _ := a.last(t)
			stop(g, turn)
			if f, ok := last(collect(t, turn)).(Failed); !ok || f.Kind != Stopped || !run.stopped.Load() {
				t.Fatalf("ended with %v; want Stopped and the agent stopped", f)
			}
		})
	}
}

func TestCloseWaitsForEveryRunAndRefusesMore(t *testing.T) {
	a := newScripted("a")
	g := New(Options{}, a)
	var turns []*Turn
	for range 3 {
		turn, err := g.Send(context.Background(), "a", Query{})
		if err != nil {
			t.Fatal(err)
		}
		turns = append(turns, turn)
	}
	var wg sync.WaitGroup
	for _, turn := range turns {
		wg.Go(func() { collect(t, turn) })
	}
	g.Close() // returns once every run ended
	for i, r := range a.runs {
		if !r.stopped.Load() {
			t.Errorf("run %d still going after Close", i)
		}
	}
	wg.Wait()
	if _, err := g.Send(context.Background(), "a", Query{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Send after Close: %v, want ErrClosed", err)
	}
}

// TestARunEndingWithNeitherDoneNorFailedFails keeps an adapter's bug from
// passing for an answer.
func TestARunEndingWithNeitherDoneNorFailedFails(t *testing.T) {
	a := newScripted("a")
	g := New(Options{}, a)
	turn, err := g.Send(context.Background(), "a", Query{})
	if err != nil {
		t.Fatal(err)
	}
	run, _ := a.last(t)
	run.send(Message{Text: "half"})
	run.mu.Lock()
	run.closed = true
	close(run.events)
	run.mu.Unlock()
	if f, ok := last(collect(t, turn)).(Failed); !ok || f.Kind != Exited {
		t.Fatalf("ended with %v, want Exited", f)
	}
}

func TestARejectedResumeStartsOverCarryingTheThread(t *testing.T) {
	a := newScripted("a")
	a.reject = true
	g := New(Options{}, a)
	turn, err := g.Send(context.Background(), "a", Query{Prompt: "and then?", ConversationID: "lost", Restart: "earlier: …; and then?", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); a.count() < 2; {
		if time.Now().After(deadline) {
			t.Fatal("no new conversation was started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	run, q := a.last(t)
	if q.ConversationID != "" || q.Prompt != "earlier: …; and then?" || q.Model != "m" {
		t.Fatalf("restarted with %+v, want a new conversation with the Restart prompt", q)
	}
	run.send(Started{ConversationID: "new"})
	run.end(Done{})
	got := collect(t, turn)
	for _, e := range got {
		if f, ok := e.(Failed); ok {
			t.Fatalf("the rejection reached the caller: %v", f)
		}
	}
	if len(got) != 2 || got[0] != (Started{ConversationID: "new"}) {
		t.Fatalf("events %v, want the new conversation's", got)
	}

	// Without a thread to carry, the rejection is the caller's to handle.
	turn, err = g.Send(context.Background(), "a", Query{ConversationID: "lost"})
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := last(collect(t, turn)).(Failed); !ok || f.Kind != ResumeRejected {
		t.Fatalf("ended with %v, want ResumeRejected", f)
	}
}

func TestAllowedNeedsConsentOnlyOfAnAgentThatSends(t *testing.T) {
	local, remote := newScripted("local"), newScripted("remote")
	remote.info.Sends = &Recipient{Provider: "Anthropic", Account: "your Claude account"}
	g := New(Options{}, local, remote)
	if !g.Allowed("local") || g.Allowed("remote") || g.Allowed("missing") {
		t.Fatal("Allowed before consent: want the local agent only")
	}
	if err := g.Allow("remote"); err != nil {
		t.Fatal(err)
	}
	if !g.Allowed("remote") {
		t.Fatal("not Allowed after Allow")
	}
}

// TestARunWithNoToolsGetsNoneAndIsNotNamed guards a query that needs no
// tools: neither Capy's server nor a run file that would let Capy mcp
// serve it as the agent.
func TestARunWithNoToolsGetsNoneAndIsNotNamed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runs")
	a := newScripted("a")
	served := false
	g := New(Options{RunsDir: dir, Command: func(string) []string { return []string{"capy", "mcp"} },
		Server: func(string) *mcp.Server { served = true; return nil }}, a)
	turn, err := g.Send(context.Background(), "a", Query{Prompt: "polish", NoTools: true})
	if err != nil {
		t.Fatal(err)
	}
	run, q := a.last(t)
	if q.Tools.Command != nil || q.Tools.Open != nil || !q.NoTools {
		t.Fatalf("a run with no tools got %+v", q)
	}
	if RunGoing(dir, turn.ID) || served {
		t.Fatal("a run with no tools is named, or served Capy's tools")
	}
	// A tool it finds anyway, as a later version may bring one back, ends
	// the run, failed.
	run.send(Message{Text: "Let me look."})
	run.send(ToolCall{Name: "shell"})
	got := collect(t, turn)
	if f, ok := last(got).(Failed); !ok || f.Kind != Exited || !run.stopped.Load() {
		t.Fatalf("events %v, stopped %v; want the run stopped and failed", got, run.stopped.Load())
	}
	for _, e := range got {
		if _, tool := e.(ToolCall); tool {
			t.Fatalf("the tool call reached the turn: %v", got)
		}
	}
}

func TestARunFileNamesTheRunWhileItGoes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runs")
	a := newScripted("a")
	g := New(Options{RunsDir: dir}, a)
	turn, err := g.Send(context.Background(), "a", Query{})
	if err != nil {
		t.Fatal(err)
	}
	if !RunGoing(dir, turn.ID) {
		t.Fatal("a run going is not named")
	}
	// Only the gateway's own form: on a case-insensitive disk, another
	// would find the run's file but record edits Ask cannot list.
	for _, id := range []string{"", "..", "../runs/" + turn.ID, "not-a-uuid", strings.ToUpper(turn.ID), "{" + turn.ID + "}", "urn:uuid:" + turn.ID} {
		if RunGoing(dir, id) {
			t.Errorf("RunGoing(%q) holds", id)
		}
	}
	turn.Stop()
	collect(t, turn)
	if RunGoing(dir, turn.ID) {
		t.Fatal("an ended run is still named")
	}
}

func TestToolsOpenServesTheRunsOwnServer(t *testing.T) {
	var served []string
	var mu sync.Mutex
	server := func(run string) *mcp.Server {
		mu.Lock()
		served = append(served, run)
		mu.Unlock()
		s := mcp.NewServer(&mcp.Implementation{Name: "capy"}, nil)
		mcp.AddTool(s, &mcp.Tool{Name: "whoami"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, map[string]string, error) {
			return nil, map[string]string{"run": run}, nil
		})
		return s
	}
	a := newScripted("a")
	g := New(Options{Server: server}, a)
	turn, err := g.Send(context.Background(), "a", Query{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { turn.Stop(); collect(t, turn) }()
	_, q := a.last(t)
	cs, err := q.Tools.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(res.StructuredContent); !strings.Contains(got, turn.ID) || len(served) != 1 || served[0] != turn.ID {
		t.Fatalf("served %v, answered %s; want the run's server", served, got)
	}
}

// recorder keeps every log record's message and attributes as text.
type recorder struct {
	mu   sync.Mutex
	logs []string
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *recorder) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r *recorder) WithGroup(string) slog.Handler            { return r }
func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	var b strings.Builder
	b.WriteString(rec.Message)
	rec.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	r.mu.Lock()
	r.logs = append(r.logs, b.String())
	r.mu.Unlock()
	return nil
}

func TestLogsNeverCarryPromptsAnswersOrToolData(t *testing.T) {
	rec := &recorder{}
	a := newScripted("a")
	g := New(Options{Log: slog.New(rec)}, a)
	const session = "11111111-1111-4111-8111-111111111111"
	turn, err := g.Send(context.Background(), "a", Query{Prompt: "SECRET question", Restart: "SECRET thread"})
	if err != nil {
		t.Fatal(err)
	}
	run, _ := a.last(t)
	run.send(Started{ConversationID: "c1"})
	run.send(ToolCall{Name: "get_session", SessionID: session})
	run.send(ToolCall{Name: "Bash(cat SECRET)"})
	run.send(TextDelta{Text: "SECRET "})
	run.send(Message{Text: "SECRET answer"})
	run.end(Done{})
	collect(t, turn)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	all := strings.Join(rec.logs, "\n")
	if strings.Contains(all, "SECRET") || strings.Contains(all, session) {
		t.Fatalf("content reached the logs:\n%s", all)
	}
	if !strings.Contains(all, "tool=get_session") || !strings.Contains(all, "tool=other") || !strings.Contains(all, "text_bytes=20") {
		t.Fatalf("the logs lack the tools and byte counts:\n%s", all)
	}
}

// TestARunIsReleasedWhenItsEndIsSeen lets the conversation's next
// question go as soon as the last one's Done is read, while the agent's
// program is still exiting.
func TestARunIsReleasedWhenItsEndIsSeen(t *testing.T) {
	a := newScripted("a")
	g := New(Options{}, a)
	turn, err := g.Send(context.Background(), "a", Query{ConversationID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	run, _ := a.last(t)
	run.send(Done{})
	go func() { // a CLI closes its output a moment after its result
		time.Sleep(200 * time.Millisecond)
		run.end(Done{})
	}()
	for e := range turn.Events() {
		if e == (Done{}) {
			break
		}
	}
	next, err := g.Send(context.Background(), "a", Query{ConversationID: "c1"})
	if err != nil {
		t.Fatalf("the follow-up as Done is read: %v", err)
	}
	next.Stop()
	collect(t, next)
}
