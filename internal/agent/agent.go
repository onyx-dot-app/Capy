// Package agent is Agent, the add-on through which the user's own agents
// (Claude Code, ChatGPT) work with Capy's recordings: Capy mcp, Capy's
// tools for them, and Ask inside Capy. Only main imports it; nothing of
// the recording code does. Everything it keeps is in agent/ in Capy's
// data directory.
package agent

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/onyx-dot-app/Capy/internal/agent/ask"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/cli"
	"github.com/onyx-dot-app/Capy/internal/agent/polish"
	"github.com/onyx-dot-app/Capy/internal/agent/prefs"
	"github.com/onyx-dot-app/Capy/internal/agent/threads"
	threadsqlite "github.com/onyx-dot-app/Capy/internal/agent/threads/sqlite"
	"github.com/onyx-dot-app/Capy/internal/agent/tools"
	agentui "github.com/onyx-dot-app/Capy/internal/agent/ui"
	"github.com/onyx-dot-app/Capy/internal/features"
	"github.com/onyx-dot-app/Capy/internal/profile"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/views/addon"
)

// Exit statuses of ServeMCP.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
	// ExitTurnedOff is Capy mcp's status with Agent turned off: the agent
	// that started it reports the server failed.
	ExitTurnedOff = 3
)

// Deps are what the module needs of the app.
type Deps struct {
	Store store.Store
	Gate  features.Gate
	// Dir is Capy's data directory; Agent's data is in its agent/ folder.
	Dir string
	// Self is Capy's executable, which agents run as `<Self> mcp`.
	Self string
	// Env is where agents are looked for and how they run; the zero Env is
	// the user's (cli.DefaultEnv). Tests give one of their own.
	Env cli.Env
	Log *slog.Logger
}

// Module is Agent, wired.
type Module struct {
	deps    Deps
	prefs   *prefs.File
	threads *threadsqlite.DB
	gateway *gateway.Gateway
	ask     *ask.Ask
	polish  *polish.Polish
	ui      *agentui.AddOn
}

// folder is Agent's folder in Capy's data directory dir; work the empty
// folder agents run in, so that no repository's instructions, MCP servers
// or hooks load; runs the folder naming each run going, for Capy mcp.
func folder(dir string) string { return filepath.Join(dir, "agent") }

func workDir(dir string) string { return filepath.Join(folder(dir), "work") }

func runsDir(dir string) string { return filepath.Join(folder(dir), "runs") }

// readProfile is the user's profile in Capy's data directory dir, read
// anew for each get_profile: the app may have changed it since.
func readProfile(dir string) tools.Profile {
	p := profile.Read(filepath.Join(dir, profile.FileName))
	return tools.Profile{Name: p.Name, Aliases: p.Aliases}
}

// continueDir is where a conversation continued in Terminal runs, with
// the user's full agent: apart from work, which it could otherwise leave
// instructions in for Capy's own runs.
func continueDir(dir string) string { return filepath.Join(folder(dir), "continue") }

// New opens Agent's data in d.Dir, creating its folder. It does so with
// Agent turned off too: erasing a session's threads never waits for the
// gate.
func New(ctx context.Context, d Deps) (*Module, error) {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if err := os.MkdirAll(workDir(d.Dir), 0o700); err != nil {
		return nil, fmt.Errorf("agent: create folder: %w", prefs.Pathless(err))
	}
	// No run goes yet: what names one is left from a Capy that crashed.
	if err := os.RemoveAll(runsDir(d.Dir)); err != nil {
		return nil, fmt.Errorf("agent: clear runs: %w", prefs.Pathless(err))
	}
	db, err := threadsqlite.Open(ctx, filepath.Join(folder(d.Dir), "agent.db"))
	if err != nil {
		return nil, fmt.Errorf("agent: open threads: %w", prefs.Pathless(err))
	}
	m := &Module{deps: d, prefs: prefs.Load(filepath.Join(folder(d.Dir), "agent.json")), threads: db}
	env := d.Env
	if env.Run == nil {
		env = cli.DefaultEnv()
	}
	m.gateway = gateway.New(gateway.Options{
		Command: m.mcpCommand, Server: m.server, RunsDir: runsDir(d.Dir),
		Consent: consent{m.prefs}, Enabled: func() bool { return d.Gate.Enabled(features.Agent) }, Log: d.Log,
	}, agents(m.prefs, d.Self, workDir(d.Dir), env)...)
	model := func(id string) string { return m.prefs.Get().Models[id] }
	m.ask = ask.New(ask.Options{Gateway: m.gateway, Threads: db, Library: d.Store, Agent: m.chosenAgent,
		Model: model, ContinueDir: continueDir(d.Dir), Log: d.Log})
	m.polish = polish.New(polish.Options{Store: d.Store, Gateway: m.gateway, Agent: m.chosenAgent, Model: model,
		On:      func() bool { return m.ui.PolishOn(m.chosenAgent()) },
		Profile: func() profile.Profile { return profile.Read(filepath.Join(d.Dir, profile.FileName)) },
		Saved:   func(id string) { m.ui.Polished(id) },
		Stopped: func(s polish.Stop) { m.ui.PolishStopped(s) },
		Log:     d.Log})
	m.ui = agentui.New(agentui.Options{Gate: d.Gate, Gateway: m.gateway, Ask: m.ask, Polish: m.polish, Prefs: m.prefs, Log: d.Log})
	return m, nil
}

// mcpCommand is the command that serves Capy's tools to run, or, for "",
// to a conversation continued outside Capy: this Capy's mcp on this data
// directory.
func (m *Module) mcpCommand(run string) []string {
	cmd := []string{m.deps.Self, "mcp", "-data", m.deps.Dir}
	if run != "" {
		cmd = append(cmd, "-actor", tools.ActorAgent, "-run", run)
	}
	return cmd
}

// server is Capy's MCP server for run, in this process.
func (m *Module) server(run string) *mcp.Server {
	return tools.NewServer(tools.Options{Store: m.deps.Store, Actor: tools.ActorAgent, Origin: run, Log: m.deps.Log,
		Profile: func() tools.Profile { return readProfile(m.deps.Dir) }})
}

// chosenAgent is the agent of a new thread: the user's choice while it
// is usable, else the first found, as Settings shows it.
func (m *Module) chosenAgent() string {
	if id := m.prefs.Get().Agent; id != "" && m.gateway.Usable(context.Background(), id) {
		return id
	}
	for _, av := range m.gateway.Agents(context.Background()) {
		if av.Probe.Usable() {
			return av.ID
		}
	}
	return ""
}

// consent keeps the agents the user allowed in agent.json.
type consent struct{ p *prefs.File }

func (c consent) Allowed(id string) bool { return slices.Contains(c.p.Get().Allowed, id) }

func (c consent) Allow(id string) error {
	return c.p.Update(func(p *prefs.Prefs) {
		if !slices.Contains(p.Allowed, id) {
			p.Allowed = append(p.Allowed, id)
		}
	})
}

// Close stops every run, keeping what each said and its question
// unanswered if nothing, and closes Agent's data.
func (m *Module) Close() error {
	m.polish.Close()
	m.gateway.Close()
	m.ask.Close()
	return m.threads.Close()
}

// Ask is Ask, Agent's question answering.
func (m *Module) Ask() *ask.Ask { return m.ask }

// Allow records the user's consent to ask agent id, as the first Ask with
// it asks.
func (m *Module) Allow(id string) error { return m.gateway.Allow(id) }

// AddOn is what Agent shows in the views' slots: nothing while the gate
// keeps Agent off.
func (m *Module) AddOn() addon.AddOn { return m.ui }

// PurgeHook erases a session's threads as the session is purged. Register
// it whether Agent is on or off: Delete Now promises that a session's Ask
// threads go with it.
func (m *Module) PurgeHook() func(ctx context.Context, sessionID string) error {
	return m.threads.EraseSession
}

// Highlighted asks for a pass of polish of session sessionID, as the
// views' slot does: a replay's session calls it.
func (m *Module) Highlighted(sessionID string, final bool) { m.ui.Highlighted(sessionID, final) }

// WaitPolish returns once no pass of polish is going or due: a replay's,
// after its session ended.
func (m *Module) WaitPolish() { m.polish.Wait() }

// Threads is Agent's thread store.
func (m *Module) Threads() threads.Store { return m.threads }

// MCPDeps are what Capy mcp needs: it runs without the app.
type MCPDeps struct {
	// DataDir is Capy's data directory, unless -data names another, as
	// for the library of a replay.
	DataDir func() (string, error)
	// Gate is the feature gate of data directory dir.
	Gate func(dir string) features.Gate
	// OpenLibrary opens the library at path, returning its close.
	OpenLibrary func(ctx context.Context, path string) (store.Store, func() error, error)
	// Stderr takes the logs; stdout is the MCP stream.
	Stderr  io.Writer
	Version string
}

// ServeMCP is Capy mcp: it serves Capy's tools on stdin and stdout until
// stdin closes, and returns the exit status. args are its flags: -actor
// mcp for a connected agent, the default, or -actor agent -run <id> for
// a run the gateway started and has not ended; -data for a data directory
// other than Capy's.
func ServeMCP(ctx context.Context, args []string, d MCPDeps) int {
	log := slog.New(slog.NewTextHandler(d.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	flags := flag.NewFlagSet("mcp", flag.ContinueOnError)
	flags.SetOutput(d.Stderr)
	actor := flags.String("actor", tools.ActorMCP, "who edits through the server: mcp, or agent with -run")
	run := flags.String("run", "", "the ID of the Ask run that started the server, with -actor agent")
	data := flags.String("data", "", "the data directory, if not Capy's")
	if err := flags.Parse(args); err != nil || flags.NArg() > 0 {
		return exitUsage
	}
	if err := validActor(*actor, *run); err != nil {
		fmt.Fprintln(d.Stderr, "capy mcp:", err)
		return exitUsage
	}
	dir := *data
	if dir == "" {
		var err error
		if dir, err = d.DataDir(); err != nil {
			log.Error("data directory", "err", prefs.Pathless(err))
			return exitError
		}
	} else if !filepath.IsAbs(dir) {
		fmt.Fprintln(d.Stderr, "capy mcp: -data must be an absolute path")
		return exitUsage
	}
	if !d.Gate(dir).Enabled(features.Agent) {
		fmt.Fprintln(d.Stderr, "Agent is turned off")
		return ExitTurnedOff
	}
	if *actor == tools.ActorAgent && !gateway.RunGoing(runsDir(dir), *run) {
		fmt.Fprintln(d.Stderr, "capy mcp: no such run going")
		return exitUsage
	}
	st, closeLibrary, err := d.OpenLibrary(ctx, filepath.Join(dir, "capy.db"))
	if err != nil {
		log.Error("open library", "err", err)
		return exitError
	}
	defer func() {
		if err := closeLibrary(); err != nil {
			log.Warn("close library", "err", err)
		}
	}()
	srv := tools.NewServer(tools.Options{Store: st, Actor: *actor, Origin: *run, Version: d.Version, Log: log,
		Profile: func() tools.Profile { return readProfile(dir) }})
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
		log.Error("serve", "err", err)
		return exitError
	}
	return exitOK
}

// validActor checks Capy mcp's -actor and -run: a connected agent has no
// run, an agent Ask started has its run's ID; there is no other actor.
func validActor(actor, run string) error {
	switch actor {
	case tools.ActorMCP:
		if run != "" {
			return errors.New("-run goes with -actor agent")
		}
	case tools.ActorAgent:
		if u, err := uuid.Parse(run); err != nil || u.String() != run {
			return errors.New("-actor agent needs -run with the run's ID")
		}
	default:
		return errors.New("-actor must be mcp or agent")
	}
	return nil
}
