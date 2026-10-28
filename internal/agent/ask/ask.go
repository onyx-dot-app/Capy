// Package ask is Ask, Agent's question answering: it turns a question
// about Capy's recordings into a thread, sends it through the gateway to
// the user's agent, follows the run, and keeps the question, the answer
// and the edits the agent made. It knows no agent: it names them by
// gateway ID.
package ask

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/threads"
	"github.com/onyx-dot-app/Capy/internal/store"
)

// Capability is Ask's name in the thread store.
const Capability = "ask"

// MaxQuestion is the longest question, in bytes.
const MaxQuestion = 32 << 10

// Errors of Ask and FollowUp, besides the gateway's.
var (
	// ErrEmpty is a blank question.
	ErrEmpty = errors.New("ask: the question is empty")
	// ErrTooLong is a question over MaxQuestion.
	ErrTooLong = errors.New("ask: the question is too long")
)

// ConsentError is a question for an agent the user has not allowed yet:
// the user's Allow (gateway.Allow) of Agent, then Ask again. It is
// gateway.ErrConsentNeeded.
type ConsentError struct{ Agent string }

func (e *ConsentError) Error() string { return "ask: " + e.Agent + " needs the user's consent" }
func (e *ConsentError) Unwrap() error { return gateway.ErrConsentNeeded }

// sendError is err of a send to agent, which names the agent when it
// needs consent.
func sendError(agent string, err error) error {
	if errors.Is(err, gateway.ErrConsentNeeded) {
		return &ConsentError{Agent: agent}
	}
	return err
}

// Options are what Ask depends on.
type Options struct {
	Gateway *gateway.Gateway
	Threads threads.Store
	Library store.Store
	// Agent is the agent of a new thread: the user's choice, by gateway
	// ID.
	Agent func() string
	// Model is the model the user chose for agent id, "" for its default.
	Model func(id string) string
	// ContinueDir is where a conversation continued in Terminal runs, and
	// its script is written: never the folder of Capy's own runs, which
	// the user's full agent could leave instructions in.
	ContinueDir string
	// Open opens the Continue in… script in Terminal; nil runs open -a
	// Terminal.
	Open func(path string) error
	Log  *slog.Logger
}

// Ask asks the user's agents about Capy's recordings. It is safe for
// concurrent use.
type Ask struct {
	o       Options
	mu      sync.Mutex
	busy    map[string]bool      // threads with a run going
	seen    map[string]time.Time // when each live session's panel last closed
	closed  bool
	running sync.WaitGroup // the runs begun, until their outcome is saved
}

// New returns Ask.
func New(o Options) *Ask {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Model == nil {
		o.Model = func(string) string { return "" }
	}
	if o.Open == nil {
		o.Open = openInTerminal
	}
	return &Ask{o: o, busy: map[string]bool{}}
}

// begin records a run of thread id: gateway.ErrBusy while the thread has
// one, as a follow-up must wait for the answer before it, and
// gateway.ErrClosed as Capy quits. Each begin is ended by finish.
func (a *Ask) begin(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.closed:
		return gateway.ErrClosed
	case a.busy[id]:
		return gateway.ErrBusy
	}
	a.busy[id] = true
	a.running.Add(1)
	return nil
}

func (a *Ask) finish(id string) {
	a.mu.Lock()
	delete(a.busy, id)
	a.mu.Unlock()
	a.running.Done()
}

// Scope is what a new thread asks about: one session, a range of days,
// a batch of imported files, or every session.
type Scope struct {
	// SessionID and Title are the session's.
	SessionID, Title string
	// From and To bound the sessions of a range, both days included; both
	// zero for every session.
	From, To time.Time
	// Batch, BatchName and SessionIDs are a batch's: its ID, its name and
	// its sessions, in order.
	Batch, BatchName string
	SessionIDs       []string
}

func (s Scope) store() string {
	switch {
	case s.SessionID != "":
		return threads.ScopeSession
	case s.Batch != "":
		return threads.BatchScope(s.Batch)
	case !s.From.IsZero() || !s.To.IsZero():
		return threads.RangeScope(s.From, s.To)
	}
	return threads.ScopeAll
}

// State is a question's run as Ask's screens show it. Every change is a
// new State.
type State struct {
	ThreadID string
	Question string
	// Answer is the answer so far.
	Answer string
	// Tool is the call going, nil for none; Used are the calls done.
	Tool *gateway.ToolCall
	Used []gateway.ToolCall
	// Edits are the edits the agent made in the run, oldest first.
	Edits []store.Edit
	// Ended is set on the run's last State; Failure then says how it
	// failed, nil when it answered.
	Ended   bool
	Failure *gateway.Failed
}

// Turn is a question being answered.
type Turn struct {
	states chan State // the latest State not read yet
	run    *gateway.Turn
	thread string
	agent  string
}

// ThreadID is the thread the question is in.
func (t *Turn) ThreadID() string { return t.thread }

// Agent is the gateway ID of the agent answering it.
func (t *Turn) Agent() string { return t.agent }

// States reports the run, and is closed after the State that Ended it. A
// reader that falls behind gets the latest State, never a stale one: the
// run never waits for it.
func (t *Turn) States() <-chan State { return t.states }

// put makes st the State to read next, in place of one not read yet.
func (t *Turn) put(st State) {
	for {
		select {
		case t.states <- st:
			return
		default:
			select {
			case <-t.states:
			default:
			}
		}
	}
}

// Stop stops the run; the answer so far is kept as incomplete.
func (t *Turn) Stop() { t.run.Stop() }

// Ask starts a thread about scope with question, with the user's agent.
// It saves nothing when the gateway refuses the question, as for
// gateway.ErrConsentNeeded: the user's Allow then Ask again.
func (a *Ask) Ask(ctx context.Context, scope Scope, question string) (*Turn, error) {
	return a.ask(ctx, scope, question, "")
}

// ask is Ask, with hint added to the prompt after the question, never
// saved with it.
func (a *Ask) ask(ctx context.Context, scope Scope, question, hint string) (*Turn, error) {
	question, err := clean(question)
	if err != nil {
		return nil, err
	}
	agent := a.o.Agent()
	if agent == "" {
		return nil, gateway.ErrUnavailable
	}
	th := threads.Thread{ID: uuid.NewString(), Capability: Capability, SessionID: scope.SessionID,
		Scope: scope.store(), Agent: agent, Title: title(question)}
	if err := th.Validate(); err != nil {
		return nil, err
	}
	if err := a.begin(th.ID); err != nil {
		return nil, err
	}
	run, err := a.o.Gateway.Send(ctx, agent, gateway.Query{Prompt: withHint(firstPrompt(scope, question), hint), Model: a.o.Model(agent)})
	if err != nil {
		a.finish(th.ID)
		return nil, sendError(agent, err)
	}
	if th, err = a.o.Threads.CreateThread(ctx, th); err != nil {
		return nil, a.abandon(th.ID, run, err)
	}
	return a.follow(ctx, th, question, run)
}

// FollowUp asks question in thread id with the agent the user chooses now:
// the thread's own resumes its conversation; another starts one from what
// the thread holds, and the thread is its from then on.
func (a *Ask) FollowUp(ctx context.Context, id, question string) (*Turn, error) {
	return a.followUp(ctx, id, question, "")
}

// followUp is FollowUp, with hint added to the prompt after the question.
func (a *Ask) followUp(ctx context.Context, id, question, hint string) (*Turn, error) {
	question, err := clean(question)
	if err != nil {
		return nil, err
	}
	if err := a.begin(id); err != nil {
		return nil, err
	}
	d, err := a.Thread(ctx, id)
	if err != nil {
		a.finish(id)
		return nil, err
	}
	scope, err := a.scopeOf(ctx, d.Thread)
	if err != nil {
		a.finish(id)
		return nil, err
	}
	th := d.Thread
	agent := a.o.Agent()
	if agent == "" {
		a.finish(id)
		return nil, gateway.ErrUnavailable
	}
	if agent != th.Agent { // another agent cannot resume this one's conversation
		th.Agent, th.ConversationID = agent, ""
	}
	q := gateway.Query{Model: a.o.Model(agent), ConversationID: th.ConversationID}
	if th.ConversationID == "" { // none to resume: start anew
		q.Prompt = withHint(restartPrompt(scope, d.Messages, question), hint)
	} else {
		q.Prompt, q.Restart = withHint(question, hint), withHint(restartPrompt(scope, d.Messages, question), hint)
	}
	run, err := a.o.Gateway.Send(ctx, agent, q)
	if err != nil {
		a.finish(id)
		return nil, sendError(agent, err)
	}
	if th.Agent != d.Agent {
		if err := a.o.Threads.SetAgent(ctx, th.ID, agent); err != nil {
			return nil, a.abandon(th.ID, run, err)
		}
	}
	return a.follow(ctx, th, question, run)
}

// abandon stops run of thread id, which a failure to save its thread or
// question left without one.
func (a *Ask) abandon(id string, run *gateway.Turn, err error) error {
	run.Stop()
	for range run.Events() {
	}
	a.finish(id)
	return err
}

// clean is question without surrounding space, checked.
func clean(question string) (string, error) {
	question = strings.TrimSpace(question)
	switch {
	case question == "":
		return "", ErrEmpty
	case len(question) > MaxQuestion:
		return "", ErrTooLong
	case !utf8.ValidString(question):
		return "", ErrEmpty
	}
	return question, nil
}

// title is a thread's title: its first question, on one line, cut to the
// store's bound.
func title(question string) string {
	t := strings.Join(strings.Fields(question), " ")
	for len(t) > threads.MaxTitleLen {
		_, n := utf8.DecodeLastRuneInString(t)
		t = t[:len(t)-n]
	}
	return t
}

// scopeOf is a thread's scope, its session's title read from the library.
func (a *Ask) scopeOf(ctx context.Context, th threads.Thread) (Scope, error) {
	if th.SessionID != "" {
		s, err := a.o.Library.GetSession(ctx, th.SessionID)
		if err != nil {
			return Scope{}, err
		}
		return Scope{SessionID: s.ID, Title: s.Title}, nil
	}
	if from, to, ok := threads.ParseRange(th.Scope); ok {
		return Scope{From: from.Local(), To: to.Local()}, nil
	}
	if id, ok := threads.ParseBatch(th.Scope); ok {
		b, err := a.o.Library.GetBatch(ctx, id)
		if errors.Is(err, store.ErrNotFound) { // ungrouped or deleted since
			return Scope{Batch: id}, nil
		}
		if err != nil {
			return Scope{}, err
		}
		return BatchScope(b), nil
	}
	return Scope{}, nil
}

// BatchScope is the scope of batch b: its saved files' sessions.
func BatchScope(b store.BatchSummary) Scope {
	sc := Scope{Batch: b.ID, BatchName: b.Name}
	for _, f := range b.Files {
		if f.SessionID != "" {
			sc.SessionIDs = append(sc.SessionIDs, f.SessionID)
		}
	}
	return sc
}

// writeTools are the tools that edit the library.
var writeTools = []string{"rename_session", "update_recap", "delete_session"}

// follow saves question in th, then follows run to its end in the
// background, saving its answer and edits.
func (a *Ask) follow(ctx context.Context, th threads.Thread, question string, run *gateway.Turn) (*Turn, error) {
	user, err := a.o.Threads.AppendMessage(ctx, threads.Message{ID: uuid.NewString(), ThreadID: th.ID,
		Role: threads.RoleUser, Text: question, Status: threads.StatusUnanswered})
	if err != nil {
		return nil, a.abandon(th.ID, run, err)
	}
	t := &Turn{states: make(chan State, 1), run: run, thread: th.ID, agent: th.Agent}
	go a.watch(context.WithoutCancel(ctx), th, user, run, t)
	return t, nil
}

// watch passes run's events on to t as States, and saves the outcome:
// the answer, done or incomplete, each edit, and the question answered or
// not.
func (a *Ask) watch(ctx context.Context, th threads.Thread, user threads.Message, run *gateway.Turn, t *Turn) {
	defer close(t.states)
	st := State{ThreadID: th.ID, Question: user.Text}
	var said []string // whole messages
	var partial strings.Builder
	conversation := th.ConversationID
	handle := func(e gateway.Event) {
		switch e := e.(type) {
		case gateway.Started:
			if e.ConversationID != conversation && threads.ValidateConversation(e.ConversationID) == nil {
				conversation = e.ConversationID
				if err := a.o.Threads.SetConversation(ctx, th.ID, conversation); err != nil {
					a.o.Log.Warn("ask: save conversation", "err", err)
				}
			}
		case gateway.TextDelta:
			partial.WriteString(e.Text)
		case gateway.Message:
			said = append(said, e.Text)
			partial.Reset()
		case gateway.ToolCall:
			if !e.Done {
				st.Tool = &e
				break
			}
			st.Tool, st.Used = nil, append(slices.Clone(st.Used), e)
			if slices.Contains(writeTools, e.Name) {
				st.Edits = a.edits(ctx, run.ID)
			}
		case gateway.Done:
			st.Ended = true
		case gateway.Failed:
			st.Ended, st.Failure = true, &e
		}
	}
	events := run.Events()
	saved := false
	for e := range events {
		handle(e)
		// Every event waiting goes into one State: the answer is built
		// once for them all, not once a word.
		for more := true; more && !st.Ended; {
			select {
			case e, ok := <-events:
				if more = ok; ok {
					handle(e)
				}
			default:
				more = false
			}
		}
		st.Answer = strings.TrimSpace(strings.Join(append(slices.Clone(said), partial.String()), "\n\n"))
		if len(st.Answer) > threads.MaxTextLen && !st.Ended {
			// Past what a message holds: what fits is kept, cut short.
			run.Stop()
		}
		st.Answer = cut(st.Answer, threads.MaxTextLen)
		if st.Ended && !saved {
			// A run that finds no edits shows none, whatever the agent
			// says; one that made some shows them all.
			st.Edits = a.edits(ctx, run.ID)
			a.save(ctx, th, user, st)
			saved = true
			a.finish(th.ID) // saved: a follow-up may go once this is seen
		}
		t.put(st)
	}
	if !saved { // the gateway ends every run; never leave the thread busy
		a.finish(th.ID)
	}
}

// Close refuses new questions, then waits until every run begun has ended
// and its outcome is saved: as Capy quits, after the gateway stopped the
// runs.
func (a *Ask) Close() {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
	a.running.Wait()
}

// cut is s cut to at most n bytes, on a rune boundary.
func cut(s string, n int) string {
	for len(s) > n {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// edits are the edits made in run id.
func (a *Ask) edits(ctx context.Context, id string) []store.Edit {
	edits, err := a.o.Library.ListEdits(ctx, id)
	if err != nil {
		a.o.Log.Warn("ask: list edits", "err", err)
	}
	return edits
}

// save keeps the end of a run: the answer and the edits after the
// question, which is answered once any answer came.
func (a *Ask) save(ctx context.Context, th threads.Thread, user threads.Message, st State) {
	if st.Answer != "" {
		status := threads.StatusDone
		if st.Failure != nil {
			status = threads.StatusIncomplete
		}
		if _, err := a.o.Threads.AppendMessage(ctx, threads.Message{ID: uuid.NewString(), ThreadID: th.ID,
			Role: threads.RoleAgent, Text: st.Answer, Status: status}); err != nil {
			a.o.Log.Warn("ask: save answer", "err", err)
			st.Answer = "" // the question stays unanswered, not answered with nothing
		}
	}
	for _, e := range st.Edits {
		if _, err := a.o.Threads.AppendMessage(ctx, threads.Message{ID: uuid.NewString(), ThreadID: th.ID,
			Role: threads.RoleEdit, Text: e.ID}); err != nil {
			a.o.Log.Warn("ask: save edit", "err", err)
		}
	}
	if st.Answer != "" || st.Failure == nil {
		if err := a.o.Threads.SetMessageStatus(ctx, user.ID, threads.StatusDone); err != nil {
			a.o.Log.Warn("ask: save question", "err", err)
		}
	}
}

// Threads are the Ask threads of session id, or the cross-session ones
// for "", oldest first. A trashed session's are hidden with it.
func (a *Ask) Threads(ctx context.Context, sessionID string) ([]threads.Thread, error) {
	if sessionID != "" {
		if _, err := a.o.Library.GetSession(ctx, sessionID); errors.Is(err, store.ErrNotFound) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
	}
	return a.o.Threads.ListThreads(ctx, threads.ListQuery{SessionID: sessionID, Capability: Capability})
}

// Thread is Ask thread id with its messages; threads.ErrNotFound for one
// that is not Ask's, or whose session is trashed.
func (a *Ask) Thread(ctx context.Context, id string) (threads.Detail, error) {
	d, err := a.o.Threads.GetThread(ctx, id)
	if err != nil {
		return threads.Detail{}, err
	}
	if d.Capability != Capability {
		return threads.Detail{}, threads.ErrNotFound
	}
	if d.SessionID != "" {
		if _, err := a.o.Library.GetSession(ctx, d.SessionID); errors.Is(err, store.ErrNotFound) {
			return threads.Detail{}, threads.ErrNotFound
		} else if err != nil {
			return threads.Detail{}, err
		}
	}
	return d, nil
}

// Undo undoes an edit an agent made: store.ErrConflict when the field
// changed since.
func (a *Ask) Undo(ctx context.Context, editID string) (store.Edit, error) {
	return a.o.Library.UndoEdit(ctx, editID)
}
