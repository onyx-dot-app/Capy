// Package fake is a scripted agent for tests: in this process, as Agent,
// or as fakeagent (Main), a stand-in for claude and codex that takes
// either one's arguments and prints its stream. Either way it calls
// Capy's real tools, as the script says, and answers what the script
// says. Nothing of the app imports it.
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
)

// Script is what the fake does in a run.
type Script struct {
	// Calls are the tools it calls, in order. "{session}" in an argument
	// is the first UUID of the prompt.
	Calls []Call `json:"calls"`
	// Answer is its answer, written a word at a time.
	Answer string `json:"answer"`
	// Stall stops it after the calls, silent until it is stopped.
	Stall bool `json:"stall,omitempty"`
	// Fail ends the run failing after the calls and what of the answer
	// FailAfter keeps: "signed-out", "usage-limit" or "exit".
	Fail      string `json:"fail,omitempty"`
	FailAfter int    `json:"failAfter,omitempty"` // bytes of the answer
	// RejectResume refuses every conversation ID, as an agent that lost
	// the conversation does.
	RejectResume bool `json:"rejectResume,omitempty"`
}

// Call is a call of one of Capy's tools.
type Call struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// ScriptEnv names the file of fakeagent's script.
const ScriptEnv = "CAPY_FAKEAGENT_SCRIPT"

// ReadScript reads a script file.
func ReadScript(path string) (Script, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Script{}, err
	}
	var s Script
	if err := json.Unmarshal(b, &s); err != nil {
		return Script{}, err
	}
	return s, nil
}

var uuidRe = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// play plays s against Capy's tools in cs, nil for a run with none, for
// prompt, reporting each step with emit, but the run's Started and its
// end. It returns the failure the script ends with, if any, and ctx's
// error when it was stopped.
func play(ctx context.Context, cs *mcp.ClientSession, s Script, prompt string, emit func(gateway.Event)) (*gateway.Failed, error) {
	if cs == nil && len(s.Calls) > 0 {
		return nil, errors.New("fake: a tool call in a run with no tools")
	}
	session := uuidRe.FindString(prompt)
	for _, c := range s.Calls {
		args := map[string]any{}
		for k, v := range c.Args {
			if str, ok := v.(string); ok {
				v = strings.ReplaceAll(str, "{session}", session)
			}
			args[k] = v
		}
		id, _ := args["id"].(string)
		emit(gateway.ToolCall{Name: c.Tool, SessionID: id})
		if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: c.Tool, Arguments: args}); err != nil {
			return nil, err
		}
		emit(gateway.ToolCall{Name: c.Tool, SessionID: id, Done: true})
	}
	if s.Stall {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	answer := s.Answer
	if s.Fail != "" {
		answer = answer[:min(s.FailAfter, len(answer))]
	}
	if answer != "" {
		for _, w := range strings.SplitAfter(answer, " ") {
			emit(gateway.TextDelta{Text: w})
		}
		emit(gateway.Message{Text: answer})
	}
	if s.Fail != "" {
		f := gateway.Failed{Kind: failure(s.Fail), Code: 1}
		return &f, nil
	}
	return nil, nil
}

func failure(name string) gateway.Failure {
	switch name {
	case "signed-out":
		return gateway.SignedOut
	case "usage-limit":
		return gateway.UsageLimit
	}
	return gateway.Exited
}

// Agent is the fake as a gateway agent in this process: ID "fake",
// sending nothing off the Mac. It reaches Capy's tools through
// Tools.Open, else by starting Tools.Command.
type Agent struct {
	Script Script

	mu      sync.Mutex
	queries []gateway.Query
}

// Queries are the queries the agent was sent, oldest first.
func (a *Agent) Queries() []gateway.Query {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]gateway.Query(nil), a.queries...)
}

func (a *Agent) Info() gateway.Info {
	return gateway.Info{ID: "fake", Name: "Fake Agent", Streams: true, Command: "fakeagent"}
}

func (a *Agent) Probe(context.Context) gateway.Probe {
	return gateway.Probe{Found: true, Path: "/fake/fakeagent", Version: "1.0.0"}
}

func (a *Agent) Run(ctx context.Context, q gateway.Query) (gateway.Run, error) {
	a.mu.Lock()
	a.queries = append(a.queries, q)
	a.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	r := &run{events: make(chan gateway.Event, 64), cancel: cancel}
	go func() {
		defer close(r.events)
		defer cancel()
		r.events <- a.answer(ctx, q, func(e gateway.Event) { r.events <- e })
	}()
	return r, nil
}

// answer plays the script for q and returns the run's end.
func (a *Agent) answer(ctx context.Context, q gateway.Query, emit func(gateway.Event)) gateway.Event {
	if q.ConversationID != "" && a.Script.RejectResume {
		return gateway.Failed{Kind: gateway.ResumeRejected, Code: 1}
	}
	var cs *mcp.ClientSession
	if !q.NoTools {
		var err error
		if cs, err = open(ctx, q.Tools); err != nil {
			return gateway.Failed{Kind: gateway.Exited, Code: 1}
		}
		defer cs.Close()
	}
	emit(gateway.Started{ConversationID: conversation(q)})
	f, err := play(ctx, cs, a.Script, q.Prompt, emit)
	switch {
	case ctx.Err() != nil:
		return gateway.Failed{Kind: gateway.Stopped, Code: -1}
	case err != nil:
		return gateway.Failed{Kind: gateway.Exited, Code: 1}
	case f != nil:
		return *f
	}
	return gateway.Done{}
}

// conversation is the run's conversation: the one resumed, else a new
// one.
func conversation(q gateway.Query) string {
	if q.ConversationID != "" {
		return q.ConversationID
	}
	return uuid.NewString()
}

// open connects to Capy's tools.
func open(ctx context.Context, t gateway.Tools) (*mcp.ClientSession, error) {
	if t.Open != nil {
		return t.Open(ctx)
	}
	if len(t.Command) == 0 {
		return nil, errors.New("fake: no tools")
	}
	cmd := exec.Command(t.Command[0], t.Command[1:]...)
	return mcp.NewClient(&mcp.Implementation{Name: "fake"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
}

type run struct {
	events chan gateway.Event
	cancel context.CancelFunc
}

func (r *run) Events() <-chan gateway.Event { return r.events }
func (r *run) Stop()                        { r.cancel() }
