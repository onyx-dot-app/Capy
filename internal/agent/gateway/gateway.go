// Package gateway is how Agent reaches the user's own agents: the Agent
// contract every adapter implements, the capabilities an adapter may add,
// and the Gateway that holds the registered agents, what was found of
// each, and every run: routing, consent, Capy's tools, limits, Stop and
// logs are the gateway's, the same for every agent. No screen knows which
// agents exist: they list them here.
package gateway

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Agent is an agent Capy can use: an adapter that knows how to find
// itself and answer one turn of a conversation with Capy's tools.
type Agent interface {
	Info() Info
	// Probe looks for the agent: is it installed, where, which version. It
	// may run a program for seconds: never call it on the main thread.
	Probe(ctx context.Context) Probe
	// Run starts answering q. The run reports on Events until it is done
	// or failed, and stops early on Stop or when ctx ends.
	Run(ctx context.Context, q Query) (Run, error)
}

// Query is one turn of a conversation.
type Query struct {
	Prompt string
	// ConversationID resumes the agent's conversation of that ID; ""
	// starts one.
	ConversationID string
	// Model is the model to answer with, "" for the agent's default.
	Model string
	// Restart is the prompt that starts the conversation anew, carrying
	// what was said in it, should the agent refuse to resume it; "" for
	// none. The gateway sends it; an agent never reads it.
	Restart string
	// NoTools runs the agent with no tools at all, Capy's neither: for a
	// query whose prompt carries all the agent needs.
	NoTools bool
	// Tools is how the agent reaches Capy's tools; the gateway sets it,
	// unless NoTools.
	Tools Tools
}

// Tools is how an agent reaches Capy's MCP server: a command to start,
// for an agent in another process, or a session in this one.
type Tools struct {
	Command []string
	Open    func(ctx context.Context) (*mcp.ClientSession, error)
}

// Run is an agent answering one turn.
type Run interface {
	// Events reports the run in order, and is closed after Done or
	// Failed.
	Events() <-chan Event
	// Stop ends the run; Events then reports Failed and is closed.
	Stop()
}

// Event is what a run reports: Started, TextDelta, Message, ToolCall,
// Done or Failed.
type Event interface{ event() }

// Started reports the agent's ID of the conversation, to resume it.
type Started struct{ ConversationID string }

// TextDelta is text added to the message being written, by an agent that
// streams.
type TextDelta struct{ Text string }

// Message is a whole message of the answer: for an agent that streams,
// the text its TextDeltas wrote since the last Message.
type Message struct{ Text string }

// ToolCall is a call of one of Capy's tools, as it starts and as it is
// Done.
type ToolCall struct {
	// Name is the tool's, without the agent's prefix: "get_session".
	Name string
	// SessionID is the session the call names, "" for none.
	SessionID string
	Done      bool
}

// Done ends a run that answered.
type Done struct{}

// Failed ends a run that did not answer in full.
type Failed struct {
	Kind Failure
	// ResetAt is when a usage limit resets, when the agent says.
	ResetAt time.Time
	// Code is an Exited program's exit status, -1 when it was killed.
	Code int
}

func (Started) event()   {}
func (TextDelta) event() {}
func (Message) event()   {}
func (ToolCall) event()  {}
func (Done) event()      {}
func (Failed) event()    {}

// Failure is why a run failed.
type Failure int

const (
	// Exited: the agent stopped unexpectedly.
	Exited Failure = iota
	// SignedOut: the agent needs the user to sign in.
	SignedOut
	// UsageLimit: the user's plan allows no more for now.
	UsageLimit
	// ResumeRejected: the agent has no conversation of that ID.
	ResumeRejected
	// Stalled: the run was silent or long past the gateway's limits.
	Stalled
	// Stopped: the user's Stop, Capy quitting, or Agent turned off.
	Stopped
)

func (f Failure) String() string {
	switch f {
	case SignedOut:
		return "signed-out"
	case UsageLimit:
		return "usage-limit"
	case ResumeRejected:
		return "resume-rejected"
	case Stalled:
		return "stalled"
	case Stopped:
		return "stopped"
	}
	return "exited"
}

// Info describes an agent.
type Info struct {
	ID   string // stable, saved with each thread: "claude-code", "codex"
	Name string // "Claude Code", "ChatGPT"
	// Sends is where questions and what the agent reads go; nil: nothing
	// leaves this Mac.
	Sends *Recipient
	// Streams reports that text arrives as it is written, not in whole
	// messages.
	Streams bool
	// How Settings names it, so that no screen knows which agents exist.
	// Command is the file name of its program, which Choose… looks for;
	// Program names that program when it is not the agent ("Codex" of
	// ChatGPT), "" otherwise. AppsSubject is what one connection serves
	// when that is more than the agent, as a sentence's subject ("ChatGPT,
	// the Codex CLI and Codex in your editor"). Download is the page to get
	// it.
	Command, Program, AppsSubject, Download string
}

// Recipient is who receives what an agent sends: "Anthropic", "your
// Claude account".
type Recipient struct{ Provider, Account string }

// Probe is what looking for an agent found.
type Probe struct {
	Found         bool
	Path, Version string
	// TooOld is the minimum version, when the agent was found but older.
	TooOld string
}

// Usable reports whether the agent was found and is recent enough.
func (p Probe) Usable() bool { return p.Found && p.TooOld == "" }

// Errors of Send.
var (
	// ErrUnavailable is an agent that is not registered, not found, or
	// too old to use, or Agent turned off.
	ErrUnavailable = errors.New("gateway: agent unavailable")
	// ErrConsentNeeded is an agent that sends questions off this Mac,
	// which the user has not allowed yet: Allow it, then send again.
	ErrConsentNeeded = errors.New("gateway: the user has not allowed this agent")
	// ErrBusy is a conversation with a run going: one run per
	// conversation.
	ErrBusy = errors.New("gateway: the conversation has a run going")
	// ErrClosed is a gateway closed as Capy quits.
	ErrClosed = errors.New("gateway: closed")
)

// Connection is whether an agent's own apps reach Capy's tools.
type Connection int

const (
	// NotConnected: the agent has no Capy server.
	NotConnected Connection = iota
	// Connected: the agent starts this Capy.
	Connected
	// Moved: the agent starts a Capy at another path.
	Moved
)

// ExitCoder is an error of an agent's program that exited with a status,
// which Settings shows: "Couldn't connect (exit 1)".
type ExitCoder interface {
	error
	ExitCode() int
}

// Connector is an agent whose own apps can be connected to Capy's tools,
// with its own command-line tool, which writes its own config.
type Connector interface {
	// ConnectCommands and DisconnectCommand are the commands Connect and
	// Disconnect run, in order, as the user is shown them before they run.
	ConnectCommands() [][]string
	DisconnectCommand() []string
	Connect(ctx context.Context) error
	Disconnect(ctx context.Context) error
	Connection(ctx context.Context) (Connection, error)
}

// HandOff is an agent whose conversation the user can continue in the
// agent itself, in Terminal.
type HandOff interface {
	// ContinueCommand is the command that resumes conversation id with
	// Capy's tools from tools.Command; an id not of the agent's format is
	// an error.
	ContinueCommand(id string, tools Tools) ([]string, error)
}

// Models is an agent that runs a model the user may choose.
type Models interface {
	// DefaultModel is the model of the agent's own settings, "" for none.
	DefaultModel(ctx context.Context) (string, error)
	// Models are the models the user may choose among.
	Models(ctx context.Context) ([]string, error)
}

// Available is an agent with what its last probe found.
type Available struct {
	Info
	Probe Probe
	Agent Agent
}

// Consent keeps which agents the user allowed to receive questions.
type Consent interface {
	Allowed(agentID string) bool
	Allow(agentID string) error
}

// Options are what the gateway's runs depend on. The zero Options run
// agents without Capy's tools, consent kept in memory, the limits below,
// and no logs.
type Options struct {
	// Command is the command that serves Capy's tools to run runID, ""
	// for a conversation continued outside Capy.
	Command func(runID string) []string
	// Server is Capy's MCP server for run runID, which Tools.Open serves
	// in this process.
	Server func(runID string) *mcp.Server
	// RunsDir holds a file named after each run going, for Capy mcp to
	// check that a run it serves is one the gateway started; "" for none.
	RunsDir string
	Consent Consent
	// Enabled reports whether Agent is on; a run stops when it turns off.
	// nil is always on.
	Enabled func() bool
	// Idle and Total stop a run that reports nothing, or runs, longer:
	// 60 s and 120 s when zero.
	Idle, Total time.Duration
	Log         *slog.Logger
}

// Gateway holds the registered agents, their last probes and the runs
// going. It is safe for concurrent use.
type Gateway struct {
	o      Options
	agents []Agent

	probing sync.Mutex // one probe of every agent at a time
	mu      sync.Mutex
	found   []Available // nil until the first probe
	busy    map[string]bool
	turns   map[*Turn]bool
	closed  bool
	running sync.WaitGroup
}

// New returns a gateway of agents, in the order the UI lists them.
func New(o Options, agents ...Agent) *Gateway {
	if o.Consent == nil {
		o.Consent = &memoryConsent{}
	}
	if o.Enabled == nil {
		o.Enabled = func() bool { return true }
	}
	if o.Idle == 0 {
		o.Idle = 60 * time.Second
	}
	if o.Total == 0 {
		o.Total = 120 * time.Second
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Gateway{o: o, agents: agents, busy: map[string]bool{}, turns: map[*Turn]bool{}}
}

// memoryConsent is consent kept for the gateway's life.
type memoryConsent struct {
	mu      sync.Mutex
	allowed map[string]bool
}

func (c *memoryConsent) Allowed(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.allowed[id]
}

func (c *memoryConsent) Allow(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.allowed == nil {
		c.allowed = map[string]bool{}
	}
	c.allowed[id] = true
	return nil
}

// Agents returns every agent with its last probe, probing them all the
// first time; the probes stay until Refresh.
func (g *Gateway) Agents(ctx context.Context) []Available {
	if found := g.cached(); found != nil {
		return found
	}
	g.probing.Lock()
	defer g.probing.Unlock()
	if found := g.cached(); found != nil { // probed while this one waited
		return found
	}
	return g.probe(ctx)
}

// Refresh probes every agent anew, as Look Again does.
func (g *Gateway) Refresh(ctx context.Context) []Available {
	g.probing.Lock()
	defer g.probing.Unlock()
	return g.probe(ctx)
}

// Agent returns the registered agent of id.
func (g *Gateway) Agent(id string) (Agent, bool) {
	for _, a := range g.agents {
		if a.Info().ID == id {
			return a, true
		}
	}
	return nil, false
}

func (g *Gateway) cached() []Available {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.found == nil {
		return nil
	}
	return append([]Available(nil), g.found...)
}

// probe probes every agent at once. The caller holds g.probing.
func (g *Gateway) probe(ctx context.Context) []Available {
	found := make([]Available, len(g.agents))
	var wg sync.WaitGroup
	for i, a := range g.agents {
		wg.Go(func() { found[i] = Available{Info: a.Info(), Probe: a.Probe(ctx), Agent: a} })
	}
	wg.Wait()
	if ctx.Err() != nil {
		return found // cut short: the next call probes again
	}
	g.mu.Lock()
	g.found = found
	g.mu.Unlock()
	return append([]Available(nil), found...)
}
