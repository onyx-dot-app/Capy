// Package codex is the gateway's adapter for ChatGPT: its app, the Codex
// CLI and Codex in an editor share one Codex, configured in
// ~/.codex/config.toml.
package codex

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"

	"github.com/BurntSushi/toml"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/cli"
)

// Spec is where Codex is found: the one bundled with the ChatGPT app
// first, as the npm Codex 0.144 rejects ChatGPT sign-in for current
// models. Minimum is provisional: step 9 of the Agent plan sets it from
// the versions make e2e-agents passes with.
var Spec = cli.Spec{
	Name: "codex",
	Known: []string{
		"/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex",
		"/opt/homebrew/bin/codex",
	},
	VersionArgs: []string{"--version"},
	Minimum:     "0.162.0",
}

// Options are what the adapter depends on. Env.Home holds .codex.
type Options struct {
	Env cli.Env
	// Self is Capy's executable, which `<Self> mcp` serves Capy's tools.
	Self string
	// Chosen is the path the user chose for codex, or nil.
	Chosen func() string
	// WorkDir is the empty folder codex runs in, so that no repository's
	// AGENTS.md or config loads.
	WorkDir string
}

type agent struct {
	prog    *cli.Program
	self    string
	home    string
	workDir string
}

// New returns ChatGPT's adapter, a gateway.Connector, gateway.HandOff and
// gateway.Models too.
func New(o Options) gateway.Agent {
	return &agent{prog: &cli.Program{Env: o.Env, Spec: Spec, Chosen: o.Chosen}, self: o.Self, home: o.Env.Home, workDir: o.WorkDir}
}

func (a *agent) Info() gateway.Info {
	return gateway.Info{
		ID: "codex", Name: "ChatGPT",
		Sends:       &gateway.Recipient{Provider: "OpenAI", Account: "your ChatGPT account"},
		Command:     "codex",
		Program:     "Codex",
		AppsSubject: "ChatGPT, the Codex CLI and Codex in your editor",
		Download:    "https://chatgpt.com/download",
	}
}

func (a *agent) Probe(ctx context.Context) gateway.Probe { return a.prog.Probe(ctx) }

func (a *agent) connectArgs() []string { return []string{"mcp", "add", "capy", "--", a.self, "mcp"} }

func (a *agent) ConnectCommands() [][]string {
	return [][]string{append([]string{a.prog.Path()}, a.connectArgs()...)}
}

func (a *agent) Connect(ctx context.Context) error {
	_, err := a.prog.Run(ctx, a.connectArgs()...)
	return err
}

// disconnectArgs remove Capy's server from Codex's config.
var disconnectArgs = []string{"mcp", "remove", "capy"}

func (a *agent) DisconnectCommand() []string {
	return append([]string{a.prog.Path()}, disconnectArgs...)
}

func (a *agent) Disconnect(ctx context.Context) error {
	_, err := a.prog.Run(ctx, disconnectArgs...)
	return err
}

// server is the part of `codex mcp get capy --json` Connection reads.
type server struct {
	Transport struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	} `json:"transport"`
}

// Connection reads Codex's capy server: none (exit 1), this Capy, or
// another one, as a Capy that has moved.
func (a *agent) Connection(ctx context.Context) (gateway.Connection, error) {
	out, err := a.prog.Run(ctx, "mcp", "get", "capy", "--json")
	if ee, ok := errors.AsType[*cli.ExitError](err); ok && ee.Code == 1 {
		return gateway.NotConnected, nil
	}
	if err != nil {
		return gateway.NotConnected, err
	}
	var s server
	if err := json.Unmarshal(out, &s); err != nil {
		return gateway.NotConnected, errors.New("codex: unreadable mcp get output")
	}
	if filepath.Clean(s.Transport.Command) == filepath.Clean(a.self) && slices.Equal(s.Transport.Args, []string{"mcp"}) {
		return gateway.Connected, nil
	}
	return gateway.Moved, nil
}

// DefaultModel is the model of Codex's own settings, "" when they name
// none. --ignore-user-config, which Ask runs Codex with, leaves no usable
// default, so Ask passes this one.
func (a *agent) DefaultModel(context.Context) (string, error) {
	var cfg struct {
		Model string `toml:"model"`
	}
	_, err := toml.DecodeFile(filepath.Join(a.home, ".codex", "config.toml"), &cfg)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case err != nil:
		// TOML errors quote the file, which may hold secrets.
		if _, ok := errors.AsType[*fs.PathError](err); ok {
			return "", errors.New("codex: config.toml is unreadable")
		}
		return "", errors.New("codex: config.toml is malformed")
	}
	return cfg.Model, nil
}

// model is a model of the catalog `codex debug models --bundled` prints.
type model struct {
	Slug       string `json:"slug"`
	Visibility string `json:"visibility"` // "list" for those Codex offers
	Priority   int    `json:"priority"`
}

// Models are the models Codex lists, in its order: those of the catalog
// bundled with it, which `codex debug models --bundled` prints without
// going to the network, with the default first when the catalog lacks it.
// Codex has no other listing; when this one fails, the default alone.
func (a *agent) Models(ctx context.Context) ([]string, error) {
	def, err := a.DefaultModel(ctx)
	if err != nil {
		return nil, err
	}
	var models []string
	if out, err := a.prog.Run(ctx, "debug", "models", "--bundled"); err == nil {
		var c struct{ Models []model }
		if json.Unmarshal(out, &c) == nil {
			listed := slices.DeleteFunc(c.Models, func(m model) bool { return m.Visibility != "list" || m.Slug == "" })
			slices.SortStableFunc(listed, func(x, y model) int { return cmp.Compare(x.Priority, y.Priority) })
			for _, m := range listed {
				models = append(models, m.Slug)
			}
		}
	}
	if def != "" && !slices.Contains(models, def) {
		models = append([]string{def}, models...)
	}
	return models, nil
}
