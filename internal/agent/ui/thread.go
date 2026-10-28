package ui

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"

	"github.com/onyx-dot-app/Capy/internal/agent/ask"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/threads"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
)

// Suggestions of the empty state, which send at once but the one that
// ends in "…", which fills the field for the user to finish.
var (
	sessionSuggestions = []string{"Summarize", "Action items", "Decisions", "Was I mentioned?", "Draft a follow-up email"}
	acrossSuggestions  = []string{"What did I commit to this week?", "Find where we discussed…", "Catch me up on yesterday"}
	liveSuggestions    = []string{ask.CatchUpQuestion, "Action items so far", "Was I mentioned?"}
)

// threadView is the open thread, or the empty state of a new one. A
// thread opens at its end, and glides to keep it in view as an answer
// comes until the user scrolls up; Jump to Latest shows while they are
// away from it.
func (p *panel) threadView(c *ui.Context, id string) {
	r := p.runs[id]
	d, loaded := p.details[id]
	if id == "" && r == nil {
		ui.Scroll(c).Grow(1).Basis(0).Children(func() {
			ui.Column(c).Padding(8, 16, 4).AlignItems(ui.Stretch).Children(func() { p.empty(c) })
		})
		return
	}
	if p.threadShown != id {
		p.threadShown = id
		p.thread.ScrollToEnd()
	}
	p.thread.FollowEnd, p.thread.FollowAnimation = true, &ui.Smooth
	p.thread.Key = func(int) any { return id }
	ui.Column(c).Grow(1).Basis(0).AlignItems(ui.Stretch).Children(func() {
		// One row, the whole thread, which grows as an answer comes.
		ui.List(c, &p.thread, 1, func(int) {
			ui.Column(c).Padding(8, 16, 4).Gap(12).AlignItems(ui.Stretch).Children(func() {
				if !loaded && r == nil {
					ui.Spinner(c).Label("Loading")
					return
				}
				p.messages(c, d, r)
				if r != nil {
					p.runningView(c, r)
				}
			})
		}).Grow(1).Basis(0)
		if first, last := p.thread.Visible(); last >= first && !p.thread.AtEnd() { // laid out, and away from the end
			p.jumpToLatest(c)
		}
	})
}

// jumpToLatest is the round button over the thread's corner that
// scrolls back to its end, where it follows again.
func (p *panel) jumpToLatest(c *ui.Context) {
	pal := p.colors(c)
	b := ui.ButtonBase(c).Label("Jump to Latest").Tooltip("Jump to Latest").Absolute().Right(16).Bottom(8).Size(28, 28).Radius(14).Center().
		Background(pal.Window).Border(1, pal.Separator).Shadow(0, 1, 3, 0, ui.RGBA(0, 0, 0, 0.12)).TextColor(pal.Secondary)
	if b.Hovered() {
		b.TextColor(pal.Label)
	}
	b.Children(func() { ui.Icon(c, kit.ChevronDownIcon).Size(14, 14) })
	if b.Clicked() {
		p.thread.ScrollToEnd()
	}
}

// empty is a new thread's state: what the scope asks about, and
// suggestions.
func (p *panel) empty(c *ui.Context) {
	pal := p.colors(c)
	name := p.chosen().Name
	heading, body, suggestions := "Ask across your sessions", name+" searches every session in Capy to answer.", acrossSuggestions
	switch {
	case p.live:
		heading, body, suggestions = "Ask about this session",
			name+" reads what’s been said so far. The sentence being spoken now isn’t included yet.", liveSuggestions
	case p.effective().kind == thisSession:
		heading, body, suggestions = "Ask about this session", name+" reads this session’s transcript to answer.", sessionSuggestions
	}
	ui.Column(c).Padding(6, 0, 0, 0).AlignItems(ui.Start).Children(func() {
		ui.Text(c, heading).FontSize(15).FontWeight(600)
		ui.Text(c, body).FontSize(12).TextColor(pal.Secondary).Margin(4, 0, 14, 0)
		ui.Column(c).Gap(6).AlignItems(ui.Start).Children(func() {
			for _, s := range suggestions {
				b := ui.ButtonBase(c.Key(s)).Label(s).Height(26).Padding(0, 10).Radius(7).Border(1, pal.Separator).AlignItems(ui.Center)
				if b.Hovered() {
					b.Background(pal.Fill)
				}
				b.Children(func() { ui.Text(c, s).FontSize(12).SingleLine() })
				if b.Clicked() {
					p.suggest(s)
				}
			}
		})
	})
}

// suggest sends suggestion s, or fills the field with one to finish.
func (p *panel) suggest(s string) {
	if open, ok := strings.CutSuffix(s, "…"); ok {
		p.draft, p.caretEnd, p.focus = open+" ", true, true
		return
	}
	if p.live && s == ask.CatchUpQuestion {
		p.catchUp()
		return
	}
	p.submit(s)
}

// messages are a thread's saved messages: read before a run of it began,
// they never hold its question, which the run shows.
func (p *panel) messages(c *ui.Context, d threads.Detail, r *running) {
	msgs := d.Messages
	last := lastQuestion(msgs)
	for i, m := range msgs {
		switch m.Role {
		case threads.RoleUser:
			p.bubble(c.Key(m.ID), m.Text, 1)
			if i == last && r == nil && m.Status == threads.StatusUnanswered {
				if f := p.failures[d.ID]; f != nil {
					p.problem(c, d.Agent, f, m.Text)
				} else {
					p.notAnswered(c, m.Text)
				}
			}
		case threads.RoleAgent:
			if i > last && r == nil {
				p.toolLine(c.Key("tools-"+m.ID), p.used[d.ID]) // the last run's, while Capy runs
			}
			p.answer(c.Key(m.ID), m.ID, m.Text, d.SessionID, false)
			if m.Status == threads.StatusIncomplete && i > last && r == nil {
				f := p.failures[d.ID]
				if f == nil {
					f = &gateway.Failed{Kind: gateway.Stopped}
				}
				p.problem(c, d.Agent, f, msgs[last].Text)
			}
		case threads.RoleEdit:
			if e, ok := p.edits[m.Text]; ok {
				p.chip(c.Key(m.ID), e)
			}
		}
	}
}

// lastQuestion is the index of msgs' last question, -1 for none.
func lastQuestion(msgs []threads.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == threads.RoleUser {
			return i
		}
	}
	return -1
}

// runningView is a question being answered: the tools it read, the
// answer so far, what it does now, and once it ends, its edits or how it
// failed. A follow-up typed meanwhile waits under it.
func (p *panel) runningView(c *ui.Context, r *running) {
	pal := p.colors(c)
	st := r.st
	p.bubble(c.Key("run-question"), r.question, 1)
	ui.Column(c.Key("run")).Gap(6).AlignItems(ui.Stretch).Children(func() {
		p.toolLine(c, st.Used)
		streams := p.info(r.agent).Streams
		if st.Answer != "" {
			p.answer(c, "run:"+r.id, st.Answer, r.session, streams && !st.Ended)
		}
		if !st.Ended && (st.Tool != nil || st.Answer == "" || !streams) {
			ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
				ui.Spinner(c).FontSize(12).Label("Working")
				ui.Text(c, toolDoing(st.Tool, p.titleOf)).FontSize(12).TextColor(pal.Secondary)
			})
		}
	})
	if st.Ended {
		for _, e := range st.Edits {
			p.chip(c.Key("run-edit-"+e.ID), p.editOf(e))
		}
		if st.Failure != nil {
			p.problem(c, r.agent, st.Failure, r.question)
		}
	}
	for i, q := range p.pending[r.id] {
		p.bubble(c.Key("pending-"+strconv.Itoa(i)), q, 0.5)
	}
}

// toolLine is the quiet line of the tools a run called: "✓ Read Launch
// rehearsal".
func (p *panel) toolLine(c *ui.Context, used []gateway.ToolCall) {
	done := toolsDone(used, p.titleOf)
	if done == "" {
		return
	}
	ui.Row(c).Gap(5).AlignItems(ui.Center).TextColor(p.colors(c).Tertiary).Children(func() {
		ui.Icon(c, kit.CheckIcon).Size(11, 11)
		ui.Text(c, done).FontSize(11)
	})
}

// editOf is e as last read, which an Undo may have changed.
func (p *panel) editOf(e store.Edit) store.Edit {
	if known, ok := p.edits[e.ID]; ok && !known.UndoneAt.IsZero() {
		return known
	}
	return e
}

// bubble is a question, right-aligned on a quiet fill.
func (p *panel) bubble(c *ui.Context, text string, opacity float32) {
	pal := p.colors(c)
	size := &p.a.inspectorSize
	if p.live {
		size = &p.a.liveSize
	}
	width := p.a.width(size)
	ui.Text(c, text).Selectable().AlignSelf(ui.End).MaxWidth((width-32)*0.86).Padding(6, 10).Radius(10).
		Background(pal.Fill2).LineHeight(18.0 / 13).Opacity(opacity)
}

// answer is an answer's text, its citations and titles linked once per
// text, streaming in while it is written, with its context menu.
func (p *panel) answer(c *ui.Context, key, text, session string, streaming bool) {
	cache := p.answers[key]
	if cache.text != text || cache.gen != p.sessionsGen || cache.turns != len(p.turns) {
		var sessions []ask.Session
		if session == "" {
			sessions = p.sessions
		}
		cache = answerCache{text: text, gen: p.sessionsGen, turns: len(p.turns), linked: linkAnswer(text, session, sessions, p.turns)}
		p.answers[key] = cache
	}
	for _, id := range cache.cited {
		if _, ok := p.turns[id]; !ok {
			p.loadTurns(id)
		}
	}
	pal := p.colors(c)
	p.md.Theme, p.md.ShareSelection = answerTheme(pal), true
	p.md.OnLink = func(dest string) { p.openLink(c, dest) }
	ui.Column(c).Gap(2).AlignItems(ui.Stretch).Selectable().ContextMenu(func(m *ui.Menu) {
		if m.Item("Copy").Chosen() {
			c.WriteClipboard(plain(text))
		}
		if m.Item("Copy as Markdown").Chosen() {
			c.WriteClipboard(text)
		}
		m.Separator()
		if m.Item("Ask a Follow-up").Chosen() {
			p.focus = true
		}
	}).Children(func() {
		if streaming {
			markdown.Stream(c, cache.src, &p.md)
			blink(c, pal.Label)
		} else {
			markdown.View(c, cache.src, &p.md)
		}
	})
}

// blink is the streaming caret, 2 × 14, on for half of each second.
func blink(c *ui.Context, color ui.Color) {
	on := c.Now().UnixMilli()%1000 < 500
	c.After(time.Duration(500-c.Now().UnixMilli()%500) * time.Millisecond)
	caret := ui.Box(c).Size(2, 14).Background(color)
	if !on {
		caret.Opacity(0)
	}
}

// openSession shows a session an answer names, at a cited turn.
func (p *panel) openSession(id string, atMs int64, cite bool) {
	if cite {
		p.a.host.ShowTranscript(id, atMs)
		return
	}
	p.a.host.ShowSession(id)
}

// titleOf is a session's title as Ask's lines name it.
func (p *panel) titleOf(id string) string {
	if id != "" && id == p.session.ID && p.session.Title != "" {
		return p.session.Title
	}
	for _, s := range p.sessions {
		if s.ID == id {
			return s.Title
		}
	}
	return "a session"
}

// toolDoing says what a tool call going does: "Reading Launch rehearsal…".
func toolDoing(call *gateway.ToolCall, title func(string) string) string {
	if call == nil {
		return "Thinking…"
	}
	switch call.Name {
	case "list_sessions":
		return "Listing sessions…"
	case "search":
		return "Searching sessions…"
	case "get_session":
		return "Reading " + title(call.SessionID) + "…"
	case "get_live_session":
		return "Reading the live session…"
	case "get_profile":
		return "Checking your name…"
	case "rename_session":
		return "Renaming " + title(call.SessionID) + "…"
	case "update_recap":
		return "Editing the recap…"
	case "delete_session":
		return "Moving to Recently Deleted…"
	}
	return "Working…"
}

// toolsDone is the calls done, each named once: "Read Launch rehearsal ·
// Searched sessions".
func toolsDone(calls []gateway.ToolCall, title func(string) string) string {
	var done []string
	for _, call := range calls {
		var s string
		switch call.Name {
		case "list_sessions":
			s = "Listed sessions"
		case "search":
			s = "Searched sessions"
		case "get_session":
			s = "Read " + title(call.SessionID)
		case "get_live_session":
			s = "Read the live session"
		case "get_profile":
			s = "Checked your name"
		case "rename_session":
			s = "Renamed session"
		case "update_recap":
			s = "Edited the recap"
		case "delete_session":
			s = "Moved to Recently Deleted"
		default:
			continue
		}
		if !slices.Contains(done, s) {
			done = append(done, s)
		}
	}
	return strings.Join(done, " · ")
}

// chip is an edit the agent made, with its Undo.
func (p *panel) chip(c *ui.Context, e store.Edit) {
	pal := p.colors(c)
	var what string
	switch e.Field {
	case store.FieldTitle:
		what = "Renamed to “" + e.NewValue + "”"
		if e.NewValue == "" {
			what = "Renamed to its recap’s title"
		}
	case store.FieldRecap:
		what = "Recap updated"
	case store.FieldTrash:
		what = "Moved “" + p.titleOf(e.SessionID) + "” to Recently Deleted"
	}
	ui.Row(c).Gap(8).Padding(6, 10).Radius(8).Border(1, pal.Separator).AlignItems(ui.Center).TextColor(pal.Secondary).Children(func() {
		ui.Icon(c, pencilIcon).Size(13, 13)
		ui.Text(c, what).FontSize(12).Grow(1).Basis(0)
		switch {
		case p.undone[e.ID] != "":
			ui.Text(c, p.undone[e.ID]).FontSize(12)
		case !e.UndoneAt.IsZero():
			ui.Text(c, "Undone").FontSize(12)
		case p.undoing[e.ID]:
			ui.Spinner(c).FontSize(12).Label("Undoing")
		default:
			if ui.Link(c, "Undo", "").Label("Undo " + what).FontSize(12).FontWeight(500).TextColor(pal.Accent).Clicked() {
				p.undo(e.ID)
			}
		}
	})
}

// notAnswered is a question whose run never ended, as when Capy quit.
func (p *panel) notAnswered(c *ui.Context, question string) {
	pal := p.colors(c)
	ui.RichText(c).FontSize(12).TextColor(pal.Secondary).Children(func() {
		ui.Text(c, "Not answered · ")
		if ui.Link(c, "Ask Again", "").TextColor(pal.Accent).FontWeight(500).Clicked() {
			p.retry(question)
		}
	})
}

// retry asks question again in the open thread.
func (p *panel) retry(question string) {
	if p.open[p.key()] != "" {
		p.submit(question)
	}
}

// problem is a quiet note of why a run failed, and what to do.
func (p *panel) problem(c *ui.Context, agent string, f *gateway.Failed, question string) {
	pal := p.colors(c)
	info := p.info(agent)
	icon, message, detail, action := kit.WarnIcon, "The answer stopped early.", "", "Retry"
	var do func()
	switch f.Kind {
	case gateway.SignedOut:
		icon, detail = kit.KeyIcon, ""
		if info.Program != "" { // an app with a program of its own: sign in there
			message, action = "Open "+info.Name+" and sign in.", "Open "+info.Name
			do = func() { p.openApp(info.Name) }
		} else {
			message, action = "Sign in to "+info.Name+": run "+info.Command+" once in Terminal.", "Open Terminal"
			do = func() { p.openApp("Terminal") }
		}
	case gateway.UsageLimit:
		icon, action = kit.InfoIcon, ""
		message = info.Name + " says you’ve reached your usage limit."
		detail = "Your question is saved; send it again later."
		if !f.ResetAt.IsZero() {
			detail = "It resets at " + f.ResetAt.Local().Format("3:04 PM") + ". Your question is saved; send it again then."
		}
	case gateway.Stalled: // silent past the idle limit, or long past the total one
		detail = info.Name + " didn’t answer in time."
	case gateway.Exited, gateway.ResumeRejected:
		detail = info.Name + " stopped unexpectedly."
	}
	if do == nil && action == "Retry" {
		do = func() { p.retry(question) }
	}
	ui.Row(c.Key("problem")).Gap(8).AlignItems(ui.Start).Children(func() {
		ui.Icon(c, icon).Size(15, 15).TextColor(pal.Secondary).Margin(1, 0, 0, 0)
		ui.Column(c).Grow(1).Basis(0).Gap(2).AlignItems(ui.Start).Children(func() {
			ui.Text(c, message).FontSize(12)
			if detail != "" {
				ui.Text(c, detail).FontSize(12).TextColor(pal.Secondary)
			}
			if action != "" && ui.Button(c, action).Height(24).Padding(0, 12).Margin(8, 0, 0, 0).Clicked() {
				do()
			}
		})
	})
}

// openApp opens an app, as Open Terminal and Open ChatGPT do.
func (p *panel) openApp(name string) {
	open, log := p.a.o.OpenApp, p.a.o.Log
	go func() {
		if err := open(name); err != nil {
			log.Warn("ask: open app", "err", err)
		}
	}()
}
