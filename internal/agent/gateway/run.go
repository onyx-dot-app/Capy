package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Turn is a query's run through the gateway.
type Turn struct {
	// ID is the run's: the origin of every edit the agent makes with
	// Capy's tools in it.
	ID     string
	events chan Event
	stop   chan struct{}
	once   sync.Once
}

// Events reports the run as its agent does, and is closed after Done or
// Failed. Read it until it is closed.
func (t *Turn) Events() <-chan Event { return t.events }

// Stop ends the run: Events then reports Failed{Stopped}.
func (t *Turn) Stop() { t.once.Do(func() { close(t.stop) }) }

// drainWait bounds how long a stopped run's agent may take to close its
// events.
const drainWait = 5 * time.Second

// Send runs q with the agent of agentID: the user's choice for a new
// conversation, the conversation's own agent for a follow-up. It is
// ErrUnavailable for an agent not found or too old, or with Agent off;
// ErrConsentNeeded until the user allows an agent that sends questions
// off this Mac; ErrBusy while the conversation has a run going. Send may
// probe the agents: never call it on the main thread.
func (g *Gateway) Send(ctx context.Context, agentID string, q Query) (*Turn, error) {
	if !g.o.Enabled() {
		return nil, ErrUnavailable
	}
	a, err := g.usable(ctx, agentID)
	if err != nil {
		return nil, err
	}
	if a.Info().Sends != nil && !g.o.Consent.Allowed(agentID) {
		return nil, ErrConsentNeeded
	}
	t := &Turn{ID: uuid.NewString(), events: make(chan Event, 64), stop: make(chan struct{})}
	if err := g.begin(t, q.ConversationID, !q.NoTools); err != nil {
		return nil, err
	}
	if !q.NoTools {
		q.Tools = g.tools(t.ID, agentID)
	}
	run, err := g.start(a, t, q)
	if err != nil {
		g.end(t, q.ConversationID)
		return nil, err
	}
	go g.supervise(a, t, q, run)
	return t, nil
}

// Usable reports whether agent id can be sent a question, probing it
// alone when the agents were not probed yet. Never call it on the main
// thread.
func (g *Gateway) Usable(ctx context.Context, id string) bool {
	_, err := g.usable(ctx, id)
	return err == nil
}

// usable is the registered agent of id when its last probe found it
// usable: the probe of every agent, else one of this agent alone, so that
// asking one agent runs no other.
func (g *Gateway) usable(ctx context.Context, id string) (Agent, error) {
	if found := g.cached(); found != nil {
		for _, av := range found {
			if av.ID == id && av.Probe.Usable() {
				return av.Agent, nil
			}
		}
		return nil, ErrUnavailable
	}
	if a, ok := g.Agent(id); ok && a.Probe(ctx).Usable() {
		return a, nil
	}
	return nil, ErrUnavailable
}

// begin records t as going, and its conversation as busy; named, for a
// run with Capy's tools, so that Capy mcp serves it as the agent.
func (g *Gateway) begin(t *Turn, conversation string, named bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case g.closed:
		return ErrClosed
	case conversation != "" && g.busy[conversation]:
		return ErrBusy
	}
	if g.o.RunsDir != "" && named {
		if err := writeRunFile(g.o.RunsDir, t.ID); err != nil {
			return err
		}
	}
	if conversation != "" {
		g.busy[conversation] = true
	}
	g.turns[t] = true
	g.running.Add(1)
	return nil
}

// end forgets t.
func (g *Gateway) end(t *Turn, conversation string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.o.RunsDir != "" {
		if err := os.Remove(filepath.Join(g.o.RunsDir, t.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			g.o.Log.Warn("agent run file", "err", errors.Unwrap(err))
		}
	}
	delete(g.busy, conversation)
	delete(g.turns, t)
	g.running.Done()
}

// writeRunFile names run id in dir for Capy mcp, 0600.
func writeRunFile(dir, id string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.New("gateway: cannot create the runs folder")
	}
	f, err := os.OpenFile(filepath.Join(dir, id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.New("gateway: cannot record the run")
	}
	return f.Close()
}

// RunGoing reports whether dir names run id: Capy mcp's check that the
// run it serves is one the gateway started and has not ended.
func RunGoing(dir, id string) bool {
	// Only the gateway's own form: on a case-insensitive disk another
	// would name the run's file but not its edits.
	if u, err := uuid.Parse(id); err != nil || u.String() != id {
		return false
	}
	fi, err := os.Lstat(filepath.Join(dir, id))
	return err == nil && fi.Mode().IsRegular()
}

// tools is how run runID's agent reaches Capy's tools.
func (g *Gateway) tools(runID, agentID string) Tools {
	var t Tools
	if g.o.Command != nil {
		t.Command = g.o.Command(runID)
	}
	if g.o.Server != nil {
		t.Open = func(ctx context.Context) (*mcp.ClientSession, error) {
			st, ct := mcp.NewInMemoryTransports()
			if _, err := g.o.Server(runID).Connect(ctx, st, nil); err != nil {
				return nil, err
			}
			return mcp.NewClient(&mcp.Implementation{Name: agentID}, nil).Connect(ctx, ct, nil)
		}
	}
	return t
}

// start runs q with a, logging it.
func (g *Gateway) start(a Agent, t *Turn, q Query) (Run, error) {
	run, err := a.Run(context.Background(), q)
	if err != nil {
		g.o.Log.Warn("agent run", "agent", a.Info().ID, "run", t.ID, "outcome", "not started")
		return nil, err
	}
	g.o.Log.Info("agent run", "agent", a.Info().ID, "run", t.ID, "resume", q.ConversationID != "")
	return run, nil
}

// stats are what a run's log says of it: never its content.
type stats struct {
	start                time.Time
	events, bytes, tools int
}

// supervise passes run's events on to t, applying the gateway's limits,
// Stop, the gate and the resume fallback, until the run ends.
func (g *Gateway) supervise(a Agent, t *Turn, q Query, run Run) {
	id := a.Info().ID
	st := stats{start: time.Now()}
	idle, total := time.NewTimer(g.o.Idle), time.NewTimer(g.o.Total)
	defer idle.Stop()
	defer total.Stop()
	gate := time.NewTicker(time.Second)
	defer gate.Stop()
	resumed := q.ConversationID != ""
	for {
		var stop Failure
		select {
		case e, ok := <-run.Events():
			if !ok {
				g.finish(id, t, q, &st, nil, Failed{Kind: Exited, Code: -1}) // an agent that ends with neither
				return
			}
			idle.Reset(g.o.Idle)
			if f, isFailed := e.(Failed); isFailed && f.Kind == ResumeRejected && resumed && q.Restart != "" {
				g.o.Log.Info("agent run", "agent", id, "run", t.ID, "outcome", f.Kind.String(), "fallback", true)
				resumed = false
				g.drain(run)
				next, err := a.Run(context.Background(), Query{Prompt: q.Restart, Model: q.Model, NoTools: q.NoTools, Tools: q.Tools})
				if err != nil {
					g.finish(id, t, q, &st, nil, f)
					return
				}
				run = next
				continue
			}
			g.count(id, t, &st, e)
			if _, tool := e.(ToolCall); tool && q.NoTools {
				// An agent that found a tool in a run given none, as a
				// later version may bring back: stopped, not trusted.
				run.Stop()
				g.finish(id, t, q, &st, run, Failed{Kind: Exited, Code: -1})
				return
			}
			if isEnd(e) {
				g.finish(id, t, q, &st, run, e)
				return
			}
			t.events <- e
			continue
		case <-idle.C:
			stop = Stalled
		case <-total.C:
			stop = Stalled
		case <-t.stop:
			stop = Stopped
		case <-gate.C:
			if g.o.Enabled() {
				continue
			}
			stop = Stopped
		}
		run.Stop()
		g.finish(id, t, q, &st, run, Failed{Kind: stop})
		return
	}
}

// finish ends t with its last event, end: once run's agent closed its
// events (nil for one that did), and the turn no longer counts as going,
// so that whoever sees the end may send the conversation's next question.
func (g *Gateway) finish(id string, t *Turn, q Query, st *stats, run Run, end Event) {
	if run != nil {
		g.drain(run)
	}
	g.end(t, q.ConversationID)
	g.log(id, t, st, end)
	t.events <- end
	close(t.events)
}

// drain reads a run's events until the agent closes them, so that its
// program is gone before the turn ends, but no longer than drainWait.
func (g *Gateway) drain(run Run) {
	deadline := time.After(drainWait)
	for {
		select {
		case _, ok := <-run.Events():
			if !ok {
				return
			}
		case <-deadline:
			return
		}
	}
}

func isEnd(e Event) bool {
	switch e.(type) {
	case Done, Failed:
		return true
	}
	return false
}

// toolName is the tool names logs keep; another is "other".
var toolName = regexp.MustCompile(`^[a-z_]{1,64}$`)

// count tallies e for the run's log, and logs a tool call by its name.
func (g *Gateway) count(id string, t *Turn, st *stats, e Event) {
	st.events++
	switch e := e.(type) {
	case TextDelta:
		st.bytes += len(e.Text)
	case Message:
		st.bytes += len(e.Text)
	case ToolCall:
		name := e.Name
		if !toolName.MatchString(name) {
			name = "other"
		}
		if !e.Done {
			st.tools++
		}
		g.o.Log.Info("agent tool call", "agent", id, "run", t.ID, "tool", name, "done", e.Done)
	}
}

// log logs the end of a run: its outcome, time, and counts.
func (g *Gateway) log(id string, t *Turn, st *stats, end Event) {
	outcome, code := "done", 0
	if f, ok := end.(Failed); ok {
		outcome, code = f.Kind.String(), f.Code
	}
	g.o.Log.Info("agent run ended", "agent", id, "run", t.ID, "outcome", outcome, "exit", code,
		"ms", time.Since(st.start).Milliseconds(), "events", st.events, "text_bytes", st.bytes, "tool_calls", st.tools)
}

// Allow records the user's consent to send questions to agent id.
func (g *Gateway) Allow(id string) error { return g.o.Consent.Allow(id) }

// Allowed reports whether agent id may be sent a query without asking
// the user: it sends nothing off this Mac, or the user allowed it.
func (g *Gateway) Allowed(id string) bool {
	a, ok := g.Agent(id)
	return ok && (a.Info().Sends == nil || g.o.Consent.Allowed(id))
}

// ContinueCommand is the command that continues conversation
// conversationID of agent id in the agent itself, with Capy's tools.
func (g *Gateway) ContinueCommand(id, conversationID string) ([]string, error) {
	a, ok := g.Agent(id)
	if !ok {
		return nil, ErrUnavailable
	}
	h, ok := a.(HandOff)
	if !ok {
		return nil, ErrUnavailable
	}
	var tools Tools
	if g.o.Command != nil {
		tools.Command = g.o.Command("")
	}
	return h.ContinueCommand(conversationID, tools)
}

// Close stops every run and waits for them to end, as Capy quits; Send
// is ErrClosed after it.
func (g *Gateway) Close() {
	g.mu.Lock()
	g.closed = true
	for t := range g.turns {
		t.Stop()
	}
	g.mu.Unlock()
	g.running.Wait()
}
