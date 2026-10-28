// Package claudecode is the gateway's adapter for Claude Code, the
// claude command-line tool.
package claudecode

import (
	"context"
	"errors"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/cli"
)

// Spec is where Claude Code is found. Minimum is provisional: step 9 of
// the Agent plan sets it from the versions make e2e-agents passes with.
var Spec = cli.Spec{
	Name: "claude",
	Known: []string{
		"~/.local/bin/claude",
		"~/.claude/local/claude",
		"/opt/homebrew/bin/claude",
		"/usr/local/bin/claude",
	},
	VersionArgs: []string{"--version"},
	Minimum:     "2.1.0",
}

// Memory keeps whether Claude Code is connected, as its last Connect or
// Disconnect left it: `claude mcp get` starts the server to check it, so
// Capy does not ask.
type Memory interface {
	Connected() bool
	SetConnected(bool) error
}

// Options are what the adapter depends on.
type Options struct {
	Env cli.Env
	// Self is Capy's executable, which `<Self> mcp` serves Capy's tools.
	Self string
	// Chosen is the path the user chose for claude, or nil.
	Chosen func() string
	// Memory keeps the connection; nil keeps none.
	Memory Memory
	// WorkDir is the empty folder claude runs in, so that no repository's
	// CLAUDE.md, .mcp.json or hooks load.
	WorkDir string
}

type agent struct {
	prog    *cli.Program
	self    string
	memory  Memory
	workDir string
}

// New returns Claude Code's adapter, a gateway.Connector and
// gateway.HandOff too.
func New(o Options) gateway.Agent {
	return &agent{prog: &cli.Program{Env: o.Env, Spec: Spec, Chosen: o.Chosen}, self: o.Self, memory: o.Memory, workDir: o.WorkDir}
}

func (a *agent) Info() gateway.Info {
	return gateway.Info{
		ID: "claude-code", Name: "Claude Code",
		Sends:    &gateway.Recipient{Provider: "Anthropic", Account: "your Claude account"},
		Streams:  true,
		Command:  "claude",
		Download: "https://claude.com/product/claude-code",
	}
}

func (a *agent) Probe(ctx context.Context) gateway.Probe { return a.prog.Probe(ctx) }

func (a *agent) connectArgs() []string {
	return []string{"mcp", "add", "-s", "user", "capy", "--", a.self, "mcp"}
}

// ConnectCommands removes an entry Capy cannot see first: claude mcp add
// refuses one that exists, and Capy keeps the connection itself rather
// than ask (step 0), so an entry added by hand, or before agent.json was
// reset, would otherwise stop every Connect.
func (a *agent) ConnectCommands() [][]string {
	return [][]string{a.DisconnectCommand(), append([]string{a.prog.Path()}, a.connectArgs()...)}
}

func (a *agent) Connect(ctx context.Context) error {
	if _, err := a.prog.Run(ctx, disconnectArgs...); err != nil && !missing(err) {
		return err
	}
	if _, err := a.prog.Run(ctx, a.connectArgs()...); err != nil {
		return err
	}
	return a.remember(true)
}

// missing reports claude mcp remove's exit for a server that is not there.
func missing(err error) bool {
	ee, ok := errors.AsType[*cli.ExitError](err)
	return ok && ee.Code == 1
}

// disconnectArgs remove Capy's server from the user's settings.
var disconnectArgs = []string{"mcp", "remove", "-s", "user", "capy"}

func (a *agent) DisconnectCommand() []string {
	return append([]string{a.prog.Path()}, disconnectArgs...)
}

// Disconnect leaves Capy disconnected also when the entry was removed by
// hand.
func (a *agent) Disconnect(ctx context.Context) error {
	if _, err := a.prog.Run(ctx, disconnectArgs...); err != nil && !missing(err) {
		return err
	}
	return a.remember(false)
}

func (a *agent) remember(connected bool) error {
	if a.memory == nil {
		return nil
	}
	return a.memory.SetConnected(connected)
}

// Connection is what the last Connect or Disconnect left; Claude Code is
// never reported Moved.
func (a *agent) Connection(context.Context) (gateway.Connection, error) {
	if a.memory != nil && a.memory.Connected() {
		return gateway.Connected, nil
	}
	return gateway.NotConnected, nil
}
