// Package ui is Agent's screens in the views' slots: Settings › Agents,
// and Ask: its inspector, the caption bar's panel, its export. It reaches
// agents only through the gateway's contract, never an adapter, and asks
// the feature gate on every slot: with Agent off, it shows nothing.
package ui

import (
	"context"
	"log/slog"
	"math"
	"sync/atomic"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/agent/ask"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/polish"
	"github.com/onyx-dot-app/Capy/internal/agent/prefs"
	"github.com/onyx-dot-app/Capy/internal/features"
	"github.com/onyx-dot-app/Capy/internal/views/addon"
)

// Options are what Agent's screens need.
type Options struct {
	Gate    features.Gate
	Gateway *gateway.Gateway
	// Ask is Ask; nil shows no Ask.
	Ask *ask.Ask
	// Polish is Highlight polish; nil shows no Polish with agent.
	Polish *polish.Polish
	Prefs  *prefs.File
	Log    *slog.Logger
	// ChooseFile asks the user for a program, for Choose…; nil asks with
	// the system's open panel.
	ChooseFile func() (string, error)
	// OpenApp opens an app by name, for Open Terminal and Open ChatGPT;
	// nil opens it as Finder would.
	OpenApp func(name string) error
}

// AddOn fills the views' slots with Agent's screens.
type AddOn struct {
	o        Options
	host     addon.Host
	settings settings
	// panel is the inspector's Ask, live the caption bar's: the two share
	// data.
	data        *data
	panel, live *panel
	// liveOpen is whether the caption bar's panel is open; liveKey
	// whether ⌥⌘K is Capy's.
	liveOpen, liveKey bool
	// found is the agents and what their last probe found.
	found []gateway.Available
	// inspectorSize and liveSize are the widths of the inspector and the
	// caption bar's panel.
	inspectorSize, liveSize paneSize
	// polishConsent is the agent Polish with agent's consent dialog asks
	// about, while polishAsking keeps it open.
	polishConsent string
	polishAsking  bool
}

var _ addon.AddOn = (*AddOn)(nil)

// New returns Agent's screens.
func New(o Options) *AddOn {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.ChooseFile == nil {
		o.ChooseFile = chooseFile
	}
	if o.OpenApp == nil {
		o.OpenApp = openApp
	}
	a := &AddOn{o: o, inspectorSize: paneSize{field: inspectorField}, liveSize: paneSize{field: liveField}}
	a.settings.a = a
	a.data = newData()
	a.panel = newPanel(a, a.data, false)
	a.live = newPanel(a, a.data, true)
	return a
}

// atomicWidth is a width read off the main thread.
type atomicWidth struct{ bits atomic.Uint32 }

func (w *atomicWidth) Store(v float32) { w.bits.Store(math.Float32bits(v)) }
func (w *atomicWidth) Load() float32   { return math.Float32frombits(w.bits.Load()) }

// on reports whether the gate shows Agent now.
func (a *AddOn) on() bool { return a.o.Gate.Enabled(features.Agent) }

// Start implements addon.AddOn: with Agent on, it looks for the agents.
func (a *AddOn) Start(h addon.Host) {
	a.host = h
	if a.on() { // with Agent off, Capy takes no other app's ⌥⌘K
		a.lookForAgents()
		a.registerLive()
	}
}

// Export implements addon.AddOn: the session's Ask threads, as "## Ask".
func (a *AddOn) Export(s addon.Session) []string {
	if !a.on() || a.o.Ask == nil {
		return nil
	}
	sections, err := a.o.Ask.Export(context.Background(), s.ID)
	if err != nil {
		a.o.Log.Warn("export ask threads", "err", err)
		return nil
	}
	return sections
}

// Settings implements addon.AddOn: Settings › Agents.
func (a *AddOn) Settings(c *ui.Context) {
	if !a.on() {
		return
	}
	a.settings.view(c)
}

// chooseFile asks for a program in the system's open panel, inside app
// bundles too, where ChatGPT keeps its Codex.
func chooseFile() (string, error) {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Parent: mygo.FocusedWindow(), Title: "Choose an Agent",
		ButtonLabel: "Choose", TreatPackagesAsDirectories: true, ShowHiddenFiles: true})
	if err != nil || len(paths) == 0 {
		return "", err
	}
	return paths[0], nil
}
