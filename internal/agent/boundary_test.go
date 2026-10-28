package agent

import (
	"encoding/json"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const module = "github.com/onyx-dot-app/Capy"

// pkg is a package of the module as go list gives it.
type pkg struct {
	ImportPath string
	Imports    []string
}

// packages lists the module's packages with what each imports, tests
// left out: the boundary is what the built app links.
func packages(t *testing.T) []pkg {
	t.Helper()
	cmd := exec.Command("go", "list", "-json=ImportPath,Imports", module+"/...")
	cmd.Dir = "../.."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	var list []pkg
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var p pkg
		if err := dec.Decode(&p); err != nil {
			t.Fatal(err)
		}
		list = append(list, p)
	}
	return list
}

// rel is path within the module, "" for the module itself (main).
func rel(path string) (string, bool) {
	if path == module {
		return "", true
	}
	r, ok := strings.CutPrefix(path, module+"/")
	return r, ok
}

func under(path, dir string) bool { return path == dir || strings.HasPrefix(path, dir+"/") }

// allowed is what of Capy internal/agent may import (design.md › Module
// boundary).
var allowed = []string{"internal/store", "internal/profile", "internal/features", "internal/views/addon", "internal/views/kit", "internal/lang"}

// TestModuleBoundary keeps Agent an add-on: only main imports it, and it
// imports of Capy only what its design allows. Removing internal/agent and
// its lines in main.go leaves a Capy that builds.
func TestModuleBoundary(t *testing.T) {
	list := packages(t)
	if len(list) < 10 {
		t.Fatalf("go list found %d packages: is it run from the module?", len(list))
	}
	for _, p := range list {
		from, ok := rel(p.ImportPath)
		if !ok {
			continue
		}
		for _, imp := range p.Imports {
			to, ok := rel(imp)
			if !ok {
				continue
			}
			inAgent, toAgent := under(from, "internal/agent"), under(to, "internal/agent")
			switch {
			case under(to, fakeAgent) && !under(from, fakeAgent):
				t.Errorf("%s imports %s: the fake agent is for tests", from, to)
			case toAgent && !inAgent && from != "":
				t.Errorf("%s imports %s: only main and internal/agent may", from, to)
			case inAgent && !toAgent && !slices.ContainsFunc(allowed, func(a string) bool { return to == a }):
				t.Errorf("%s imports %s: Agent may import only %v of Capy", from, to, allowed)
			case inAgent && toAgent:
				if err := agentImport(from, to); err != "" {
					t.Errorf("%s imports %s: %s", from, to, err)
				}
			}
		}
	}
}

// fakeAgent is the scripted agent of tests, and fakeagent's command.
const fakeAgent = "internal/agent/gateway/fake"

// agentImport checks an import within internal/agent: the adapters reach
// only the gateway and its CLI support, never each other, a capability or
// the UI; the UI and the capabilities reach agents only through the
// gateway's contract.
func agentImport(from, to string) string {
	const gw = "internal/agent/gateway"
	// adapter reports whether path is an agent's adapter: a package under
	// the gateway but its CLI support and test helpers.
	adapter := func(path string) bool {
		return strings.HasPrefix(path, gw+"/") && !under(path, gw+"/cli") && !under(path, gw+"/internal")
	}
	switch {
	case under(from, gw) && !under(to, gw):
		return "the gateway and its adapters import nothing else of Agent"
	case adapter(from) && adapter(to) && !under(to, from) && !under(from, to):
		return "an adapter imports only the gateway and gateway/cli"
	case under(to, gw+"/internal"):
		return "the gateway's test helpers are for tests"
	case under(from, "internal/agent/ui") && strings.HasPrefix(to, gw+"/"):
		return "the UI knows no adapter, nor how they run: it lists agents through the gateway"
	case under(from, "internal/agent/ask") && strings.HasPrefix(to, gw+"/"):
		return "a capability knows no adapter: it asks agents through the gateway"
	case under(from, "internal/agent/tools"):
		return "the tools reach only the library"
	}
	return ""
}

func TestModuleBoundaryRefusesWhatTheDesignForbids(t *testing.T) {
	for _, c := range []struct {
		from, to string
		refused  bool
	}{
		{"internal/agent/gateway/codex", "internal/agent/gateway", false},
		{"internal/agent/gateway/codex", "internal/agent/gateway/cli", false},
		{"internal/agent/gateway/codex", "internal/agent/gateway/claudecode", true},
		{"internal/agent/gateway/codex", "internal/agent/ask", true},
		{"internal/agent/gateway", "internal/agent/threads", true},
		{"internal/agent/ui", "internal/agent/gateway", false},
		{"internal/agent/ui", "internal/agent/gateway/claudecode", true},
		{"internal/agent/ui", "internal/agent/gateway/cli", true},
		{"internal/agent/gateway/codex", "internal/agent/gateway/internal/faketool", true},
		{"internal/agent/tools", "internal/agent/threads", true},
		{"internal/agent", "internal/agent/gateway/codex", false},
		{"internal/agent/gateway/fake/cmd/fakeagent", "internal/agent/gateway/fake", false},
		{"internal/agent/gateway/fake", "internal/agent/gateway/codex", true},
		{"internal/agent/ask", "internal/agent/gateway", false},
		{"internal/agent/ask", "internal/agent/gateway/claudecode", true},
		{"internal/agent/ui", "internal/agent/ask", false},
	} {
		if got := agentImport(c.from, c.to) != ""; got != c.refused {
			t.Errorf("%s → %s: refused %v, want %v", c.from, c.to, got, c.refused)
		}
	}
}
