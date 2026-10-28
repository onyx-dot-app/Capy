package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/onyx-dot-app/Capy/internal/agent"
	"github.com/onyx-dot-app/Capy/internal/agent/ask"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/prefs"
	"github.com/onyx-dot-app/Capy/internal/agent/threads"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
)

// askTimeout bounds the replay's question, past the gateway's own limits.
const askTimeout = 3 * time.Minute

// askOptions are the replay's question: asked after its last turn, or
// after turn At while it records.
type askOptions struct {
	question  string
	at        int
	agent     string // a gateway ID
	agentPath string // the agent's program, as Settings' Choose… keeps it
}

// reportAsk is what the replay's question did, without IDs or times.
type reportAsk struct {
	Agent    string          `json:"agent"`
	Question string          `json:"question"`
	Live     bool            `json:"live"`
	Tools    []string        `json:"tools"`
	Answer   string          `json:"answer"`
	Outcome  string          `json:"outcome"`
	Thread   reportThread    `json:"thread"`
	Edits    []reportEdit    `json:"edits"`
	checks   []reportCheck   // joined to the report's
	cited    map[string]bool // the turns' times, for the citation check
}

type reportThread struct {
	Scope        string          `json:"scope"`
	Agent        string          `json:"agent"`
	Conversation bool            `json:"conversation"`
	Messages     []reportMessage `json:"messages"`
}

type reportMessage struct {
	Role   string `json:"role"`
	Status string `json:"status"`
	Text   string `json:"text"` // "" for an edit, whose text is its ID
}

type reportEdit struct {
	Field  string `json:"field"`
	Old    string `json:"old"`
	New    string `json:"new"`
	Undone bool   `json:"undone"`
}

// capyTools are the tools of Capy mcp: an in-app run sees no other.
var capyTools = []string{"list_sessions", "search", "get_session", "get_live_session", "get_profile",
	"rename_session", "update_recap", "delete_session"}

// newAgent wires Agent on the replay's library: in dir, beside capy.db,
// with the agent and its program as Settings would keep them, and the
// user's consent, which -ask gives.
func newAgent(ctx context.Context, o askOptions, polish bool, db store.Store, dir string, log *slog.Logger) (*agent.Module, error) {
	if err := os.MkdirAll(filepath.Join(dir, "agent"), 0o700); err != nil {
		return nil, err
	}
	if err := prefs.Load(filepath.Join(dir, "agent", "agent.json")).Update(func(p *prefs.Prefs) {
		p.Agent, p.Polish = o.agent, polish
		if o.agentPath != "" {
			p.Paths = map[string]string{o.agent: o.agentPath}
		}
	}); err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	m, err := agent.New(ctx, agent.Deps{Store: db, Gate: gate(dir, log), Dir: dir, Self: self, Log: log})
	if err != nil {
		return nil, err
	}
	if o.agent != "" {
		if err := m.Allow(o.agent); err != nil {
			m.Close()
			return nil, err
		}
	}
	return m, nil
}

// askAt waits until run has stored at turns, then asks; it returns when
// the answer ended.
func askAt(ctx context.Context, m *agent.Module, db store.Store, o askOptions, run *session.Run, changed <-chan struct{}) (*reportAsk, error) {
	for spoken(run.Snapshot()) < o.at {
		select {
		case <-changed:
		case <-run.Done():
			if spoken(run.Snapshot()) < o.at { // the last turns may come as it ends
				return nil, fmt.Errorf("the replay ended before turn %d", o.at)
			}
		}
	}
	snap := run.Snapshot()
	return askNow(ctx, m, db, o, snap.SessionID, snap.Status == session.StatusLive)
}

// spoken is how many turns s has stored, gaps aside.
func spoken(s session.Snapshot) int {
	n := 0
	for _, l := range s.Lines {
		if !l.Gap {
			n++
		}
	}
	return n
}

// askNow asks about session id and follows the answer to its end.
func askNow(ctx context.Context, m *agent.Module, db store.Store, o askOptions, id string, live bool) (*reportAsk, error) {
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	r := &reportAsk{Question: o.question, Live: live, Tools: []string{}, Edits: []reportEdit{}}
	turn, err := m.Ask().Ask(ctx, ask.Scope{SessionID: id}, o.question)
	if err != nil {
		return nil, err
	}
	var last ask.State
	for done := false; !done; {
		select {
		case st, ok := <-turn.States():
			if done = !ok; ok {
				last = st
			}
		case <-ctx.Done():
			turn.Stop() // the states then end with the run
			ctx = context.Background()
		}
	}
	for _, c := range last.Used {
		r.Tools = append(r.Tools, c.Name)
	}
	r.Answer, r.Outcome = last.Answer, "done"
	if last.Failure != nil {
		r.Outcome = last.Failure.Kind.String()
	}
	// Restore edits before reading: a deleted session hides its thread
	// and transcript until its trash edit is undone.
	r.Edits = undoAskEdits(ctx, m, last.Edits)
	d, err := m.Ask().Thread(ctx, last.ThreadID)
	if err != nil {
		return nil, err
	}
	r.Agent = d.Agent
	r.Thread = reportThread{Scope: d.Scope, Agent: d.Agent, Conversation: d.ConversationID != "", Messages: []reportMessage{}}
	for _, msg := range d.Messages {
		rm := reportMessage{Role: string(msg.Role), Status: string(msg.Status)}
		if msg.Role != threads.RoleEdit {
			rm.Text = msg.Text
		}
		r.Thread.Messages = append(r.Thread.Messages, rm)
	}
	// The turns stored as the answer ended: all it may cite.
	sd, err := db.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	r.cited = turnTimes(sd)
	r.checks = checkAsk(r, d)
	return r, nil
}

// citation is a [mm:ss] or [h:mm:ss] of an answer.
var citation = regexp.MustCompile(`\[(\d+:)?\d{2}:\d{2}\]`)

// checkAsk is the verdicts on an answer: checks, not model text, so that
// they hold for a real agent too. Undoing each edit is one of them.
func checkAsk(r *reportAsk, d threads.Detail) []reportCheck {
	var checks []reportCheck
	check := func(name string, pass bool, detail string) {
		checks = append(checks, reportCheck{Name: name, Pass: pass, Detail: detail})
	}
	check("answered", r.Outcome == "done" && r.Answer != "", r.Outcome)
	check("read the session", slices.Contains(r.Tools, "get_session") || slices.Contains(r.Tools, "get_live_session"),
		fmt.Sprintf("%d tool calls", len(r.Tools)))
	others := slices.DeleteFunc(slices.Clone(r.Tools), func(t string) bool { return slices.Contains(capyTools, t) })
	check("only Capy's tools", len(others) == 0, fmt.Sprintf("%d others", len(others)))
	cites := citation.FindAllString(r.Answer, -1)
	unknown := slices.DeleteFunc(slices.Clone(cites), func(c string) bool { return r.cited[c] })
	check("citations are stored turns", len(cites) > 0 && len(unknown) == 0,
		fmt.Sprintf("%d cited, %d unknown", len(cites), len(unknown)))
	saved := len(d.Messages) >= 2 && d.Messages[0].Role == threads.RoleUser && d.Messages[0].Status == threads.StatusDone &&
		d.Messages[1].Role == threads.RoleAgent && d.Messages[1].Status == threads.StatusDone && d.ConversationID != ""
	check("thread saved", saved, fmt.Sprintf("%d messages", len(d.Messages)))
	undone := 0
	for _, e := range r.Edits {
		if e.Undone {
			undone++
		}
	}
	check("edits undone", undone == len(r.Edits), fmt.Sprintf("%d of %d", undone, len(r.Edits)))
	return checks
}

// undoAskEdits restores newest first, keeping the report in edit order.
func undoAskEdits(ctx context.Context, m *agent.Module, edits []store.Edit) []reportEdit {
	out := make([]reportEdit, len(edits))
	for i, e := range slices.Backward(edits) {
		re := reportEdit{Field: string(e.Field), Old: e.OldValue, New: e.NewValue}
		if e.Field == store.FieldTrash { // a time
			re.Old, re.New = "", "trashed"
		}
		if _, err := m.Ask().Undo(ctx, e.ID); err == nil {
			re.Undone = true
		}
		out[i] = re
	}
	return out
}

// turnTimes are the citations a session's turns may be cited by.
func turnTimes(d store.SessionDetail) map[string]bool {
	times := map[string]bool{}
	for _, s := range d.Segments {
		if s.Lang == store.GapLang {
			continue
		}
		dur := time.Duration(max(s.StartMs, 0)) * time.Millisecond
		h, mins, secs := int(dur.Hours()), int(dur.Minutes())%60, int(dur.Seconds())%60
		if h > 0 {
			times[fmt.Sprintf("[%d:%02d:%02d]", h, mins, secs)] = true
		} else {
			times[fmt.Sprintf("[%02d:%02d]", mins, secs)] = true
		}
	}
	return times
}

// failedAsk reports a question that could not be asked at all.
func failedAsk(o askOptions, err error) *reportAsk {
	outcome := "not asked"
	if errors.Is(err, gateway.ErrUnavailable) {
		outcome = "no agent"
	}
	return &reportAsk{Question: o.question, Tools: []string{}, Edits: []reportEdit{}, Outcome: outcome,
		Thread: reportThread{Messages: []reportMessage{}},
		checks: []reportCheck{{Name: "answered", Pass: false, Detail: outcome}}}
}
