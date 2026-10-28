// Package polish is Highlight polish: the user's own agent, through the
// gateway, writes each highlight of a session as a sentence with its
// fields, corrects its kind, folds restatements and records debates. A
// pass sends the agent everything it needs in its prompt and gives it no
// tools; Capy reads the ops of its answer, keeps those its guards pass,
// and saves them on the highlights.
package polish

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/profile"
	"github.com/onyx-dot-app/Capy/internal/store"
)

// LiveGap is the least time between the starts of a session's live
// passes.
const LiveGap = 60 * time.Second

// maxFailed is how many passes in a row may fail before polish stops for
// the session.
const maxFailed = 3

// Options are what Polish needs.
type Options struct {
	Store   store.Store
	Gateway *gateway.Gateway
	// Agent is the ID of the agent chosen in Settings › Agents, "" for
	// none; Model the model to ask it with, "" for its default.
	Agent func() string
	Model func(agentID string) string
	// On reports whether polish is on: its switch, the features, the
	// user's consent.
	On      func() bool
	Profile func() profile.Profile
	// Saved reports that a pass saved polish of a session; Stopped that
	// polish stopped for one. Both are called on the pass's goroutine.
	Saved   func(sessionID string)
	Stopped func(Stop)
	// Gap is LiveGap, unless a test sets it; Now is time.Now.
	Gap time.Duration
	Now func() time.Time
	Log *slog.Logger
}

// Reason is why polish stopped for a session.
type Reason int

const (
	// SignedOut: the agent needs the user to sign in.
	SignedOut Reason = iota
	// UsageLimit: the user's plan allows no more for now.
	UsageLimit
	// Failing: passes kept failing.
	Failing
	// Unavailable: the chosen agent is not found, or none is chosen.
	Unavailable
)

// Stop is polish stopped for a session: no more passes until Retry.
type Stop struct {
	SessionID string
	Agent     string // the agent's ID, "" for none
	Reason    Reason
	ResetAt   time.Time // when a usage limit resets, when the agent says
}

// Polish runs the passes of every session, one at a time each.
type Polish struct {
	o      Options
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	closed   bool
	sessions map[string]*state
	lastStop *Stop
}

// state is a session's passes: those asked for, and how the last went.
type state struct {
	live, final bool // passes asked for
	driving     bool // a goroutine runs them
	last        time.Time
	failed      int
	stopped     bool
	wake        chan struct{}
	// halt cancels the pass going, nil while none is.
	halt context.CancelFunc
}

// errOff ends a pass when polish was turned off while it went.
var errOff = errors.New("polish: turned off")

// New returns Polish, running no pass until Notify.
func New(o Options) *Polish {
	if o.Gap == 0 {
		o.Gap = LiveGap
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Model == nil {
		o.Model = func(string) string { return "" }
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Polish{o: o, ctx: ctx, cancel: cancel, sessions: map[string]*state{}}
}

// Notify asks for a pass of session id: a live one, after Highlights
// saved highlights while it records; the final one, after its last check
// or Find Highlights. It never blocks.
func (p *Polish) Notify(id string, final bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	st := p.sessions[id]
	if st == nil {
		st = &state{wake: make(chan struct{}, 1)}
		p.sessions[id] = st
	}
	if st.stopped {
		return
	}
	if final {
		st.final = true
	} else {
		st.live = true
	}
	p.drive(id, st)
}

// Retry polishes session id again after it stopped: its final pass.
func (p *Polish) Retry(id string) {
	p.mu.Lock()
	if st := p.sessions[id]; st != nil {
		st.stopped, st.failed = false, 0
	}
	if p.lastStop != nil && p.lastStop.SessionID == id {
		p.lastStop = nil
	}
	p.mu.Unlock()
	p.Notify(id, true)
}

// LastStop is the last session polish stopped for and why, until Retry.
func (p *Polish) LastStop() (Stop, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastStop == nil {
		return Stop{}, false
	}
	return *p.lastStop, true
}

// Wait returns once no pass of any session is going or due. Call it only
// once nothing will Notify any more, as a replay does after its session
// ended.
func (p *Polish) Wait() { p.wg.Wait() }

// Close stops the passes going and waits for them.
func (p *Polish) Close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.cancel()
	p.wg.Wait()
}

// drive runs st's passes on a goroutine of their own, or wakes the one
// that does. Call with mu held.
func (p *Polish) drive(id string, st *state) {
	if st.driving {
		select {
		case st.wake <- struct{}{}:
		default:
		}
		return
	}
	st.driving = true
	p.wg.Add(1)
	go p.run(id, st)
}

// run makes st's passes as they come due, until none is asked for.
func (p *Polish) run(id string, st *state) {
	defer p.wg.Done()
	for {
		p.mu.Lock()
		if p.closed || st.stopped || (!st.live && !st.final) {
			st.driving = false
			p.mu.Unlock()
			return
		}
		final := st.final
		if wait := st.last.Add(p.o.Gap).Sub(p.o.Now()); !final && wait > 0 {
			p.mu.Unlock()
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-st.wake:
			case <-p.ctx.Done():
			}
			timer.Stop()
			continue
		}
		st.live, st.final, st.last = false, false, p.o.Now()
		ctx, halt := context.WithCancel(p.ctx)
		st.halt = halt
		p.mu.Unlock()
		err := errOff
		if p.o.On() {
			err = p.pass(ctx, id, final)
		}
		p.mu.Lock()
		st.halt = nil
		p.mu.Unlock()
		halt()
		p.settle(id, st, err)
	}
}

// Halt stops the passes going, as turning polish off does: what they
// sent no more goes to the agent. A pass asked for later runs, if polish
// is on by then.
func (p *Polish) Halt() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, st := range p.sessions {
		if st.halt != nil {
			st.halt()
		}
	}
}

// settle records how a pass of session id went, stopping polish for it
// as its failure says.
func (p *Polish) settle(id string, st *state, err error) {
	var f failure
	var stop *Stop
	p.mu.Lock()
	switch {
	case err == nil:
		st.failed = 0
	case p.ctx.Err() != nil, errors.Is(err, errOff), errors.Is(err, context.Canceled), errors.As(err, &f) && f.Kind == gateway.Stopped:
	case errors.Is(err, gateway.ErrUnavailable), errors.Is(err, gateway.ErrConsentNeeded):
		stop = &Stop{Reason: Unavailable}
	case errors.As(err, &f) && f.Kind == gateway.SignedOut:
		stop = &Stop{Reason: SignedOut}
	case errors.As(err, &f) && f.Kind == gateway.UsageLimit:
		stop = &Stop{Reason: UsageLimit, ResetAt: f.ResetAt}
	default:
		if st.failed++; st.failed >= maxFailed {
			stop = &Stop{Reason: Failing}
		}
	}
	if stop != nil {
		stop.SessionID, stop.Agent = id, p.o.Agent()
		st.stopped, st.live, st.final = true, false, false
		p.lastStop = stop
	}
	p.mu.Unlock()
	if stop != nil && p.o.Stopped != nil {
		p.o.Stopped(*stop)
	}
}

// failure is a run of the agent that did not answer in full.
type failure struct{ gateway.Failed }

func (f failure) Error() string { return "polish: the agent's run failed: " + f.Kind.String() }

// pass polishes session id's highlights: those not polished yet, or all
// of them for the final pass, a prompt at a time, each saved as its
// answer comes.
func (p *Polish) pass(ctx context.Context, id string, final bool) (err error) {
	agentID := p.o.Agent()
	a, ok := p.o.Gateway.Agent(agentID)
	if agentID == "" || !ok {
		return gateway.ErrUnavailable
	}
	started := p.o.Now()
	d, err := p.o.Store.GetSession(ctx, id)
	if err != nil {
		return err
	}
	in := Input{Session: d, Profile: p.o.Profile(), Final: final}
	batches := prompts(in)
	kept, dropped := map[string]int{}, map[string]int{}
	bad, sent, read := 0, 0, 0
	defer func() {
		// Counts, sizes and errors, whose text is Capy's own: never a
		// prompt's or an answer's.
		p.o.Log.Info("polish pass", "session", id, "agent", agentID, "final", final, "prompts", len(batches),
			"took", p.o.Now().Sub(started).Round(time.Millisecond), "sent", sent, "read", read,
			"kept", kept, "dropped", dropped, "unparsed", bad, "err", err)
	}()
	for i, b := range batches {
		if !p.o.On() || ctx.Err() != nil {
			return errOff // turned off: nothing more goes to the agent
		}
		sent += len(b.prompt)
		answer, err := p.ask(ctx, agentID, b.prompt)
		read += len(answer)
		if err != nil {
			return err
		}
		ops, n, err := parse(answer)
		if err != nil {
			return err
		}
		bad += n
		pol, out := apply(ops, in, b, a.Info().Name, p.o.Now())
		for k, v := range out.kept {
			kept[k] += v
		}
		for k, v := range out.dropped {
			dropped[k] += v
		}
		if len(pol.Polish)+len(pol.Debates) > 0 {
			if err := p.o.Store.PolishHighlights(context.WithoutCancel(ctx), pol); err != nil {
				return err
			}
			if p.o.Saved != nil {
				p.o.Saved(id)
			}
		}
		if i+1 < len(batches) {
			// The next prompt's guards read what this one saved, and what
			// the user dismissed meanwhile.
			if in.Session, err = p.o.Store.GetSession(ctx, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// ask runs prompt with the agent of agentID, with no tools, and returns
// its answer: every message of the run.
func (p *Polish) ask(ctx context.Context, agentID, prompt string) (string, error) {
	turn, err := p.o.Gateway.Send(ctx, agentID, gateway.Query{Prompt: prompt, Model: p.o.Model(agentID), NoTools: true})
	if err != nil {
		return "", err
	}
	stop := context.AfterFunc(ctx, turn.Stop)
	defer stop()
	var answer []byte
	for e := range turn.Events() {
		switch e := e.(type) {
		case gateway.Message:
			if len(answer) <= maxAnswer {
				answer = append(append(answer, e.Text...), '\n')
			}
		case gateway.Failed:
			return "", failure{e}
		}
	}
	return string(answer), nil
}
