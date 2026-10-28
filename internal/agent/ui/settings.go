package ui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/prefs"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
)

// lookTimeout bounds a look for agents: each probe takes 5 s at most.
const lookTimeout = 30 * time.Second

// reread is the gap after which Settings reads the connections anew.
const reread = 2 * time.Second

// fieldWidth is the width of the Ask group's controls, one under another.
const fieldWidth = 210

// settings is Settings › Agents. Main thread only; what it reads of agents
// it reads off the main thread and hands back through Host.Update.
type settings struct {
	a       *AddOn
	looking bool // agents are being probed
	looked  bool // they were, once
	agents  []gateway.Available
	rows    map[string]*connection // by agent ID, for the agents with a Connector
	models  map[string][]string    // the models offered, by agent ID
	// defaults is each Models agent's own default, for its pop-up.
	defaults map[string]string
	notice   string // why Choose… did nothing
	// askWith and model are the pop-ups' values, which a choice changes
	// after the frame that builds them: they outlive it.
	askWith, model string
	askWithAgent   string // the agent preference the Ask with control reflects
	// seen is the last frame that built the section: after a gap, as when
	// Settings opens again, the connections are read anew, as the user may
	// have changed them in Terminal meanwhile.
	seen time.Time

	// The Connect or Disconnect alert, for one agent.
	asking     bool
	askingID   string
	disconnect bool
}

// connection is the state of an agent's Connect, in its row.
type connection struct {
	state  gateway.Connection
	known  bool // state was read
	busy   bool
	failed bool
	exit   int  // the failed command's exit status, -1 if it gave none
	undo   bool // the failed command was a Disconnect
}

// found is what a look for agents read, handed to the main thread.
type found struct {
	agents   []gateway.Available
	conns    map[string]gateway.Connection
	models   map[string][]string
	defaults map[string]string
}

// look probes the agents, anew when again, and reads the connection of
// each found with a Connector and the models of each with Models.
func (s *settings) look(again bool) {
	s.looking = true
	gw, log, update := s.a.o.Gateway, s.a.o.Log, s.a.host.Update
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), lookTimeout)
		defer cancel()
		list := gw.Agents(ctx)
		if again {
			list = gw.Refresh(ctx)
		}
		f := found{agents: list, conns: map[string]gateway.Connection{}, models: map[string][]string{}, defaults: map[string]string{}}
		for _, av := range list {
			if !av.Probe.Usable() {
				continue
			}
			if c, ok := av.Agent.(gateway.Connector); ok {
				if conn, err := c.Connection(ctx); err != nil {
					log.Warn("agent connection", "agent", av.ID, "err", err)
				} else {
					f.conns[av.ID] = conn
				}
			}
			if m, ok := av.Agent.(gateway.Models); ok {
				def, err := m.DefaultModel(ctx)
				if err != nil {
					log.Warn("agent default model", "agent", av.ID, "err", err)
				}
				list, err := m.Models(ctx)
				if err != nil {
					log.Warn("agent models", "agent", av.ID, "err", err)
				}
				f.defaults[av.ID], f.models[av.ID] = def, list
			}
		}
		update(func() { s.found(f) })
	}()
}

// found takes what a look read.
func (s *settings) found(f found) {
	s.looking, s.looked = false, true
	s.agents, s.models, s.defaults = f.agents, f.models, f.defaults
	s.a.found = f.agents // what Settings finds, Ask uses
	s.rows = map[string]*connection{}
	for id, conn := range f.conns {
		s.rows[id] = &connection{state: conn, known: true}
	}
	for _, av := range f.agents {
		if _, ok := av.Agent.(gateway.Connector); ok && av.Probe.Usable() && s.rows[av.ID] == nil {
			s.rows[av.ID] = &connection{} // its state could not be read
		}
	}
}

// usable is the agents found that Ask can use.
func (s *settings) usable() []gateway.Available {
	return slices.DeleteFunc(slices.Clone(s.agents), func(av gateway.Available) bool { return !av.Probe.Usable() })
}

func (s *settings) agent(id string) (gateway.Available, bool) {
	i := slices.IndexFunc(s.agents, func(av gateway.Available) bool { return av.ID == id })
	if i < 0 {
		return gateway.Available{}, false
	}
	return s.agents[i], true
}

func (s *settings) view(c *ui.Context) {
	if !s.looking && (!s.looked || !s.seen.IsZero() && c.Now().Sub(s.seen) > reread) {
		s.look(false)
	}
	s.seen = c.Now()
	kit.SectionHeader(c, "Agents")
	usable := s.usable()
	switch {
	case !s.looked:
		kit.Group(c, func() {
			ui.Row(c).MinHeight(40).Padding(8, 12).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Spinner(c)
				ui.Text(c, "Looking for agents…").TextColor(kit.Colors(c).Secondary)
			})
		})
		ui.Box(c).Height(20)
	case len(usable) == 0:
		s.none(c)
	default:
		s.agentList(c, usable)
		s.ask(c, usable)
	}
	s.alerts(c)
}

// agentList is a row for each agent found that Ask can use: where it was
// found, and its connection for use outside Capy.
func (s *settings) agentList(c *ui.Context, usable []gateway.Available) {
	rows := make([]func(), len(usable))
	for i, av := range usable {
		rows[i] = func() { s.agentRow(c, av, s.rows[av.ID]) }
	}
	kit.Group(c, rows...)
	note := "Ask uses these agents in Capy, connected or not. Connect one to use your recordings in its own app too: " +
		"it can then read every session in Capy, rename sessions, edit recaps and move sessions to Recently Deleted; " +
		"every change can be undone."
	if old := s.tooOld(); old != "" {
		note = old + " " + note
	}
	kit.Footnote(c, note)
}

// ask is the group choosing the agent Ask asks, and its model.
func (s *settings) ask(c *ui.Context, usable []gateway.Available) {
	p := s.a.o.Prefs.Get()
	chosen := usable[0]
	if i := slices.IndexFunc(usable, func(av gateway.Available) bool { return av.ID == p.Agent }); i >= 0 {
		chosen = usable[i]
	}
	kit.SectionHeader(c, "Ask and Polish")
	rows := []func(){func() {
		kit.SettingRow(c, "Use", func() {
			if len(usable) == 1 { // nothing to choose
				ui.Text(c, chosen.Name).TextColor(kit.Colors(c).Secondary).SingleLine()
				return
			}
			names := make([]string, len(usable))
			for i, av := range usable {
				names[i] = av.Name
			}
			if s.askWithAgent != chosen.ID || !slices.Contains(names, s.askWith) {
				s.askWith = chosen.Name
				s.askWithAgent = chosen.ID
			}
			ui.Select(c, &s.askWith, names).Label("Use").Width(fieldWidth)
			if i := slices.Index(names, s.askWith); usable[i].ID != chosen.ID {
				s.a.setAgent(usable[i].ID)
			}
		})
	}}
	if _, ok := chosen.Agent.(gateway.Models); ok {
		rows = append(rows, func() { s.modelRow(c, chosen, p) })
	}
	kit.Group(c, rows...)
	ui.Box(c).Height(20)
}

// modelRow chooses the model of an agent that offers a choice: its own
// default, "From Codex settings", or one the user picks.
func (s *settings) modelRow(c *ui.Context, av gateway.Available, p prefs.Prefs) {
	from := "From " + programName(av.Info) + " settings"
	options := []string{from}
	for _, m := range s.models[av.ID] {
		if prefs.ValidModel(m) && !slices.Contains(options, m) {
			options = append(options, m)
		}
	}
	saved := from
	if m := p.Models[av.ID]; m != "" {
		saved = m
		if !slices.Contains(options, m) {
			options = append(options, m)
		}
	}
	if !slices.Contains(options, s.model) {
		s.model = saved
	}
	kit.SettingRow(c, "Model", func() {
		ui.Select(c, &s.model, options).Label("Model").Width(fieldWidth)
	})
	if s.model != saved {
		model := s.model
		if model == from {
			model = ""
		}
		s.save(func(p *prefs.Prefs) {
			if p.Models == nil {
				p.Models = map[string]string{}
			}
			p.Models[av.ID] = model
		})
		if s.a.o.Prefs.Get().Models[av.ID] != model {
			s.model = saved // refused: show what is kept, and try no more
		}
	}
}

// save changes agent.json, logging a failure: what the user changed stays
// shown, and is kept for the next change.
func (s *settings) save(change func(p *prefs.Prefs)) {
	if err := s.a.o.Prefs.Update(change); err != nil {
		s.a.o.Log.Warn("save agent preferences", "err", err)
	}
}

// agentRow is an agent found: its name and where, then Connect… or its
// connection, when row is not nil.
func (s *settings) agentRow(c *ui.Context, av gateway.Available, row *connection) {
	p := kit.Colors(c)
	sub := foundText(av)
	if row != nil && row.known && row.state == gateway.Moved && !row.busy && !row.failed {
		sub = "Capy is no longer where you connected it from"
	}
	ui.Row(c).Key(av.ID).MinHeight(44).Padding(8, 12).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Column(c).Grow(1).Basis(0).Gap(2).Children(func() {
			ui.Text(c, av.Name).SingleLine()
			ui.Text(c, sub).FontSize(12).TextColor(p.Secondary)
		})
		if row == nil {
			return // it cannot be connected
		}
		button := func(label string, primary bool) bool {
			newButton := ui.Button
			if primary {
				newButton = ui.PrimaryButton
			}
			return newButton(c, label).Label(label+" "+av.Name).Height(24).Padding(0, 12).Clicked()
		}
		switch {
		case row.busy:
			ui.Spinner(c).Label("Working")
		case row.failed:
			what := "connect"
			if row.undo {
				what = "disconnect"
			}
			status := "Couldn’t " + what
			if row.exit >= 0 {
				status += " (exit " + strconv.Itoa(row.exit) + ")"
			}
			ui.Text(c, status).TextColor(p.Danger).SingleLine()
			if button("Try Again", false) {
				s.run(av, row, row.undo)
			}
		case row.known && row.state == gateway.Connected:
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				kit.CheckBadge(c, p.Success)
				ui.Text(c, "Connected").TextColor(p.Secondary).SingleLine()
			})
			if button("Disconnect", false) {
				s.asking, s.askingID, s.disconnect = true, av.ID, true
			}
		case row.known && row.state == gateway.Moved:
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				ui.Icon(c, kit.WarnIcon).Size(15, 15).TextColor(ui.Hex("#e8930c"))
				ui.Text(c, "Capy has moved").TextColor(p.Secondary).SingleLine()
			})
			if button("Reconnect", true) {
				s.asking, s.askingID, s.disconnect = true, av.ID, false
			}
		default:
			if button("Connect…", false) {
				s.asking, s.askingID, s.disconnect = true, av.ID, false
			}
		}
	})
}

// alerts asks before connecting or disconnecting an agent.
func (s *settings) alerts(c *ui.Context) {
	av, ok := s.agent(s.askingID)
	row := s.rows[s.askingID]
	if !s.asking || !ok || row == nil {
		s.asking = false
		return
	}
	conn, ok := av.Agent.(gateway.Connector)
	if !ok {
		s.asking = false
		return
	}
	if s.disconnect {
		if commandAlert(c, &s.asking, "Disconnect "+av.Name+"?",
			"Capy will run this command. "+av.Name+" will no longer see your sessions.",
			commandLine(conn.DisconnectCommand()), "Disconnect") {
			s.run(av, row, true)
		}
		return
	}
	subject := av.AppsSubject
	if subject == "" {
		subject = av.Name
	}
	// Every command it runs shows: a Reconnect removes the old entry first.
	var lines []string
	if row.known && row.state == gateway.Moved {
		lines = append(lines, commandLine(conn.DisconnectCommand()))
	}
	for _, argv := range conn.ConnectCommands() {
		lines = append(lines, commandLine(argv))
	}
	message, command := "Capy will run this command. ", strings.Join(lines, "\n")
	if len(lines) > 1 {
		message = "Capy will run these commands. "
	}
	if commandAlert(c, &s.asking, "Connect "+av.Name+" to Capy?",
		message+subject+" will be able to read every session in Capy.", command, "Connect") {
		s.run(av, row, false)
	}
}

// run connects or disconnects an agent, off the main thread: a Reconnect
// of a moved Capy disconnects first, so the agent's CLI records the
// running one.
func (s *settings) run(av gateway.Available, row *connection, disconnect bool) {
	conn := av.Agent.(gateway.Connector)
	reconnect := !disconnect && row.known && row.state == gateway.Moved
	row.busy, row.failed, row.undo = true, false, disconnect
	log, update := s.a.o.Log, s.a.host.Update
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), lookTimeout)
		defer cancel()
		var err error
		switch {
		case disconnect:
			err = conn.Disconnect(ctx)
		case reconnect:
			if err = conn.Disconnect(ctx); err == nil {
				err = conn.Connect(ctx)
			}
		default:
			err = conn.Connect(ctx)
		}
		state, known := gateway.NotConnected, false
		if err == nil {
			var cerr error
			state, cerr = conn.Connection(ctx)
			known = cerr == nil
		} else {
			log.Warn("agent connect", "agent", av.ID, "disconnect", disconnect, "err", err)
		}
		update(func() {
			row.busy = false
			if err != nil {
				row.failed, row.exit = true, -1
				if ec, ok := errors.AsType[gateway.ExitCoder](err); ok {
					row.exit = ec.ExitCode()
				}
				return
			}
			row.state, row.known = state, known
		})
	}()
}

// none is the group shown when no agent can be used: how to get one, the
// ones too old, and where to look for one installed elsewhere.
func (s *settings) none(c *ui.Context) {
	p := kit.Colors(c)
	rows := []func(){func() {
		ui.Row(c).Padding(14, 12).Gap(12).AlignItems(ui.Start).Children(func() {
			ui.Icon(c, kit.CaptionsIcon).Size(20, 20).TextColor(p.Secondary).Margin(1, 0, 0, 0)
			ui.Column(c).Grow(1).Basis(0).Gap(2).AlignItems(ui.Start).Children(func() {
				ui.Text(c, "No agent found").FontWeight(600)
				ui.Text(c, "Ask works with an agent you already use: Claude Code, or the ChatGPT app with its built-in Codex. "+
					"Install one and sign in, then click Look Again.").FontSize(12).TextColor(p.Secondary)
				ui.Row(c).Gap(8).Margin(8, 0, 0, 0).Children(func() {
					for _, av := range s.agents {
						if av.Download == "" {
							continue
						}
						get := ui.Button(c, "").Label("Get "+av.Name).Height(24).Padding(0, 12).Gap(4)
						if get.Clicked() {
							c.OpenURL(av.Download)
						}
						get.Children(func() {
							ui.Text(c, "Get "+av.Name).SingleLine()
							ui.Text(c, "↗").TextColor(p.Secondary)
						})
					}
				})
			})
		})
	}}
	if old := s.tooOld(); old != "" {
		rows = append(rows, func() {
			ui.Text(c, old).FontSize(12).Padding(8, 12)
		})
	}
	rows = append(rows, func() {
		kit.SettingRow(c, "Installed somewhere else?", func() {
			if ui.Button(c, "Choose…").Height(24).Padding(0, 12).Disabled(s.looking).Clicked() {
				s.choose()
			}
			if s.looking {
				ui.Spinner(c).Label("Looking")
			} else if ui.Button(c, "Look Again").Height(24).Padding(0, 12).Clicked() {
				s.look(true)
			}
		})
	})
	kit.Group(c, rows...)
	if s.notice != "" {
		kit.Footnote(c, s.notice)
	} else {
		ui.Box(c).Height(20)
	}
}

// choose asks for an agent's program installed where Capy does not look,
// keeps it as that agent's path, which is tried first, and looks again.
func (s *settings) choose() {
	choose, update := s.a.o.ChooseFile, s.a.host.Update
	go func() {
		path, err := choose()
		update(func() {
			s.notice = ""
			if err != nil || path == "" {
				return
			}
			i := slices.IndexFunc(s.agents, func(av gateway.Available) bool { return av.Command == filepath.Base(path) })
			if i < 0 {
				var names []string
				for _, av := range s.agents {
					names = append(names, av.Command)
				}
				s.notice = "Choose the " + strings.Join(names, " or ") + " program."
				return
			}
			id := s.agents[i].ID
			s.save(func(p *prefs.Prefs) {
				if p.Paths == nil {
					p.Paths = map[string]string{}
				}
				p.Paths[id] = filepath.Clean(path)
			})
			s.look(true)
		})
	}()
}

// tooOld says which agents found are too old for Ask, and what they need.
func (s *settings) tooOld() string {
	var out []string
	for _, av := range s.agents {
		if av.Probe.Found && av.Probe.TooOld != "" {
			out = append(out, av.Name+" "+shortVersion(av.Probe.Version)+" is too old for Ask. Update to "+
				shortVersion(av.Probe.TooOld)+" or later.")
		}
	}
	return strings.Join(out, " ")
}

// foundText says which version of an agent was found where: "2.1.296 in
// ~/.local/bin", "Codex 0.162 in ChatGPT.app".
func foundText(av gateway.Available) string {
	text := shortVersion(av.Probe.Version) + " in " + where(av.Probe.Path)
	if av.Program != "" {
		text = av.Program + " " + text
	}
	return text
}

// shortVersion is a version without a pre-release part or a trailing
// ".0": "0.162.0-alpha.17.2" is "0.162".
func shortVersion(v string) string {
	v, _, _ = strings.Cut(v, "-")
	return strings.TrimSuffix(v, ".0")
}

// where names the folder of an agent's program for people: the app bundle
// it is in, or its folder with the home folder as ~.
func where(path string) string {
	for dir := filepath.Dir(path); dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		if strings.HasSuffix(dir, ".app") {
			return filepath.Base(dir)
		}
	}
	dir := filepath.Dir(path)
	if home, err := os.UserHomeDir(); err == nil && (dir == home || strings.HasPrefix(dir, home+"/")) {
		return "~" + strings.TrimPrefix(dir, home)
	}
	return dir
}

// programName is what the agent's settings are called after: "Codex" of
// ChatGPT.
func programName(info gateway.Info) string {
	if info.Program != "" {
		return info.Program
	}
	return info.Name
}

// commandLine is argv as a person would type it, each argument with a
// space or a quote single-quoted.
func commandLine(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " '\"\\$`") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}

// commandAlert is an alert, as AlertDialog's, that shows the exact command
// confirming runs, in a box. It reports whether confirm was clicked.
func commandAlert(c *ui.Context, open *bool, title, message, command, confirm string) bool {
	chosen := false
	ui.DialogBase(c, open, func(backdrop, panel ui.Element) {
		p := kit.Colors(c)
		backdrop.Background(ui.RGBA(0, 0, 0, 0.4))
		panel.Width(340).Padding(20).Gap(12).Radius(10).Background(p.Group).Shadow(0, 10, 30, 0, ui.RGBA(0, 0, 0, 0.3)).
			AlignItems(ui.Stretch).Label(title).Children(func() {
			ui.Text(c, title).Bold().FontSize(14)
			ui.Text(c, message).TextColor(p.Secondary)
			ui.Text(c, command).Font("Menlo").FontSize(11).Selectable().Padding(8, 10).Radius(6).Background(p.Fill2)
			ui.Row(c).Gap(8).Justify(ui.End).Margin(8, 0, 0, 0).Children(func() {
				if ui.Button(c, "Cancel").Clicked() {
					*open = false
				}
				if ui.PrimaryButton(c, confirm).AutoFocus().Clicked() {
					*open, chosen = false, true
				}
			})
		})
	})
	return chosen
}
