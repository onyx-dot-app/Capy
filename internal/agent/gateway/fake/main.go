package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
)

// Versions fakeagent reports, as claude and codex print them.
const (
	ClaudeVersion = "2.1.296 (Claude Code)"
	CodexVersion  = "codex-cli 0.162.0"
)

// invocation is what fakeagent read of its arguments.
type invocation struct {
	codex        bool
	command      []string // Capy's tools, nil for a run with none
	conversation string   // resumed
	prompt       string
}

// parse reads claude's or codex's arguments, as the adapters build them.
func parse(args []string) (invocation, error) {
	var inv invocation
	dash := slices.Index(args, "--")
	if dash < 0 || dash != len(args)-2 {
		return inv, fmt.Errorf("fakeagent: the prompt must follow --")
	}
	inv.prompt = args[dash+1]
	flags := args[:dash]
	value := func(name string) string {
		if i := slices.Index(flags, name); i >= 0 && i+1 < len(flags) {
			return flags[i+1]
		}
		return ""
	}
	if len(flags) > 0 && flags[0] == "exec" {
		inv.codex = true
		var command string
		var rest []string
		for i, f := range flags {
			if f != "-c" || i+1 >= len(flags) {
				continue
			}
			if v, ok := strings.CutPrefix(flags[i+1], "mcp_servers.capy.command="); ok && json.Unmarshal([]byte(v), &command) != nil {
				return inv, fmt.Errorf("fakeagent: bad command")
			}
			if v, ok := strings.CutPrefix(flags[i+1], "mcp_servers.capy.args="); ok && json.Unmarshal([]byte(v), &rest) != nil {
				return inv, fmt.Errorf("fakeagent: bad args")
			}
		}
		if command != "" {
			inv.command = append([]string{command}, rest...)
		}
		if len(flags) > 1 && flags[1] == "resume" {
			inv.conversation = flags[len(flags)-1]
		}
		return inv, nil
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(value("--mcp-config")), &cfg); err != nil {
		return inv, fmt.Errorf("fakeagent: bad --mcp-config")
	}
	if capy, ok := cfg.MCPServers["capy"]; ok {
		inv.command = append([]string{capy.Command}, capy.Args...)
	}
	inv.conversation = value("--resume")
	return inv, nil
}

// Main is fakeagent: it answers --version as claude, or as codex when
// codex is set; else it takes either one's run arguments, plays the
// script $CAPY_FAKEAGENT_SCRIPT names against the Capy mcp command they
// give, and prints that CLI's stream on stdout. It returns the exit
// status.
func Main(args []string, codex bool, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--version" {
		if codex {
			fmt.Fprintln(stdout, CodexVersion)
		} else {
			fmt.Fprintln(stdout, ClaudeVersion)
		}
		return 0
	}
	inv, err := parse(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	s, err := ReadScript(os.Getenv(ScriptEnv))
	if err != nil {
		fmt.Fprintln(stderr, "fakeagent: no script")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	p := &printer{w: stdout, codex: inv.codex}
	a := &Agent{Script: s}
	end := a.answer(ctx, gateway.Query{Prompt: inv.prompt, ConversationID: inv.conversation,
		NoTools: inv.command == nil, Tools: gateway.Tools{Command: inv.command}}, p.event)
	return p.end(end, inv.conversation, stderr)
}

// printer prints events as claude's stream-json or codex's JSONL.
type printer struct {
	w     io.Writer
	codex bool
	n     int // tool calls, for their IDs
}

func (p *printer) line(v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(p.w, "%s\n", b)
}

type obj = map[string]any

func (p *printer) event(e gateway.Event) {
	switch e := e.(type) {
	case gateway.Started:
		if p.codex {
			p.line(obj{"type": "thread.started", "thread_id": e.ConversationID})
			p.line(obj{"type": "turn.started"})
		} else {
			p.line(obj{"type": "system", "subtype": "init", "session_id": e.ConversationID})
		}
	case gateway.TextDelta:
		if !p.codex {
			p.line(obj{"type": "stream_event", "event": obj{"type": "content_block_delta", "delta": obj{"type": "text_delta", "text": e.Text}}})
		}
	case gateway.Message:
		if p.codex {
			p.line(obj{"type": "item.completed", "item": obj{"id": "item_msg", "type": "agent_message", "text": e.Text}})
		} else {
			p.line(obj{"type": "assistant", "message": obj{"content": []obj{{"type": "text", "text": e.Text}}}})
		}
	case gateway.ToolCall:
		if !e.Done {
			p.n++
		}
		id := fmt.Sprintf("call_%d", p.n)
		switch {
		case p.codex:
			kind := "item.started"
			if e.Done {
				kind = "item.completed"
			}
			p.line(obj{"type": kind, "item": obj{"id": id, "type": "mcp_tool_call", "server": "capy", "tool": e.Name, "arguments": obj{"id": e.SessionID}}})
		case e.Done:
			p.line(obj{"type": "user", "message": obj{"content": []obj{{"type": "tool_result", "tool_use_id": id, "content": "{}"}}}})
		default:
			p.line(obj{"type": "assistant", "message": obj{"content": []obj{{"type": "tool_use", "id": id, "name": "mcp__capy__" + e.Name, "input": obj{"id": e.SessionID}}}}})
		}
	}
}

// end prints the run's end as the CLI does, and returns its exit status.
func (p *printer) end(e gateway.Event, conversation string, stderr io.Writer) int {
	f, failed := e.(gateway.Failed)
	if !failed {
		if p.codex {
			p.line(obj{"type": "turn.completed"})
		} else {
			p.line(obj{"type": "result", "subtype": "success", "is_error": false})
		}
		return 0
	}
	switch {
	case f.Kind == gateway.ResumeRejected && p.codex:
		fmt.Fprintln(stderr, "Error: thread/resume: thread/resume failed: no rollout found for thread id "+conversation)
	case f.Kind == gateway.ResumeRejected:
		p.line(obj{"type": "result", "subtype": "error_during_execution", "is_error": true, "errors": []string{"No conversation found with session ID: " + conversation}})
	case p.codex:
		msg := "stream disconnected"
		switch f.Kind {
		case gateway.SignedOut:
			msg = "unexpected status 401 Unauthorized: Missing bearer or basic authentication in header"
		case gateway.UsageLimit:
			msg = "You've hit your usage limit. Try again later."
		}
		p.line(obj{"type": "turn.failed", "error": obj{"message": msg}})
	default:
		kind := map[gateway.Failure]string{gateway.SignedOut: "authentication_failed", gateway.UsageLimit: "rate_limit"}[f.Kind]
		if kind != "" {
			p.line(obj{"type": "assistant", "error": kind, "message": obj{"content": []obj{{"type": "text", "text": "Not answered"}}}})
		}
		p.line(obj{"type": "result", "subtype": "success", "is_error": true})
	}
	return 1
}
