package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/cli"
)

// toolPrefix is how Claude Code names the tools of Capy's server.
const toolPrefix = "mcp__capy__"

// mcpConfig is the --mcp-config JSON naming Capy's server, command, so
// that no per-run file is written.
func mcpConfig(command []string) (string, error) {
	if len(command) == 0 {
		return "", errors.New("claudecode: no command for Capy's tools")
	}
	type server struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	b, err := json.Marshal(map[string]map[string]server{"mcpServers": {"capy": {command[0], command[1:]}}})
	return string(b), err
}

// noServers is the --mcp-config of a run with no tools: with
// --strict-mcp-config, no server loads, the user's neither.
const noServers = `{"mcpServers":{}}`

// runArgs are claude's arguments for q: Capy's tools and nothing else, or
// no tools at all for NoTools; no settings, no CLAUDE.md, streamed as
// JSON. The prompt comes after --, so that one starting with - is not
// taken for a flag.
func runArgs(q gateway.Query) ([]string, error) {
	cfg := noServers
	if !q.NoTools {
		var err error
		if cfg, err = mcpConfig(q.Tools.Command); err != nil {
			return nil, err
		}
	}
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages",
		"--mcp-config", cfg, "--strict-mcp-config", "--tools", ""}
	if !q.NoTools {
		args = append(args, "--allowedTools", toolPrefix+"*")
	}
	args = append(args, "--permission-mode", "dontAsk", "--setting-sources", "")
	if q.Model != "" { // one argument, never taken for a flag
		args = append(args, "--model="+q.Model)
	}
	if q.ConversationID != "" {
		if err := uuid.Validate(q.ConversationID); err != nil {
			return nil, errors.New("claudecode: a conversation id is a UUID")
		}
		args = append(args, "--resume", q.ConversationID)
	}
	return append(args, "--", q.Prompt), nil
}

func (a *agent) Run(ctx context.Context, q gateway.Query) (gateway.Run, error) {
	args, err := runArgs(q)
	if err != nil {
		return nil, err
	}
	p, err := a.prog.Start(a.workDir, args...)
	if err != nil {
		return nil, err
	}
	return cli.NewRun(ctx, p, &stream{}), nil
}

// ContinueCommand resumes conversation id in claude with the user's own
// settings and servers, and Capy's.
func (a *agent) ContinueCommand(id string, tools gateway.Tools) ([]string, error) {
	path, ok := a.prog.Found()
	if !ok {
		return nil, gateway.ErrUnavailable
	}
	if err := uuid.Validate(id); err != nil {
		return nil, errors.New("claudecode: a conversation id is a UUID")
	}
	cfg, err := mcpConfig(tools.Command)
	if err != nil {
		return nil, err
	}
	return []string{path, "--resume", id, "--mcp-config", cfg}, nil
}

// line is the part of a stream-json line the stream reads.
type line struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	// system/init
	SessionID string `json:"session_id"`
	// stream_event
	Event struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
	// assistant, user
	Error   string `json:"error"`
	Message struct {
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
		} `json:"content"`
	} `json:"message"`
	// rate_limit_event
	RateLimit struct {
		Status   string `json:"status"`
		ResetsAt int64  `json:"resetsAt"`
	} `json:"rate_limit_info"`
	// result
	IsError bool     `json:"is_error"`
	Result  string   `json:"result"`
	Errors  []string `json:"errors"`
}

// stream reads claude's stream-json: system/init gives the session,
// stream_event text deltas the text as it is written, assistant messages
// whole messages and tool calls, user tool results their end, and result
// the outcome.
type stream struct {
	calls     map[string]gateway.ToolCall // by tool_use id
	result    bool                        // a result line came
	isError   bool                        // the result is a failure
	said      string                      // the result's errors, to classify
	signedOut bool
	limited   bool
	resetAt   time.Time
}

func (s *stream) Line(b []byte) []gateway.Event {
	var l line
	if json.Unmarshal(b, &l) != nil {
		return nil
	}
	switch l.Type {
	case "system":
		if l.Subtype == "init" && l.SessionID != "" {
			return []gateway.Event{gateway.Started{ConversationID: l.SessionID}}
		}
	case "stream_event":
		if l.Event.Type == "content_block_delta" && l.Event.Delta.Type == "text_delta" && l.Event.Delta.Text != "" {
			return []gateway.Event{gateway.TextDelta{Text: l.Event.Delta.Text}}
		}
	case "assistant":
		return s.assistant(l)
	case "user":
		var events []gateway.Event
		for _, c := range l.Message.Content {
			if call, ok := s.calls[c.ToolUseID]; ok && c.Type == "tool_result" {
				delete(s.calls, c.ToolUseID)
				call.Done = true
				events = append(events, call)
			}
		}
		return events
	case "rate_limit_event":
		if l.RateLimit.Status == "rejected" {
			s.limited = true
			if l.RateLimit.ResetsAt > 0 {
				s.resetAt = time.Unix(l.RateLimit.ResetsAt, 0)
			}
		}
	case "result":
		s.result, s.isError = true, l.IsError
		if l.IsError {
			s.said = strings.Join(append(l.Errors, l.Result), "\n")
		}
	}
	return nil
}

// assistant reads a whole message: its text, and the tools it calls. A
// message with an error is Claude Code's own ("Not logged in"), not an
// answer.
func (s *stream) assistant(l line) []gateway.Event {
	switch l.Error {
	case "":
	case "authentication_failed":
		s.signedOut = true
		return nil
	case "rate_limit":
		s.limited = true
		return nil
	default:
		return nil
	}
	var events []gateway.Event
	for _, c := range l.Message.Content {
		switch c.Type {
		case "text":
			if c.Text != "" {
				events = append(events, gateway.Message{Text: c.Text})
			}
		case "tool_use":
			call := gateway.ToolCall{Name: strings.TrimPrefix(c.Name, toolPrefix), SessionID: sessionOf(c.Input)}
			if s.calls == nil {
				s.calls = map[string]gateway.ToolCall{}
			}
			s.calls[c.ID] = call
			events = append(events, call)
		}
	}
	return events
}

// sessionOf is the session a tool call's input names, "" for none: input
// is decoded on its own, so that one of another shape loses only this.
func sessionOf(input json.RawMessage) string {
	var in struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(input, &in) != nil || uuid.Validate(in.ID) != nil {
		return ""
	}
	return in.ID
}

func (s *stream) End(code int, stderr []byte) gateway.Event {
	switch {
	case s.result && !s.isError:
		return gateway.Done{}
	case s.result:
		return s.failed(s.said, code)
	}
	return s.failed(string(stderr), code)
}

// failed is why a run failed, from what Claude Code said: the result's
// errors, or its stderr.
func (s *stream) failed(said string, code int) gateway.Failed {
	switch {
	case s.signedOut || containsAny(said, "Not logged in", "Please run /login", "Invalid API key"):
		return gateway.Failed{Kind: gateway.SignedOut, Code: code}
	case s.limited || containsAny(said, "usage limit", "hit your limit"):
		return gateway.Failed{Kind: gateway.UsageLimit, ResetAt: s.resetAt, Code: code}
	case containsAny(said, "No conversation found"):
		return gateway.Failed{Kind: gateway.ResumeRejected, Code: code}
	}
	return gateway.Failed{Kind: gateway.Exited, Code: code}
}

// containsAny reports whether s contains one of subs, ignoring case.
func containsAny(s string, subs ...string) bool {
	s = strings.ToLower(s)
	for _, sub := range subs {
		if strings.Contains(s, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}
