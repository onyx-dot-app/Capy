package ui

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/onyx-dot-app/Capy/internal/agent/ask"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/threads"
	"github.com/onyx-dot-app/Capy/internal/store"
)

// loadTimeout bounds a read of Agent's data or the library.
const loadTimeout = 10 * time.Second

// running is a question being answered in thread id, about session ("" for
// a cross-session thread), and how it is going.
type running struct {
	id, session, question, agent string
	st                           ask.State
}

// fetch runs get off the main thread and applies what it returns on the
// main thread: one read at a time per key, and one asked for meanwhile
// runs once that one ends, so that what is applied last is the newest.
func (p *panel) fetch(key string, get func(ctx context.Context) func()) {
	if p.loading[key] {
		p.again[key] = get
		return
	}
	p.loading[key] = true
	update := p.a.host.Update
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), loadTimeout)
		defer cancel()
		apply := get(ctx)
		update(func() {
			delete(p.loading, key)
			if apply != nil {
				apply()
			}
			if next, ok := p.again[key]; ok {
				delete(p.again, key)
				p.fetch(key, next)
			}
		})
	}()
}

// list reads the threads of key's scope, for History, and opens the
// newest of a session's when none is open. A failed read lists none, as
// it is not read again every frame.
func (p *panel) list(key string, sc scope, session string) {
	ak, log := p.a.o.Ask, p.a.o.Log
	p.fetch("list:"+key, func(ctx context.Context) func() {
		list, err := ak.Threads(ctx, session)
		if err != nil {
			log.Warn("ask: list threads", "err", err)
			return func() { p.threads[key] = nil }
		}
		if session == "" {
			list = slices.DeleteFunc(list, func(t threads.Thread) bool { return !sc.holds(t) })
		}
		return func() {
			p.threads[key] = list
			if _, chose := p.open[key]; !chose && session != "" && len(list) > 0 {
				p.open[key] = list[len(list)-1].ID
			}
		}
	})
}

// load reads thread id with the edits its messages name, and settles a
// run of it that ended, whether the read worked or not.
func (p *panel) load(id string) {
	ak, log := p.a.o.Ask, p.a.o.Log
	p.fetch("thread:"+id, func(ctx context.Context) func() {
		d, err := ak.Thread(ctx, id)
		if err != nil {
			log.Warn("ask: read thread", "err", err)
			return func() {
				p.details[id] = threads.Detail{Thread: threads.Thread{ID: id}}
				p.settled(id)
			}
		}
		edits := map[string]store.Edit{}
		for _, m := range d.Messages {
			if m.Role == threads.RoleEdit {
				if e, err := ak.Edit(ctx, m.Text); err == nil {
					edits[e.ID] = e
				}
			}
		}
		return func() {
			p.details[id] = d
			for eid, e := range edits {
				p.edits[eid] = e
			}
			p.settled(id)
		}
	})
}

// settled ends thread id's run once it ended and the thread shows its
// outcome, and sends the question waiting next.
func (p *panel) settled(id string) {
	r := p.runs[id]
	if r == nil || !r.st.Ended {
		return
	}
	delete(p.runs, id)
	delete(p.turnsOf, id)
	if slices.ContainsFunc(r.st.Used, func(c gateway.ToolCall) bool { return slices.Contains(writeTools, c.Name) }) {
		p.sessionsRead = false // titles may have changed
	}
	p.next(id)
}

// writeTools are the tools that edit the library.
var writeTools = []string{"rename_session", "update_recap", "delete_session"}

// loadTurns reads session id's turns, for its citations' tooltips.
func (p *panel) loadTurns(id string) {
	ak := p.a.o.Ask
	p.fetch("turns:"+id, func(ctx context.Context) func() {
		turns, err := ak.Turns(ctx, id)
		if err != nil {
			return func() { p.turns[id] = map[int64]string{} } // gone: no tooltips
		}
		byTime := make(map[int64]string, len(turns))
		for _, t := range turns {
			if k := t.StartMs / 1000 * 1000; byTime[k] == "" { // the first turn of a second
				byTime[k] = t.Opening
			}
		}
		return func() { p.turns[id] = byTime }
	})
}

// loadSessions reads the sessions whose titles a cross-session answer
// links; answers parsed before are parsed again.
func (p *panel) loadSessions() {
	ak, log := p.a.o.Ask, p.a.o.Log
	p.sessionsRead = true
	p.fetch("sessions", func(ctx context.Context) func() {
		list, err := ak.Sessions(ctx)
		if err != nil {
			log.Warn("ask: list sessions", "err", err)
			return nil
		}
		return func() { p.sessions, p.sessionsGen = list, p.sessionsGen+1 }
	})
}

// submit asks question in the open thread of the scope in view, or in a
// new one, and reports whether it took it: behind a run going in the
// thread, it waits its turn; while another question is being sent, a new
// thread's is refused, and stays in the field.
func (p *panel) submit(question string) bool {
	key := p.key()
	id := p.open[key]
	if p.live && id == "" && question == ask.CatchUpQuestion {
		return p.catchUp()
	}
	if id != "" && (p.runs[id] != nil || p.starting) {
		p.pending[id] = append(p.pending[id], question)
		return true
	}
	if p.starting {
		return false
	}
	p.notice = ""
	if id != "" {
		p.sendIn(id, question)
		return true
	}
	p.starting = true
	sc := p.askScope()
	ak, update := p.a.o.Ask, p.a.host.Update
	go func() {
		turn, err := ak.Ask(context.Background(), sc, question)
		update(func() { p.started(key, sc.SessionID, question, turn, err) })
	}()
	return true
}

// catchUp starts a thread about the live session with Catch me up: what
// was said since its panel was last open. It is submit's, which reports
// whether it took the question.
func (p *panel) catchUp() bool {
	if p.starting {
		return false
	}
	p.notice, p.starting = "", true
	key, sc := p.key(), p.askScope()
	ak, update := p.a.o.Ask, p.a.host.Update
	go func() {
		turn, err := ak.CatchUp(context.Background(), sc, "")
		update(func() { p.started(key, sc.SessionID, ask.CatchUpQuestion, turn, err) })
	}()
	return true
}

// sendIn asks question in thread id, which has no run going: Catch me up
// in the thread of the session being recorded catches up, however it came
// to be sent again (a Retry, Allow, the field after an error).
func (p *panel) sendIn(id, question string) {
	p.starting = true
	session := p.sessionOf(id)
	catchUp := false
	if live := p.a.host.LiveSession(); live != nil && question == ask.CatchUpQuestion && session == live.ID {
		catchUp = true
	}
	ak, update := p.a.o.Ask, p.a.host.Update
	go func() {
		var turn *ask.Turn
		var err error
		if catchUp {
			turn, err = ak.CatchUp(context.Background(), ask.Scope{SessionID: session}, id)
		} else {
			turn, err = ak.FollowUp(context.Background(), id, question)
		}
		update(func() { p.started("", session, question, turn, err) })
	}()
}

// sessionOf is the session of thread id: its own once read, else the
// session of its run.
func (p *panel) sessionOf(id string) string {
	if d, ok := p.details[id]; ok && d.ID != "" {
		return d.SessionID
	}
	if r := p.runs[id]; r != nil {
		return r.session
	}
	return ""
}

// shown is the surface that shows what a send came to: this one, or the
// inspector once the caption bar's panel closed under it.
func (p *panel) shown() *panel {
	if p.live && !p.a.liveOpen {
		return p.a.panel
	}
	return p
}

// next sends the oldest question waiting in thread id, once nothing goes
// in it.
func (p *panel) next(id string) {
	q := p.pending[id]
	if len(q) == 0 || p.runs[id] != nil || p.starting {
		return
	}
	if len(q) == 1 {
		delete(p.pending, id)
	} else {
		p.pending[id] = q[1:]
	}
	p.sendIn(id, q[0])
}

// started takes the outcome of a send: a run to follow, the consent to
// ask for, or why it was not sent, the question kept in the field. key is
// the scope a new thread opens in, "" for a follow-up, which opens
// nothing: the user may be in another scope by now.
func (p *panel) started(key, session, question string, turn *ask.Turn, err error) {
	p.starting = false
	defer func() { // what waited for this send may go now
		for id := range p.pending {
			p.next(id)
		}
	}()
	if err != nil {
		s := p.shown()
		var ce *ask.ConsentError
		switch {
		case errors.As(err, &ce):
			s.consent = consentAsk{open: true, agent: ce.Agent, question: question}
		case errors.Is(err, gateway.ErrUnavailable):
			s.notice = "No agent can answer now. Check Settings › Agents."
		case errors.Is(err, ask.ErrTooLong):
			s.notice = "The question is too long."
		case errors.Is(err, gateway.ErrClosed):
			return // Capy is quitting
		default:
			p.a.o.Log.Warn("ask: send", "err", err)
			s.notice = "Couldn’t ask. Try again."
		}
		if s.draft == "" {
			s.draft, s.caretEnd = question, true
		}
		return
	}
	id := turn.ThreadID()
	if key != "" {
		p.open[key] = id
		delete(p.threads, key) // History lists it once read again
	}
	delete(p.failures, id)
	delete(p.used, id)
	p.runs[id] = &running{id: id, session: session, question: question, agent: turn.Agent()}
	p.turnsOf[id] = turn
	p.follow(id, turn)
}

// follow hands turn's states to the main thread, then reads the thread
// anew once it ends. Host.Update runs each on the main thread before the
// next is read, so a fast stream costs a redraw per state the main
// thread draws.
func (p *panel) follow(id string, turn *ask.Turn) {
	update := p.a.host.Update
	go func() {
		for st := range turn.States() {
			update(func() {
				if r := p.runs[id]; r != nil {
					r.st = st
					p.used[id] = st.Used
					if st.Failure != nil {
						p.failures[id] = st.Failure
					}
				}
			})
		}
		update(func() { p.load(id) })
	}()
}

// stop stops the run of thread id.
func (p *panel) stop(id string) {
	if t := p.turnsOf[id]; t != nil {
		t.Stop()
	}
}

// allow records the consent the dialog asked for, and sends its question.
func (p *panel) allow() {
	c := p.consent
	p.consent = consentAsk{}
	if err := p.a.o.Gateway.Allow(c.agent); err != nil {
		p.a.o.Log.Warn("ask: allow agent", "err", err)
		p.notice = "Couldn’t save your choice. Try again."
		return
	}
	if p.submit(c.question) && p.draft == c.question {
		p.draft = ""
	}
}

// undo undoes an edit an agent made, from its chip.
func (p *panel) undo(id string) {
	ak, update := p.a.o.Ask, p.a.host.Update
	p.undoing[id] = true
	go func() {
		e, err := ak.Undo(context.Background(), id)
		update(func() {
			delete(p.undoing, id)
			switch {
			case errors.Is(err, store.ErrConflict):
				p.undone[id] = "Changed since; can’t undo"
			case err != nil:
				p.a.o.Log.Warn("ask: undo edit", "err", err)
				p.undone[id] = "Couldn’t undo"
			default:
				p.edits[id] = e
				p.sessionsRead = false
				p.a.host.LibraryChanged()
			}
		})
	}()
}

// continueIn opens thread id's conversation in its agent, in Terminal.
func (p *panel) continueIn(id string) {
	ak, update := p.a.o.Ask, p.a.host.Update
	go func() {
		err := ak.ContinueIn(context.Background(), id)
		update(func() {
			if err != nil {
				p.a.o.Log.Warn("ask: continue in", "err", err)
				p.notice = "Couldn’t open Terminal."
			}
		})
	}()
}

// deleteThread erases cross-session thread id, open in scope key.
func (p *panel) deleteThread(key, id string) {
	ak, update := p.a.o.Ask, p.a.host.Update
	go func() {
		err := ak.DeleteThread(context.Background(), id)
		update(func() {
			if err != nil {
				p.a.o.Log.Warn("ask: delete thread", "err", err)
				p.notice = "Couldn’t delete the thread."
				return
			}
			if p.open[key] == id {
				p.open[key] = ""
			}
			delete(p.threads, key)
			delete(p.details, id)
			delete(p.failures, id)
			delete(p.used, id)
			delete(p.pending, id)
		})
	}()
}
