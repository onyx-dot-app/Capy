package ui

import (
	"context"
	"os/exec"
	"slices"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/prefs"
	"github.com/onyx-dot-app/Capy/internal/views/addon"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
)

// saveDelay is how long after the last drag of the inspector's divider
// its width is saved.
const saveDelay = 500 * time.Millisecond

// lookForAgents finds the agents Ask may use, off the main thread, as the
// app starts: Ask shows only once one is found.
func (a *AddOn) lookForAgents() {
	gw, update := a.o.Gateway, a.host.Update
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), lookTimeout)
		defer cancel()
		found := gw.Agents(ctx)
		update(func() { a.found = found })
	}()
}

// usable is the agents found that Ask can use.
func (a *AddOn) usable() []gateway.Available {
	return slices.DeleteFunc(slices.Clone(a.found), func(av gateway.Available) bool { return !av.Probe.Usable() })
}

// setAgent chooses the agent Ask's next question goes to, in a new thread
// or the one open, and shows it on every surface at once: Settings, the
// inspector and the caption bar's panel are in different windows.
func (a *AddOn) setAgent(id string) {
	if err := a.o.Prefs.Update(func(p *prefs.Prefs) { p.Agent = id }); err != nil {
		a.o.Log.Warn("save agent preferences", "err", err)
	}
	a.host.Update(func() {})
}

// askable reports whether Ask shows: Agent on, and an agent found.
func (a *AddOn) askable() bool { return a.on() && a.o.Ask != nil && len(a.usable()) > 0 }

func (a *AddOn) inspectorOpen() bool { return a.o.Prefs.Get().InspectorOpen }

// toggle opens or closes the inspector, focusing the question as it opens.
func (a *AddOn) toggle() {
	open := !a.inspectorOpen()
	if err := a.o.Prefs.Update(func(p *prefs.Prefs) { p.InspectorOpen = open }); err != nil {
		a.o.Log.Warn("save agent preferences", "err", err)
	}
	a.panel.focus = open
}

// paneSize is the width of one of Ask's panes, as the user last dragged
// it: field is where agent.json keeps it.
type paneSize struct {
	field func(*prefs.Prefs) *float32
	width float32 // 0 until read from agent.json
	saved atomicWidth
	timer *time.Timer // saves the width
}

func inspectorField(p *prefs.Prefs) *float32 { return &p.InspectorWidth }
func liveField(p *prefs.Prefs) *float32      { return &p.LivePanelWidth }

// width is pane s's width now.
func (a *AddOn) width(s *paneSize) float32 {
	if s.width == 0 {
		p := a.o.Prefs.Get()
		s.width = *s.field(&p)
	}
	return s.width
}

// resized keeps the width pane s was dragged to, and saves it once the
// drag rests. Main thread only; the save, which reads the width then,
// runs off it.
func (a *AddOn) resized(s *paneSize, w float32) {
	s.width = w
	s.saved.Store(w)
	if s.timer != nil {
		s.timer.Reset(saveDelay)
		return
	}
	pf, log, field, width := a.o.Prefs, a.o.Log, s.field, &s.saved
	s.timer = time.AfterFunc(saveDelay, func() {
		w := width.Load()
		if err := pf.Update(func(p *prefs.Prefs) { *field(p) = w }); err != nil {
			log.Warn("save agent preferences", "err", err)
		}
	})
}

// pane is Ask's pane of size s, resizable within agent.json's bounds.
func (a *AddOn) pane(s *paneSize, view func()) *addon.Pane {
	return &addon.Pane{Width: a.width(s), MinWidth: prefs.PaneMin, MaxWidth: prefs.PaneMax,
		View: view, Resized: func(w float32) { a.resized(s, w) }}
}

// Toolbar implements addon.AddOn: the Ask button, pressed while the
// inspector is open.
func (a *AddOn) Toolbar(c *ui.Context, _ *addon.Session) {
	if !a.askable() {
		return
	}
	p := kit.Colors(c)
	open := a.inspectorOpen()
	b := ui.ButtonBase(c).Label("Ask").Tooltip("Ask (⌘J)").Height(28).Padding(0, 10, 0, 8).Radius(6).Gap(5).
		AlignItems(ui.Center).TextColor(p.Secondary)
	if open || b.Hovered() {
		b.Background(p.Fill2).TextColor(p.Label)
	}
	b.Children(func() {
		ui.Icon(c, askIcon).Size(15, 15)
		ui.Text(c, "Ask").FontWeight(500).SingleLine()
	})
	if b.Clicked() {
		a.toggle()
	}
	ui.Box(c).Width(6)
}

// BatchMenu implements addon.AddOn: Ask About This Batch opens the
// inspector on a new thread about the batch's sessions.
func (a *AddOn) BatchMenu(m *ui.Menu, b addon.Batch) {
	if !a.askable() {
		return
	}
	if m.Item("Ask About This Batch").Disabled(len(b.SessionIDs) == 0).Chosen() {
		a.askAbout(b)
	}
}

// askAbout opens the inspector on a new thread about batch b.
func (a *AddOn) askAbout(b addon.Batch) {
	if !a.inspectorOpen() {
		if err := a.o.Prefs.Update(func(p *prefs.Prefs) { p.InspectorOpen = true }); err != nil {
			a.o.Log.Warn("save agent preferences", "err", err)
		}
	}
	a.panel.scope = scope{kind: thisBatch, batch: b}
	a.panel.newThread()
}

// CanAsk implements addon.AddOn: an agent is found and Agent is on.
func (a *AddOn) CanAsk() bool { return a.askable() }

// Ask implements addon.AddOn: question, in a new thread about session s,
// in the caption bar's panel, expanded from its capsule, when bar, else
// in the inspector, opened.
func (a *AddOn) Ask(s addon.Session, question string, bar bool) {
	if !a.askable() {
		return
	}
	p := a.panel
	switch {
	case bar:
		a.host.ExpandCaptionBar()
		a.openLive()
		p = a.live
	case !a.inspectorOpen():
		if err := a.o.Prefs.Update(func(p *prefs.Prefs) { p.InspectorOpen = true }); err != nil {
			a.o.Log.Warn("save agent preferences", "err", err)
		}
	}
	p.scope, p.session = scope{kind: thisSession}, s
	p.newThread()
	p.focus = false // the question is sent, not typed
	if !p.submit(question) {
		p.draft = question // another is being sent: it waits in the field
	}
}

// Shortcuts implements addon.AddOn: ⌘J opens and closes the inspector,
// ⌘N starts a thread in it.
func (a *AddOn) Shortcuts(c *ui.Context, _ *addon.Session) {
	if !a.askable() {
		return
	}
	if c.Shortcut(ui.Cmd, ui.KeyJ) {
		a.toggle()
		return
	}
	if a.inspectorOpen() && c.Shortcut(ui.Cmd, ui.KeyN) {
		a.panel.newThread()
	}
}

// Inspector implements addon.AddOn: Ask's pane, while it is open.
func (a *AddOn) Inspector(c *ui.Context, s *addon.Session) *addon.Pane {
	if !a.askable() || !a.inspectorOpen() {
		return nil
	}
	return a.pane(&a.inspectorSize, func() { a.panel.view(c, s) })
}

// openApp opens an app by name, as Finder would.
func openApp(name string) error { return exec.Command("open", "-a", name).Run() }
