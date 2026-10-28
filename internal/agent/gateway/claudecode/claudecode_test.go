package claudecode

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/cli"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/internal/faketool"
)

const self = "/Applications/Capy app/Capy.app/Contents/MacOS/Capy"

type memory struct{ connected bool }

func (m *memory) Connected() bool           { return m.connected }
func (m *memory) SetConnected(c bool) error { m.connected = c; return nil }

// installed is Claude Code at its first known location in a home of its
// own.
func installed(t *testing.T, version string) (gateway.Agent, string, *memory) {
	t.Helper()
	env := cli.Env{Home: t.TempDir(), Root: t.TempDir(), Run: cli.Exec, Timeout: 10 * time.Second}
	tool := faketool.Tool(t, filepath.Join(env.Home, ".local/bin/claude"), version)
	m := &memory{}
	return New(Options{Env: env, Self: self, Memory: m}), tool, m
}

func TestProbeFindsClaudeCodeAndItsVersion(t *testing.T) {
	a, tool, _ := installed(t, "2.1.296 (Claude Code)")
	if p := a.Probe(context.Background()); !p.Usable() || p.Path != tool || p.Version != "2.1.296" {
		t.Fatalf("probe %+v", p)
	}
	old, _, _ := installed(t, "2.0.4 (Claude Code)")
	if p := old.Probe(context.Background()); !p.Found || p.TooOld != "2.1.0" {
		t.Fatalf("an old Claude Code: %+v, want it too old for 2.1.0", p)
	}
	if info := a.Info(); info.ID != "claude-code" || info.Sends == nil || info.Sends.Provider != "Anthropic" {
		t.Fatalf("info %+v: it sends to Anthropic and must say so", info)
	}
}

func TestConnectRunsClaudesOwnCommandAndRemembersIt(t *testing.T) {
	a, tool, m := installed(t, "2.1.296 (Claude Code)")
	c := a.(gateway.Connector)
	ctx := context.Background()
	if err := c.Connect(ctx); !errors.Is(err, gateway.ErrUnavailable) {
		t.Fatalf("connect before a probe: %v", err)
	}
	a.Probe(ctx)
	// An entry Capy cannot see goes first: claude mcp add refuses one.
	remove := []string{tool, "mcp", "remove", "-s", "user", "capy"}
	want := []string{tool, "mcp", "add", "-s", "user", "capy", "--", self, "mcp"}
	if got := c.ConnectCommands(); len(got) != 2 || !slices.Equal(got[0], remove) || !slices.Equal(got[1], want) {
		t.Fatalf("connect commands %q, want %q then %q", got, remove, want)
	}
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	calls := faketool.Calls(t, tool)
	if n := len(calls); !slices.Equal(calls[n-2], remove[1:]) || !slices.Equal(calls[n-1], want[1:]) {
		t.Fatalf("ran %q, want %q then %q", calls, remove[1:], want[1:])
	}
	if conn, err := c.Connection(ctx); err != nil || conn != gateway.Connected || !m.connected {
		t.Fatalf("after Connect: %v, %v", conn, err)
	}
	// Never asked with mcp get, which would start Capy's server.
	for _, call := range faketool.Calls(t, tool) {
		if len(call) > 1 && call[0] == "mcp" && call[1] == "get" {
			t.Fatal("Connection ran claude mcp get")
		}
	}
	if err := c.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	calls = faketool.Calls(t, tool)
	if got := calls[len(calls)-1]; !slices.Equal(got, []string{"mcp", "remove", "-s", "user", "capy"}) || !slices.Equal(c.DisconnectCommand()[1:], got) {
		t.Fatalf("disconnect ran %q", got)
	}
	if conn, _ := c.Connection(ctx); conn != gateway.NotConnected {
		t.Fatalf("after Disconnect: %v", conn)
	}
}

func TestDisconnectOfAnEntryRemovedByHand(t *testing.T) {
	a, tool, m := installed(t, "2.1.296 (Claude Code)")
	c := a.(gateway.Connector)
	a.Probe(context.Background())
	m.connected = true
	faketool.Set(t, tool, "exit", "1") // claude mcp remove: no such server
	if err := c.Disconnect(context.Background()); err != nil || m.connected {
		t.Fatalf("disconnect: %v, connected %v; want it disconnected", err, m.connected)
	}
	faketool.Set(t, tool, "exit", "2")
	m.connected = true
	if err := c.Disconnect(context.Background()); err == nil || !m.connected {
		t.Fatal("a Disconnect that failed otherwise forgot the connection")
	}
}

func TestAFailedConnectKeepsTheConnectionAndGivesTheExitCode(t *testing.T) {
	a, tool, m := installed(t, "2.1.296 (Claude Code)")
	c := a.(gateway.Connector)
	a.Probe(context.Background())
	faketool.Set(t, tool, "exit", "1")
	err := c.Connect(context.Background())
	if ee, ok := errors.AsType[*cli.ExitError](err); !ok || ee.Code != 1 {
		t.Fatalf("connect: %v, want exit 1", err)
	}
	if m.connected {
		t.Fatal("a failed Connect was remembered as connected")
	}
}
