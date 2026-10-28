package agent

import (
	"slices"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/claudecode"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/cli"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/codex"
	"github.com/onyx-dot-app/Capy/internal/agent/prefs"
)

// agents is the gateway's built-in agents, one line each: adding one is an
// adapter package under gateway/ and a line here. self is Capy's
// executable, whose `mcp` serves Capy's tools, and work the folder agents
// run in.
func agents(p *prefs.File, self, work string, env cli.Env) []gateway.Agent {
	return []gateway.Agent{
		claudecode.New(claudecode.Options{Env: env, Self: self, WorkDir: work, Chosen: chosen(p, "claude-code"), Memory: memory{p, "claude-code"}}),
		codex.New(codex.Options{Env: env, Self: self, WorkDir: work, Chosen: chosen(p, "codex")}),
	}
}

// chosen is the path the user chose for agent id, read as it is probed.
func chosen(p *prefs.File, id string) func() string {
	return func() string { return p.Get().Paths[id] }
}

// memory keeps an agent's connection in agent.json, for an agent whose CLI
// cannot report it without starting Capy mcp.
type memory struct {
	p  *prefs.File
	id string
}

func (m memory) Connected() bool { return slices.Contains(m.p.Get().Connected, m.id) }

func (m memory) SetConnected(on bool) error {
	return m.p.Update(func(p *prefs.Prefs) {
		p.Connected = slices.DeleteFunc(p.Connected, func(id string) bool { return id == m.id })
		if on {
			p.Connected = append(p.Connected, m.id)
		}
	})
}
