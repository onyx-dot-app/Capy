// Package prefs is agent.json: what the user chose for Agent, in its
// folder of Capy's data directory.
package prefs

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
)

// validID is a gateway agent's ID, and validModel a model's name: they
// reach an agent's command line, as -m <model>.
var (
	validID    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	validModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
)

// ValidModel reports whether a model's name is one agent.json keeps.
func ValidModel(m string) bool { return validModel.MatchString(m) }

// Bounds of agent.json.
const (
	// InspectorWidth and LivePanelWidth are the widths Ask's panes first
	// open at; the user resizes either between PaneMin and PaneMax.
	InspectorWidth = 320
	LivePanelWidth = 320
	PaneMin        = 280
	PaneMax        = 720
)

// Prefs is agent.json: what the user chose for Agent. No secret is kept:
// the agents keep their own sign-in.
type Prefs struct {
	// Agent is the agent Ask asks for a new thread, by gateway ID; "" for
	// the first found.
	Agent string `json:"agent"`
	// Paths is the path the user chose for an agent, by gateway ID.
	Paths map[string]string `json:"paths,omitempty"`
	// Models is the model the user chose for an agent that offers a
	// choice, by gateway ID; none for the one in the agent's own settings.
	Models map[string]string `json:"models,omitempty"`
	// Allowed is the agents the user allowed to receive questions.
	Allowed []string `json:"allowed,omitempty"`
	// Connected is the agents connected through Settings whose own CLI
	// cannot say so without starting Capy mcp, as their last Connect or
	// Disconnect left them: Claude Code.
	Connected []string `json:"connected,omitempty"`
	// InspectorOpen and InspectorWidth are the inspector's state.
	InspectorOpen  bool    `json:"inspectorOpen"`
	InspectorWidth float32 `json:"inspectorWidth"`
	// LivePanelWidth is that of the caption bar's panel.
	LivePanelWidth float32 `json:"livePanelWidth"`
	// Polish is Polish with agent, in Settings › Highlights.
	Polish bool `json:"polish"`
}

// Default is Agent as first launched.
func Default() Prefs { return Prefs{InspectorWidth: InspectorWidth, LivePanelWidth: LivePanelWidth} }

// normalized drops what an older or edited file holds that this build
// cannot use: unknown IDs, relative paths, widths out of bounds.
func (p Prefs) normalized() Prefs {
	tag := validID.MatchString
	ids := func(list []string) []string {
		var out []string
		for _, s := range list {
			if tag(s) && !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
		return out
	}
	if p.Agent != "" && !tag(p.Agent) {
		p.Agent = ""
	}
	paths := map[string]string{}
	for id, path := range p.Paths {
		if tag(id) && filepath.IsAbs(path) && filepath.Clean(path) == path {
			paths[id] = path
		}
	}
	p.Paths = paths
	if len(paths) == 0 {
		p.Paths = nil
	}
	models := map[string]string{}
	for id, m := range p.Models {
		if tag(id) && validModel.MatchString(m) {
			models[id] = m
		}
	}
	p.Models = models
	if len(models) == 0 {
		p.Models = nil
	}
	p.Allowed, p.Connected = ids(p.Allowed), ids(p.Connected)
	if p.InspectorWidth < PaneMin || p.InspectorWidth > PaneMax {
		p.InspectorWidth = InspectorWidth
	}
	if p.LivePanelWidth < PaneMin || p.LivePanelWidth > PaneMax {
		p.LivePanelWidth = LivePanelWidth
	}
	return p
}

// File is agent.json, read once and saved on every change. It is
// safe for concurrent use, as probes and connects run off the main
// thread.
type File struct {
	path string
	mu   sync.Mutex
	p    Prefs
}

// Load reads path: a missing or unreadable file gives the defaults,
// as does a missing setting.
func Load(path string) *File { return &File{path: path, p: Read(path)} }

// Read reads path as Load does, without keeping it: for a process that
// reads what another changed.
func Read(path string) Prefs {
	p := Default()
	f, err := os.Open(path)
	if err != nil {
		return p
	}
	defer f.Close()
	// Far larger than agent.json grows: a bigger file is not one of Capy's.
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil || len(b) > maxFile || json.Unmarshal(b, &p) != nil {
		return Default()
	}
	return p.normalized()
}

// maxFile is the largest agent.json Read takes.
const maxFile = 64 << 10

// Get returns a copy of the preferences.
func (f *File) Get() Prefs {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.p
	p.Paths, p.Models = maps.Clone(p.Paths), maps.Clone(p.Models)
	p.Allowed, p.Connected = slices.Clone(p.Allowed), slices.Clone(p.Connected)
	return p
}

// Update changes the preferences with change, normalized, and saves them
// atomically, 0600.
func (f *File) Update(change func(p *Prefs)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.p
	p.Paths, p.Models = maps.Clone(p.Paths), maps.Clone(p.Models)
	p.Allowed, p.Connected = slices.Clone(p.Allowed), slices.Clone(p.Connected)
	change(&p)
	p = p.normalized()
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	// A new file of a name no other process can have planted, 0600, then
	// renamed over agent.json.
	tmp, err := os.CreateTemp(filepath.Dir(f.path), "agent.json.*")
	if err != nil {
		return Pathless(err)
	}
	defer os.Remove(tmp.Name()) // once renamed, there is nothing to remove
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return Pathless(err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return Pathless(err)
	}
	if err := tmp.Close(); err != nil {
		return Pathless(err)
	}
	if err := os.Rename(tmp.Name(), f.path); err != nil {
		return Pathless(err)
	}
	f.p = p
	return nil
}

// Pathless is err without the path it names, which may hold the user's
// name for their home folder.
func Pathless(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err
	}
	if le, ok := errors.AsType[*os.LinkError](err); ok {
		return le.Err
	}
	return err
}
