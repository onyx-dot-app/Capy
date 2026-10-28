package ui

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"

	"github.com/onyx-dot-app/Capy/internal/agent/ask"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/threads"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/views/addon"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
)

// scopeKind is what a new thread asks about.
type scopeKind int

const (
	thisSession scopeKind = iota
	allSessions
	lastWeek
	customRange
	thisBatch
)

// scope is the scope pop-up's choice; from and to are a custom range's
// days, local; batch the batch asked about.
type scope struct {
	kind     scopeKind
	from, to time.Time
	batch    addon.Batch
}

// key names the threads a scope lists and keeps open: a session's own, or
// the cross-session ones of a kind.
func (s scope) key(session string) string {
	switch s.kind {
	case thisSession:
		return "session:" + session
	case lastWeek:
		return "week"
	case customRange:
		return "range:" + s.from.Format(time.DateOnly) + ":" + s.to.Format(time.DateOnly)
	case thisBatch:
		return "batch:" + s.batch.ID
	}
	return "all"
}

// label is the scope pop-up's text.
func (s scope) label() string {
	switch s.kind {
	case thisSession:
		return "This session"
	case lastWeek:
		return "Last 7 days"
	case customRange:
		return s.from.Format("Jan 2") + " – " + s.to.Format("Jan 2")
	case thisBatch:
		return s.batch.Name
	}
	return "All sessions"
}

// holds reports whether cross-session thread t is listed in this scope's
// History: every session's in All Sessions, the batch's in This Batch, a
// range's in the others.
func (s scope) holds(t threads.Thread) bool {
	_, _, isRange := threads.ParseRange(t.Scope)
	switch s.kind {
	case allSessions:
		return t.Scope == threads.ScopeAll
	case thisBatch:
		return t.Scope == threads.BatchScope(s.batch.ID)
	}
	return isRange
}

// consentAsk is the first-Ask dialog of an agent, and the question it
// holds back.
type consentAsk struct {
	open            bool
	agent, question string
}

// data is what Ask's surfaces share, the inspector and the caption bar's
// panel: the threads, their runs and what was read, so that a session's
// thread asked in the bar is the inspector's once recording stops, and an
// answer that runs on finishes there. Main thread only.
type data struct {
	open     map[string]string                           // the open thread of each scope key, "" a new one
	threads  map[string][]threads.Thread                 // History of each scope key, once read
	details  map[string]threads.Detail                   // threads read, by ID
	runs     map[string]*running                         // the questions being answered, by thread
	turnsOf  map[string]*ask.Turn                        // their turns, to stop them
	pending  map[string][]string                         // follow-ups waiting for the run before them, oldest first
	failures map[string]*gateway.Failed                  // how a thread's last run failed
	used     map[string][]gateway.ToolCall               // the tools its last run called
	edits    map[string]store.Edit                       // the edits chips show, by ID
	undone   map[string]string                           // why an Undo did not undo
	undoing  map[string]bool                             // Undos going
	turns    map[string]map[int64]string                 // each session's turns' openings, by time
	answers  map[string]answerCache                      // linked answers, by message
	loading  map[string]bool                             // reads going, by what they read
	again    map[string]func(ctx context.Context) func() // reads asked for while the same one goes

	sessions     []ask.Session // sessions a cross-session answer may name
	sessionsRead bool
	sessionsGen  int  // counts the reads of sessions, which answers are parsed against
	starting     bool // a question is being sent
}

func newData() *data {
	return &data{open: map[string]string{}, threads: map[string][]threads.Thread{}, details: map[string]threads.Detail{},
		runs: map[string]*running{}, turnsOf: map[string]*ask.Turn{}, pending: map[string][]string{},
		failures: map[string]*gateway.Failed{}, used: map[string][]gateway.ToolCall{}, edits: map[string]store.Edit{},
		undone: map[string]string{}, undoing: map[string]bool{}, turns: map[string]map[int64]string{},
		answers: map[string]answerCache{}, loading: map[string]bool{}, again: map[string]func(ctx context.Context) func(){}}
}

// panel is Ask's component on one surface: its header, the open thread
// and the composer. The inspector's chooses its scope; the caption bar's
// (live) asks about the session being recorded, in the bar's colors.
// Main thread only; it reads and asks off it, through Host.Update.
type panel struct {
	*data
	a       *AddOn
	live    bool
	scope   scope
	session addon.Session // the session in view; ID "" for none

	draft    string
	sendNow  bool // Return was pressed in the composer
	caretEnd bool // put the caret at the end of the draft
	focus    bool // focus the composer
	notice   string
	consent  consentAsk
	ranging  bool // the Custom Range dialog
	from, to time.Time

	thread      ui.ListState     // the open thread, following its end
	threadShown string           // the thread it showed last
	md          markdown.Options // how answers show on this surface
}

func newPanel(a *AddOn, d *data, live bool) *panel {
	return &panel{data: d, a: a, live: live, scope: scope{kind: thisSession}}
}

// colors is the surface's palette: Capy's, or the caption bar's.
func (p *panel) colors(c *ui.Context) kit.Palette {
	if p.live {
		return barPalette
	}
	return kit.Colors(c)
}

// barPalette is the inspector's palette remapped onto the caption bar's
// dark tint, as Q1.9 gives it.
var barPalette = func() kit.Palette {
	white := func(a float32) ui.Color { return ui.RGBA(255, 255, 255, a) }
	p := kit.Palette{
		Label: white(0.93), Secondary: white(0.56), Tertiary: white(0.34), Separator: white(0.16),
		Fill: white(0.12), Fill2: white(0.12), Window: white(0.06), Group: kit.BarTint,
		Link: ui.Hex("#F2C9A2"), Highlight: white(0.14), Danger: ui.Hex("#ff6b6f"), Success: ui.Hex("#32c25a"),
		Accent: ui.Hex("#9a6b45"),
	}
	return p
}()

// answerCache is an answer's text linked, for as long as the text, the
// sessions its titles link and the turns its tooltips show hold.
type answerCache struct {
	text  string
	gen   int
	turns int // how many sessions' turns were read
	linked
}

// effective is the scope in force: This Session falls back to All
// Sessions with no session in view.
func (p *panel) effective() scope {
	if p.live {
		return scope{kind: thisSession}
	}
	if p.scope.kind == thisSession && p.session.ID == "" {
		return scope{kind: allSessions}
	}
	return p.scope
}

func (p *panel) key() string { return p.effective().key(p.session.ID) }

// askScope is the effective scope as Ask takes it.
func (p *panel) askScope() ask.Scope {
	sc := p.effective()
	switch sc.kind {
	case thisSession:
		return ask.Scope{SessionID: p.session.ID, Title: p.session.Title}
	case lastWeek:
		today := day(time.Now())
		return ask.Scope{From: today.AddDate(0, 0, -6), To: endOfDay(today)}
	case customRange:
		return ask.Scope{From: sc.from, To: endOfDay(sc.to)}
	case thisBatch:
		return ask.Scope{Batch: sc.batch.ID, BatchName: sc.batch.Name, SessionIDs: sc.batch.SessionIDs}
	}
	return ask.Scope{}
}

func day(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}

func endOfDay(t time.Time) time.Time { return day(t).AddDate(0, 0, 1).Add(-time.Millisecond) }

// chosen is the agent of a new thread: the user's choice, else the first
// found, as Settings shows it.
func (p *panel) chosen() gateway.Available {
	usable := p.a.usable()
	id := p.a.o.Prefs.Get().Agent
	for _, av := range usable {
		if av.ID == id {
			return av
		}
	}
	if len(usable) == 0 {
		return gateway.Available{}
	}
	return usable[0]
}

// info is agent id as the gateway knows it.
func (p *panel) info(id string) gateway.Info {
	for _, av := range p.a.found {
		if av.ID == id {
			return av.Info
		}
	}
	return gateway.Info{ID: id, Name: "The agent"}
}

// view builds the panel for session s, nil for none.
func (p *panel) view(c *ui.Context, s *addon.Session) {
	if s != nil {
		p.session = *s
	} else {
		p.session = addon.Session{}
	}
	key, sc := p.key(), p.effective()
	if _, ok := p.threads[key]; !ok {
		session := "" // the cross-session threads, but in This Session
		if sc.kind == thisSession {
			session = p.session.ID
		}
		p.list(key, sc, session)
	}
	id := p.open[key]
	if _, ok := p.details[id]; id != "" && !ok && p.runs[id] == nil {
		p.load(id)
	}
	if p.effective().kind != thisSession && !p.sessionsRead {
		p.loadSessions()
	}
	ui.Column(c).Fill().AlignItems(ui.Stretch).Children(func() {
		p.header(c, key, id)
		p.threadView(c, id)
		p.composer(c, id)
	})
	p.dialogs(c)
}

// header is the toolbar row's part over the pane: the scope pop-up,
// History and New Thread.
func (p *panel) header(c *ui.Context, key, id string) {
	pal := p.colors(c)
	height := float32(52) // the toolbar row's
	if p.live {
		height = 40 // the caption bar header's
	}
	ui.Row(c).Height(height).Padding(0, 10).Gap(2).AlignItems(ui.Center).DragWindow().Children(func() {
		if p.live {
			p.liveScope(c)
		} else {
			p.scopeButton(c, pal)
		}
		ui.Box(c).Grow(1)
		headerButton(c, pal, historyIcon, "History").Menu(func(m *ui.Menu) { p.historyMenu(m, key, id) })
		if headerButton(c, pal, composeIcon, "New Thread").Clicked() {
			p.newThread()
		}
	})
}

// liveScope is the caption bar's scope, which is fixed: "This session"
// with a red Live tag.
func (p *panel) liveScope(c *ui.Context) {
	ui.Row(c).Gap(6).Padding(0, 6).AlignItems(ui.Center).Children(func() {
		ui.Text(c, "This session").FontWeight(600).SingleLine()
		live := ui.Hex("#ff8a8e")
		ui.Row(c).Gap(4).Padding(1, 6).Radius(4).Background(kit.LiveRed.Alpha(0.18)).AlignItems(ui.Center).Children(func() {
			kit.LiveDot(c, live).Size(6, 6)
			ui.Text(c, "Live").FontSize(11).FontWeight(600).TextColor(live).SingleLine()
		})
	})
}

// scopeButton is the inspector's scope pop-up.
func (p *panel) scopeButton(c *ui.Context, pal kit.Palette) {
	sc := ui.ButtonBase(c).Label("Scope").Padding(3, 6).Radius(5).Gap(4).AlignItems(ui.Center)
	if sc.Hovered() {
		sc.Background(pal.Fill2)
	}
	sc.Menu(func(m *ui.Menu) { p.scopeMenu(m) }).Children(func() {
		ui.Text(c, p.effective().label()).FontWeight(600).SingleLine()
		ui.Icon(c, kit.ChevronDownIcon).Size(11, 11).TextColor(pal.Secondary)
	})
}

// headerButton is an icon button of the pane's header, 28 × 26.
func headerButton(c *ui.Context, pal kit.Palette, icon *ui.SVG, label string) ui.Element {
	b := ui.ButtonBase(c).Label(label).Tooltip(label).Size(28, 26).Radius(6).Center().TextColor(pal.Secondary)
	if b.Hovered() {
		b.Background(pal.Fill2).TextColor(pal.Label)
	}
	b.Children(func() { ui.Icon(c, icon).Size(15, 15) })
	return b
}

func (p *panel) scopeMenu(m *ui.Menu) {
	kind := p.effective().kind
	if m.Item("This Session").Checked(kind == thisSession).Disabled(p.session.ID == "").Chosen() {
		p.scope = scope{kind: thisSession}
	}
	if kind == thisBatch { // chosen from the batch's menu: it stays until another is
		m.Item(p.scope.batch.Name).Checked(true)
	}
	if m.Item("All Sessions").Checked(kind == allSessions).Chosen() {
		p.scope = scope{kind: allSessions}
	}
	if m.Item("Last 7 Days").Checked(kind == lastWeek).Chosen() {
		p.scope = scope{kind: lastWeek}
	}
	m.Separator()
	if m.Item("Custom Range…").Checked(kind == customRange).Chosen() {
		p.ranging = true
		p.to = day(time.Now())
		p.from = p.to.AddDate(0, 0, -7)
		if p.scope.kind == customRange {
			p.from, p.to = p.scope.from, p.scope.to
		}
	}
}

// historyMenu lists the scope's threads, oldest first, the open one
// checked: the session's title (or the scope) heads it.
func (p *panel) historyMenu(m *ui.Menu, key, id string) {
	head := p.effective().label()
	if p.effective().kind == thisSession && p.session.Title != "" {
		head = p.session.Title
	}
	m.Item(head).Disabled(true)
	for _, t := range p.threads[key] {
		label := t.Title + " · " + when(t.CreatedAt)
		if from, to, ok := threads.ParseRange(t.Scope); ok { // which range it asked about
			label = t.Title + " · " + from.Local().Format("Jan 2") + " – " + to.Local().Format("Jan 2")
		}
		if m.Item(label).Checked(t.ID == id).Chosen() {
			p.open[key] = t.ID
			p.focus = true
		}
	}
	m.Separator()
	if m.Item("New Thread").Shortcut(ui.Cmd, ui.KeyN).Chosen() {
		p.newThread()
	}
	if id != "" && p.effective().kind != thisSession {
		m.Separator()
		if m.Item("Delete This Thread").Disabled(p.runs[id] != nil || p.starting).Chosen() {
			p.deleteThread(key, id)
		}
	}
}

// when is a thread's time in History: its time today, else its date.
func when(t time.Time) string {
	t = t.Local()
	if day(t).Equal(day(time.Now())) {
		return t.Format("15:04")
	}
	return t.Format("Jan 2")
}

// newThread opens a new thread in the scope in view.
func (p *panel) newThread() {
	p.open[p.key()] = ""
	p.focus = true
}

// composer is the field the question is typed in, Send or Stop, the
// agent pop-up and Continue in….
func (p *panel) composer(c *ui.Context, id string) {
	pal := p.colors(c)
	r := p.runs[id]
	if p.sendNow {
		p.sendNow = false
		if q := strings.TrimSpace(p.draft); q != "" && p.submit(q) {
			p.draft = ""
		}
	}
	ui.Column(c).Padding(8, 12, 10).AlignItems(ui.Stretch).Children(func() {
		if p.notice != "" {
			ui.Text(c, p.notice).FontSize(12).TextColor(pal.Secondary).Margin(0, 2, 6, 2)
		}
		box := ui.Row(c).Radius(10).Background(pal.Window).Padding(7, 7, 7, 12).Gap(8).AlignItems(ui.End).MinHeight(40)
		if box.FocusWithin() {
			box.Border(1, pal.Accent).Shadow(0, 0, 0, 3.5, pal.Accent.Alpha(0.25))
		} else {
			box.Border(1, pal.Separator).Shadow(0, 1, 2, 0, ui.RGBA(0, 0, 0, 0.04))
		}
		box.Children(func() {
			placeholder := "Ask anything…"
			if id != "" {
				placeholder = "Ask a follow-up…"
			}
			ta := ui.TextAreaBase(c, &p.draft).Label("Question").Lines(1, 6).Placeholder(placeholder).
				LineHeight(18.0/13).Padding(0, 0, 4, 0).Grow(1).Basis(0).
				HandleInput(func(ev ui.InputEvent) bool {
					if ev.Kind == ui.InputKeyDown && ev.Key == ui.KeyEnter && ev.Mods == 0 {
						p.sendNow = true
						return true
					}
					return false
				})
			if p.focus { // once: Focus every frame would keep it
				ta.Focus()
				p.focus = false
			}
			if p.caretEnd {
				n := utf8.RuneCountInString(p.draft)
				ta.SetTextSelection(n, n)
				p.caretEnd = false
			}
			running := r != nil && !r.st.Ended
			if (running || p.live) && ta.Shortcut(0, ui.KeyEscape) {
				if running {
					p.stop(id)
				} else { // the caption bar's panel closes
					p.a.closeLive()
				}
			}
			p.sendButton(c, id, r)
		})
		p.footer(c, id)
	})
}

// sendButton is Send, or Stop while the thread's question is answered.
func (p *panel) sendButton(c *ui.Context, id string, r *running) {
	pal := p.colors(c)
	if r != nil && !r.st.Ended {
		b := ui.ButtonBase(c).Label("Stop").Tooltip("Stop").Size(26, 26).Radius(13).Center().Background(pal.Label)
		b.Children(func() { ui.Box(c).Size(9, 9).Radius(2).Background(pal.Window) })
		if b.Clicked() {
			p.stop(id)
		}
		return
	}
	empty := strings.TrimSpace(p.draft) == "" || p.starting
	b := ui.ButtonBase(c).Label("Send").Size(26, 26).Radius(13).Center().Disabled(empty)
	if empty {
		b.Background(pal.Fill2).TextColor(pal.Tertiary)
	} else {
		b.Background(pal.Accent).TextColor(ui.RGB(255, 255, 255))
	}
	b.Children(func() { ui.Icon(c, sendIcon).Size(13, 13) })
	if b.Clicked() {
		p.sendNow = true
	}
}

// footer is the agent pop-up under the field, the agent the next question
// goes to, in a thread too, and, once the thread has a conversation,
// Continue in….
func (p *panel) footer(c *ui.Context, id string) {
	pal := p.colors(c)
	ui.Row(c).Margin(8, 2, 0, 2).AlignItems(ui.Center).Children(func() {
		chosen, usable := p.chosen(), p.a.usable()
		agent := ui.ButtonBase(c).Label("Agent").Gap(3).AlignItems(ui.Center).TextColor(pal.Secondary)
		if len(usable) > 1 {
			agent.Menu(func(m *ui.Menu) {
				for _, av := range usable {
					if m.Item(av.Name).Checked(av.ID == chosen.ID).Chosen() {
						p.a.setAgent(av.ID)
					}
				}
			})
		}
		agent.Children(func() {
			ui.Text(c, chosen.Name).FontSize(11).FontWeight(500).SingleLine()
			if len(usable) > 1 {
				ui.Icon(c, kit.UpDownIcon).Size(10, 10)
			}
		})
		ui.Box(c).Grow(1)
		if d, ok := p.details[id]; ok && d.ConversationID != "" {
			name := p.info(d.Agent).Name
			cont := ui.ButtonBase(c).Label("Continue in " + name).Gap(3).AlignItems(ui.Center).TextColor(pal.Link)
			cont.Children(func() {
				ui.Text(c, "Continue in "+name).FontSize(11).FontWeight(500).SingleLine()
				ui.Icon(c, outIcon).Size(10, 10)
			})
			if cont.Clicked() {
				p.continueIn(id)
			}
		}
	})
}

// dialogs are the first-Ask consent of an agent, and Custom Range….
func (p *panel) dialogs(c *ui.Context) {
	if p.consent.open {
		info := p.info(p.consent.agent)
		message := "Capy will send your question, and the parts of your recordings " + info.Name + " reads"
		if info.Sends != nil {
			message += ", to " + info.Sends.Provider + " through " + info.Sends.Account
		}
		message += ".\n\nYou’ll be asked once for each agent."
		switch ui.AlertDialog(c, &p.consent.open, "Ask with "+info.Name+"?", message, "Cancel", "Allow") {
		case 0:
			p.consent = consentAsk{} // the question stays in the field
			p.focus = true
		case 1:
			p.allow()
		}
	}
	if p.ranging {
		p.rangeDialog(c)
	}
}

// rangeDialog asks for the days of a custom range.
func (p *panel) rangeDialog(c *ui.Context) {
	pal := p.colors(c)
	ui.Modal(c, &p.ranging, func() {
		ui.Column(c).Width(280).Gap(12).AlignItems(ui.Stretch).Children(func() {
			ui.Text(c, "Custom Range").Bold()
			for _, f := range []struct {
				label string
				date  *time.Time
			}{{"From", &p.from}, {"To", &p.to}} {
				ui.Row(c.Key(f.label)).Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, f.label).Width(40).TextColor(pal.Secondary)
					ui.DateInput(c, f.date).Label(f.label).Grow(1)
				})
			}
			bad := p.to.Before(p.from)
			if bad {
				ui.Text(c, "From must not be after To.").FontSize(12).TextColor(pal.Danger)
			}
			ui.Row(c).Gap(8).Justify(ui.End).Children(func() {
				if ui.Button(c, "Cancel").Clicked() {
					p.ranging = false
				}
				if ui.PrimaryButton(c, "Choose").Disabled(bad).Clicked() {
					p.scope = scope{kind: customRange, from: day(p.from), to: day(p.to)}
					p.ranging = false
				}
			})
		})
	})
}
