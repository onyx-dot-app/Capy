package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/cli"
)

// serverFlags are the -c flags that give codex Capy's server, command,
// as TOML values: a JSON string or array of strings is one.
func serverFlags(command []string) ([]string, error) {
	if len(command) == 0 {
		return nil, errors.New("codex: no command for Capy's tools")
	}
	cmd, err := json.Marshal(command[0])
	if err != nil {
		return nil, err
	}
	args, err := json.Marshal(append([]string{}, command[1:]...))
	if err != nil {
		return nil, err
	}
	return []string{"-c", "mcp_servers.capy.command=" + string(cmd), "-c", "mcp_servers.capy.args=" + string(args)}, nil
}

// lockdown keeps a run to Capy's tools: no user config, no shell, web,
// browser, images, apps, plugins, hooks, skills or sub-agents (every
// feature of the bundled 0.162 on by default that gives a tool), and a
// read-only sandbox for anything codex runs. code_mode_host stays: codex
// reaches MCP tools through it.
var lockdown = []string{
	"-c", `web_search="disabled"`,
	"--disable", "shell_tool", "--disable", "unified_exec", "--disable", "image_generation",
	"--disable", "apps", "--disable", "plugins", "--disable", "browser_use", "--disable", "computer_use",
	"--disable", "view_image", "--disable", "hooks", "--disable", "multi_agent",
	"--disable", "in_app_browser", "--disable", "browser_use_external", "--disable", "browser_use_full_cdp_access",
	"--disable", "tool_suggest", "--disable", "skill_search", "--disable", "skill_mcp_dependency_install",
	"--disable", "workspace_dependencies", "--disable", "goals",
}

// runArgs are codex's arguments for q, with model, the model to answer
// with ("" for codex's own choice): Capy's server, none for NoTools, which
// --ignore-user-config leaves codex with no MCP server at all. exec
// resume has no -s: the sandbox is a -c flag there. The prompt comes
// after --, so that one starting with - is not taken for a flag.
func runArgs(q gateway.Query, model string) ([]string, error) {
	var server []string
	if !q.NoTools {
		var err error
		if server, err = serverFlags(q.Tools.Command); err != nil {
			return nil, err
		}
		server = append(server, "-c", `mcp_servers.capy.default_tools_approval_mode="approve"`)
	}
	args := []string{"exec"}
	if q.ConversationID != "" {
		if err := uuid.Validate(q.ConversationID); err != nil {
			return nil, errors.New("codex: a conversation id is a UUID")
		}
		args = append(args, "resume", "-c", `sandbox_mode="read-only"`)
	} else {
		args = append(args, "-s", "read-only")
	}
	args = append(args, "--json", "--ignore-user-config", "--skip-git-repo-check")
	if model != "" { // one argument: a model read from config.toml cannot be a flag
		args = append(args, "--model="+model)
	}
	args = append(append(args, server...), lockdown...)
	if q.ConversationID != "" {
		args = append(args, q.ConversationID)
	}
	return append(args, "--", q.Prompt), nil
}

// Run runs codex exec in WorkDir. --ignore-user-config leaves codex no
// default model, so one of "" is the model of Codex's own settings.
func (a *agent) Run(ctx context.Context, q gateway.Query) (gateway.Run, error) {
	model := q.Model
	if model == "" {
		def, err := a.DefaultModel(ctx)
		if err != nil {
			return nil, err
		}
		model = def
	}
	args, err := runArgs(q, model)
	if err != nil {
		return nil, err
	}
	p, err := a.prog.Start(a.workDir, args...)
	if err != nil {
		return nil, err
	}
	return cli.NewRun(ctx, p, &stream{}), nil
}

// ContinueCommand resumes conversation id in the codex that ran it, with
// the user's own config and Capy's server.
func (a *agent) ContinueCommand(id string, tools gateway.Tools) ([]string, error) {
	path, ok := a.prog.Found()
	if !ok {
		return nil, gateway.ErrUnavailable
	}
	if err := uuid.Validate(id); err != nil {
		return nil, errors.New("codex: a conversation id is a UUID")
	}
	server, err := serverFlags(tools.Command)
	if err != nil {
		return nil, err
	}
	return append([]string{path, "resume"}, append(server, id)...), nil
}

// event is the part of a JSONL line the stream reads.
type event struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Item     struct {
		ID        string          `json:"id"`
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Server    string          `json:"server"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"item"`
	Message string `json:"message"`
	Error   struct {
		Message string `json:"message"`
	} `json:"error"`
}

// stream reads codex exec's JSONL: thread.started gives the thread,
// item.completed whole messages and the end of a tool call (item.started
// its start), and turn.completed or turn.failed the outcome. There are no
// text deltas. error lines are retries until turn.failed.
type stream struct {
	done, failed bool
	said         string // the last error, to classify
}

func (s *stream) Line(b []byte) []gateway.Event {
	var e event
	if json.Unmarshal(b, &e) != nil {
		return nil
	}
	switch e.Type {
	case "thread.started":
		if e.ThreadID != "" {
			return []gateway.Event{gateway.Started{ConversationID: e.ThreadID}}
		}
	case "item.started", "item.completed":
		switch e.Item.Type {
		case "agent_message":
			if e.Type == "item.completed" && e.Item.Text != "" {
				return []gateway.Event{gateway.Message{Text: e.Item.Text}}
			}
		case "mcp_tool_call":
			call := gateway.ToolCall{Name: e.Item.Tool, Done: e.Type == "item.completed"}
			if e.Item.Server != "capy" {
				call.Name = e.Item.Server + "." + e.Item.Tool
			}
			call.SessionID = sessionOf(e.Item.Arguments)
			return []gateway.Event{call}
		case "reasoning", "error", "todo_list", "":
		default:
			// Anything else codex did (command_execution, file_change,
			// web_search…) is a tool that is not Capy's: seen, by its kind.
			return []gateway.Event{gateway.ToolCall{Name: e.Item.Type, Done: e.Type == "item.completed"}}
		}
	case "error":
		s.said = e.Message
	case "turn.completed":
		s.done = true
	case "turn.failed":
		s.failed, s.said = true, e.Error.Message
	}
	return nil
}

// sessionOf is the session a tool call's arguments name, "" for none:
// they are decoded on their own, so that ones of another shape lose only
// this.
func sessionOf(args json.RawMessage) string {
	var in struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(args, &in) != nil || uuid.Validate(in.ID) != nil {
		return ""
	}
	return in.ID
}

func (s *stream) End(code int, stderr []byte) gateway.Event {
	switch {
	case s.done && !s.failed:
		return gateway.Done{}
	case s.failed:
		return failed(s.said, code)
	}
	return failed(s.said+"\n"+string(stderr), code)
}

// failed is why a run failed, from what codex said: turn.failed's error,
// or its stderr.
func failed(said string, code int) gateway.Failed {
	said = strings.ToLower(said)
	has := func(sub string) bool { return strings.Contains(said, sub) }
	switch {
	case has("401 unauthorized") || has("not logged in") || has("codex login"):
		return gateway.Failed{Kind: gateway.SignedOut, Code: code}
	case has("usage limit"):
		return gateway.Failed{Kind: gateway.UsageLimit, Code: code}
	case has("no rollout found"):
		return gateway.Failed{Kind: gateway.ResumeRejected, Code: code}
	}
	return gateway.Failed{Kind: gateway.Exited, Code: code}
}
