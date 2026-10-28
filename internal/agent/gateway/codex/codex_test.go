package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/cli"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/internal/faketool"
)

const self = "/Applications/Capy app/Capy.app/Contents/MacOS/Capy"

// installed is a Codex the user chose, in a home of its own.
func installed(t *testing.T) (*agent, string) {
	t.Helper()
	env := cli.Env{Home: t.TempDir(), Root: t.TempDir(), Run: cli.Exec, Timeout: 10 * time.Second}
	tool := faketool.Tool(t, filepath.Join(t.TempDir(), "codex"), "codex-cli 0.162.0-alpha.17.2")
	a := New(Options{Env: env, Self: self, Chosen: func() string { return tool }}).(*agent)
	if p := a.Probe(context.Background()); !p.Usable() || p.Version != "0.162.0" {
		t.Fatalf("probe %+v", p)
	}
	return a, tool
}

// get is `codex mcp get capy --json` as Codex 0.162 prints it.
func get(command string, args ...string) string {
	return `{"name":"capy","enabled":true,"disabled_reason":null,"transport":{"type":"stdio","command":"` + command +
		`","args":["` + strings.Join(args, `","`) + `"],"env":null,"env_vars":[],"cwd":null},"enabled_tools":null}`
}

func TestConnectionReadsCodexsServer(t *testing.T) {
	a, tool := installed(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name, get string
		want      gateway.Connection
	}{
		{"none", "", gateway.NotConnected},
		{"this Capy", get(self, "mcp"), gateway.Connected},
		{"a Capy elsewhere", get("/Users/me/Downloads/Capy.app/Contents/MacOS/Capy", "mcp"), gateway.Moved},
		{"other arguments", get(self, "serve"), gateway.Moved},
	} {
		os.Remove(tool + ".get")
		if tc.get != "" {
			faketool.Set(t, tool, "get", tc.get)
		}
		if got, err := a.Connection(ctx); err != nil || got != tc.want {
			t.Errorf("%s: %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	calls := faketool.Calls(t, tool)
	if got := calls[len(calls)-1]; !slices.Equal(got, []string{"mcp", "get", "capy", "--json"}) {
		t.Fatalf("connection ran %q", got)
	}
	faketool.Set(t, tool, "get", "not json")
	if _, err := a.Connection(ctx); err == nil || strings.Contains(err.Error(), "not json") {
		t.Fatalf("unreadable output: %v; want an error without the output", err)
	}
}

func TestConnectAndDisconnectRunCodexsOwnCommands(t *testing.T) {
	a, tool := installed(t)
	ctx := context.Background()
	want := []string{tool, "mcp", "add", "capy", "--", self, "mcp"}
	if got := a.ConnectCommands(); len(got) != 1 || !slices.Equal(got[0], want) {
		t.Fatalf("connect command %q, want %q", got, want)
	}
	if err := a.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	calls := faketool.Calls(t, tool)
	if n := len(calls); n < 2 || !slices.Equal(calls[n-2], want[1:]) || !slices.Equal(calls[n-1], []string{"mcp", "remove", "capy"}) ||
		!slices.Equal(a.DisconnectCommand()[1:], calls[n-1]) {
		t.Fatalf("ran %q", calls)
	}
	faketool.Set(t, tool, "exit", "4")
	if ee, ok := errors.AsType[*cli.ExitError](a.Connect(ctx)); !ok || ee.Code != 4 {
		t.Fatal("a failed connect does not give its exit code")
	}
}

func TestDefaultModelReadsCodexsConfig(t *testing.T) {
	a, _ := installed(t)
	ctx := context.Background()
	if m, err := a.DefaultModel(ctx); m != "" || err != nil {
		t.Fatalf("no config: %q, %v", m, err)
	}
	cfg := filepath.Join(a.home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(s string) {
		if err := os.WriteFile(cfg, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("model = \"gpt-6.1-sol\"\n[mcp_servers.capy]\ncommand = \"/x\"\n")
	if m, err := a.DefaultModel(ctx); m != "gpt-6.1-sol" || err != nil {
		t.Fatalf("config: %q, %v", m, err)
	}
	write("[profiles.work]\nmodel = \"other\"\n") // no top-level model
	if m, err := a.DefaultModel(ctx); m != "" || err != nil {
		t.Fatalf("no top-level model: %q, %v", m, err)
	}
	write("api_key = \"sk-secret\nmodel = ")
	if _, err := a.DefaultModel(ctx); err == nil || strings.Contains(err.Error(), "sk-secret") {
		t.Fatalf("malformed config: %v; want an error without its content", err)
	}
}

func TestModelsListsTheBundledCatalogWithTheDefault(t *testing.T) {
	a, tool := installed(t)
	ctx := context.Background()
	faketool.Set(t, tool, "debug", `{"models":[
		{"slug":"gpt-6-astra","visibility":"list","priority":2},
		{"slug":"gpt-6.1-sol","visibility":"list","priority":1},
		{"slug":"internal","visibility":"hide","priority":0}]}`)
	got, err := a.Models(ctx)
	if err != nil || !slices.Equal(got, []string{"gpt-6.1-sol", "gpt-6-astra"}) {
		t.Fatalf("models %q, %v; want the listed ones by priority", got, err)
	}
	calls := faketool.Calls(t, tool)
	if last := calls[len(calls)-1]; !slices.Equal(last, []string{"debug", "models", "--bundled"}) {
		t.Fatalf("ran %q; want the bundled catalog, without the network", last)
	}
	// A default the catalog lacks comes first; without a catalog, alone.
	cfg := filepath.Join(a.home, ".codex", "config.toml")
	os.MkdirAll(filepath.Dir(cfg), 0o700)
	os.WriteFile(cfg, []byte(`model = "my-model"`), 0o600)
	if got, _ := a.Models(ctx); len(got) != 3 || got[0] != "my-model" {
		t.Fatalf("with a default outside the catalog: %q", got)
	}
	faketool.Set(t, tool, "debug", "garbage")
	if got, err := a.Models(ctx); err != nil || !slices.Equal(got, []string{"my-model"}) {
		t.Fatalf("without a catalog: %q, %v", got, err)
	}
}

func TestInfoSaysWhereQuestionsGo(t *testing.T) {
	a, _ := installed(t)
	if info := a.Info(); info.ID != "codex" || info.Name != "ChatGPT" || info.Streams || info.Sends == nil || info.Sends.Provider != "OpenAI" {
		t.Fatalf("info %+v", info)
	}
}
