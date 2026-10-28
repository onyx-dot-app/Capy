package ui

import (
	"slices"

	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/polish"
	"github.com/onyx-dot-app/Capy/internal/agent/prefs"
	"github.com/onyx-dot-app/Capy/internal/features"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
)

// polishable reports whether Polish with agent shows: Agent and
// Highlights on, and polish wired.
func (a *AddOn) polishable() bool {
	return a.on() && a.o.Polish != nil && a.o.Gate.Enabled(features.Highlights)
}

// Highlighted implements addon.AddOn: a pass of polish for the session,
// which Polish runs only while it is on.
func (a *AddOn) Highlighted(sessionID string, final bool) {
	if a.o.Polish != nil {
		a.o.Polish.Notify(sessionID, final)
	}
}

// Polished shows the polish a pass saved: Polish's Saved.
func (a *AddOn) Polished(sessionID string) {
	if a.host != nil {
		a.host.Polished(sessionID)
	}
}

// PolishStopped says, once, that polish stopped for a session: Polish's
// Stopped. Settings › Highlights says why until Try Again.
func (a *AddOn) PolishStopped(s polish.Stop) {
	if a.host == nil {
		return
	}
	name := a.agentName(s.Agent)
	a.host.Update(func() {
		a.host.Toast(ui.Toast{ID: "polish-stopped", Title: name + " stopped polishing highlights",
			Description: stopText(s, name), Type: "error"})
	})
}

// PolishOn reports whether polish runs: its switch, the features, and
// the user's consent for the agent it uses. Any goroutine.
func (a *AddOn) PolishOn(agentID string) bool {
	if !a.polishable() || !a.o.Prefs.Get().Polish {
		return false
	}
	return a.o.Gateway.Allowed(agentID)
}

// agentName is the name of agent id, the ID itself when it is not
// registered.
func (a *AddOn) agentName(id string) string {
	if ag, ok := a.o.Gateway.Agent(id); ok {
		return ag.Info().Name
	}
	if id == "" {
		return "Your agent"
	}
	return id
}

// stopText is why polish stopped, as the agent named name says it.
func stopText(s polish.Stop, name string) string {
	switch s.Reason {
	case polish.SignedOut:
		return "Sign in to " + name + ", then try again."
	case polish.UsageLimit:
		if !s.ResetAt.IsZero() {
			return name + " says you’ve reached your usage limit. It resets at " + s.ResetAt.Local().Format("3:04 PM") + "."
		}
		return name + " says you’ve reached your usage limit."
	case polish.Unavailable:
		return name + " isn’t found. Choose another in Agents below."
	}
	return name + " didn’t answer as Capy asked, three times in a row."
}

// polishChosen is the agent polish uses, as Ask does: the one chosen in
// Settings › Agents when found, else the first found; ok false when none
// is.
func (a *AddOn) polishChosen() (gateway.Available, bool) {
	usable := a.usable()
	if len(usable) == 0 {
		return gateway.Available{}, false
	}
	if i := slices.IndexFunc(usable, func(av gateway.Available) bool { return av.ID == a.o.Prefs.Get().Agent }); i >= 0 {
		return usable[i], true
	}
	return usable[0], true
}

// HighlightsSettings implements addon.AddOn: Polish with agent, last in
// Settings › Highlights.
func (a *AddOn) HighlightsSettings(c *ui.Context) {
	if !a.polishable() {
		return
	}
	p := a.o.Prefs.Get()
	chosen, found := a.polishChosen()
	// An agent chosen since without the user's consent leaves polish off
	// until they allow it: turning it on asks.
	allowed := found && a.o.Gateway.Allowed(chosen.ID)
	on := p.Polish && allowed
	caption := "Polish needs an agent. Set one up in Agents below."
	switch {
	case p.Polish && found && !allowed:
		caption = "Turn this on to allow " + chosen.Name + " to polish highlights."
	case found:
		caption = chosen.Name + " writes a sentence for each highlight, and finds debates."
		if s := chosen.Info.Sends; s != nil {
			caption += " The turns around each highlight go to " + s.Provider + " through " + s.Account + "."
		}
	case p.Polish && p.Agent != "":
		caption = a.agentName(p.Agent) + " isn’t found. Choose another in Agents below."
	}
	pal := kit.Colors(c)
	kit.Group(c, func() {
		ui.Row(c).MinHeight(40).Padding(8, 12).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Grow(1).Basis(0).Gap(1).Children(func() {
				ui.Text(c, "Polish with agent")
				ui.Text(c, caption).FontSize(11).TextColor(pal.Secondary)
			})
			if ui.Switch(c, &on).Label("Polish with agent").Disabled(!found).Changed() {
				a.setPolish(on, chosen)
			}
		})
	})
	if s, ok := a.o.Polish.LastStop(); ok && p.Polish {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Padding(6, 12, 0).Children(func() {
			ui.Text(c, stopText(s, a.agentName(s.Agent))).FontSize(11).TextColor(pal.Danger).Grow(1).Basis(0)
			if ui.Button(c, "Try Again").Clicked() {
				a.o.Polish.Retry(s.SessionID)
			}
		})
	}
	a.polishDialog(c)
}

// setPolish turns Polish with agent on, asking the agent's consent first
// when it sends what it reads off this Mac and the user has not allowed
// it yet, or off.
func (a *AddOn) setPolish(on bool, chosen gateway.Available) {
	if on && !a.o.Gateway.Allowed(chosen.ID) {
		a.polishConsent, a.polishAsking = chosen.ID, true
		return
	}
	a.savePolish(on)
}

func (a *AddOn) savePolish(on bool) {
	if err := a.o.Prefs.Update(func(p *prefs.Prefs) { p.Polish = on }); err != nil {
		a.o.Log.Warn("save agent prefs", "err", err)
	}
	if !on {
		a.o.Polish.Halt()
	}
}

// polishDialog asks the agent's consent to polish, once for each agent,
// as Ask's first question does.
func (a *AddOn) polishDialog(c *ui.Context) {
	if a.polishConsent == "" {
		return
	}
	id := a.polishConsent
	name := a.agentName(id)
	message := "Capy will send the turns around each highlight of your recordings to " + name
	if ag, ok := a.o.Gateway.Agent(id); ok && ag.Info().Sends != nil {
		message += ", which sends them to " + ag.Info().Sends.Provider + " through " + ag.Info().Sends.Account
	}
	message += ", while you record and after. Each update uses your plan.\n\nYou’ll be asked once for each agent."
	switch ui.AlertDialog(c, &a.polishAsking, "Polish with "+name+"?", message, "Cancel", "Allow") {
	case 0:
		a.polishConsent = ""
	case 1:
		a.polishConsent = ""
		if err := a.o.Gateway.Allow(id); err != nil {
			a.o.Log.Warn("allow agent", "agent", id, "err", err)
			return
		}
		a.savePolish(true)
	}
	if !a.polishAsking {
		a.polishConsent = ""
	}
}
