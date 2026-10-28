package cli

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
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/internal/faketool"
)

func spec(known ...string) Spec {
	return Spec{Name: "claude", Known: known, VersionArgs: []string{"--version"}, Minimum: "2.1.0"}
}

// env runs tools for real, with a home of its own and no login shell.
func env(t *testing.T) Env {
	t.Helper()
	return Env{Home: t.TempDir(), Run: Exec, Timeout: 10 * time.Second} // known paths here are the test's own
}

func TestParseVersionReadsBothCLIs(t *testing.T) {
	for out, want := range map[string]version{
		"2.1.296 (Claude Code)\n":        {2, 1, 296},
		"codex-cli 0.162.0-alpha.17.2\n": {0, 162, 0},
	} {
		if got, ok := parseVersion(out); !ok || got != want {
			t.Errorf("parseVersion(%q) = %v, %v; want %v", out, got, ok, want)
		}
	}
	if _, ok := parseVersion("command not found"); ok {
		t.Error("a version parsed from text without one")
	}
	// Numbers compare as numbers, and a pre-release counts as its release.
	if v, _ := parseVersion("0.162.0-alpha.17.2"); v.less(version{0, 162, 0}) {
		t.Error("0.162.0-alpha below the 0.162.0 minimum")
	}
	if !(version{2, 9, 0}).less(version{2, 10, 0}) {
		t.Error("2.9.0 not below 2.10.0")
	}
}

func TestLocateTriesTheChosenPathThenKnownOnes(t *testing.T) {
	e := env(t)
	home := faketool.Tool(t, filepath.Join(e.Home, ".local/bin/claude"), "2.1.296 (Claude Code)")
	brew := faketool.Tool(t, filepath.Join(t.TempDir(), "brew/claude"), "2.1.300 (Claude Code)")
	chosen := faketool.Tool(t, filepath.Join(t.TempDir(), "my tools/claude"), "2.2.0 (Claude Code)")
	s := spec("~/.local/bin/claude", brew)

	if p := Locate(context.Background(), e, s, chosen); !p.Usable() || p.Path != chosen || p.Version != "2.2.0" {
		t.Fatalf("with a chosen path: %+v, want it", p)
	}
	if p := Locate(context.Background(), e, s, ""); p.Path != home || p.Version != "2.1.296" {
		t.Fatalf("without: %+v, want the first known location", p)
	}
	if p := Locate(context.Background(), e, spec(filepath.Join(t.TempDir(), "none")), ""); p.Found {
		t.Fatalf("nothing installed: %+v", p)
	}
}

func TestLocateSkipsBrokenCandidates(t *testing.T) {
	e := env(t)
	dir := t.TempDir()
	good := faketool.Tool(t, filepath.Join(dir, "good/claude"), "2.1.296 (Claude Code)")
	// A Node script without node, a broken symlink, one that prints no
	// version and one that hangs.
	script := faketool.Write(t, filepath.Join(dir, "npm/claude"), "#!/usr/bin/env capy-no-such-node\n")
	link := filepath.Join(dir, "link/claude")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "gone"), link); err != nil {
		t.Fatal(err)
	}
	mute := faketool.Write(t, filepath.Join(dir, "mute/claude"), "#!/bin/sh\necho hello\n")
	hang := faketool.Write(t, filepath.Join(dir, "hang/claude"), "#!/bin/sh\nexec sleep 30\n")
	e.Timeout = time.Second

	start := time.Now()
	p := Locate(context.Background(), e, spec(script, link, mute, hang, good), "")
	if !p.Usable() || p.Path != good {
		t.Fatalf("found %+v, want the one that works", p)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("a hanging candidate held the probe for %v", d)
	}
}

func TestLocateReportsTooOldOnlyWithoutAGoodOne(t *testing.T) {
	e := env(t)
	dir := t.TempDir()
	old := faketool.Tool(t, filepath.Join(dir, "old/claude"), "2.0.4 (Claude Code)")
	good := faketool.Tool(t, filepath.Join(dir, "good/claude"), "2.1.0 (Claude Code)")

	if p := Locate(context.Background(), e, spec(old, good), ""); !p.Usable() || p.Path != good {
		t.Fatalf("an old one before a good one: %+v, want the good one", p)
	}
	p := Locate(context.Background(), e, spec(old), "")
	if !p.Found || p.Usable() || p.TooOld != "2.1.0" || p.Version != "2.0.4" || p.Path != old {
		t.Fatalf("only an old one: %+v, want it found, too old for 2.1.0", p)
	}
}

func TestLocateAsksTheLoginShellLast(t *testing.T) {
	e := env(t)
	dir := t.TempDir()
	nvm := faketool.Tool(t, filepath.Join(dir, "nvm/bin/claude"), "2.1.296 (Claude Code)")
	// .zshrc may print before the answer.
	e.Shell = faketool.Write(t, filepath.Join(dir, "zsh"),
		"#!/bin/sh\nprintf '%s|' \"$@\" > \"$0.args\"\necho 'Welcome back'\necho "+nvm+"\n")

	p := Locate(context.Background(), e, spec(filepath.Join(dir, "none")), "")
	if !p.Usable() || p.Path != nvm {
		t.Fatalf("found %+v, want the login shell's", p)
	}
	args, err := os.ReadFile(e.Shell + ".args")
	if err != nil || string(args) != "-lic|command -v claude|" {
		t.Fatalf("shell ran with %q, %v", args, err)
	}
	// A name the shell could interpret is never asked about.
	s := spec()
	s.Name = "claude; rm -rf ~"
	os.Remove(e.Shell + ".args")
	if p := Locate(context.Background(), e, s, ""); p.Found {
		t.Fatalf("found %+v for an unsafe name", p)
	}
	if _, err := os.Stat(e.Shell + ".args"); err == nil {
		t.Fatal("the shell was asked about an unsafe name")
	}
}

func TestProgramRunsTheToolFoundWithArgv(t *testing.T) {
	e := env(t)
	tool := faketool.Tool(t, filepath.Join(t.TempDir(), "claude"), "2.1.296 (Claude Code)")
	p := &Program{Env: e, Spec: spec(tool)}
	if _, err := p.Run(context.Background(), "mcp", "list"); !errors.Is(err, gateway.ErrUnavailable) {
		t.Fatalf("run before a probe: %v, want ErrUnavailable", err)
	}
	if p.Path() != "claude" {
		t.Fatalf("path before a probe %q, want the name", p.Path())
	}
	p.Probe(context.Background())
	faketool.Set(t, tool, "exit", "3")
	_, err := p.Run(context.Background(), "mcp", "add", "/Applications/Capy app/Capy")
	if ee, ok := errors.AsType[*ExitError](err); !ok || ee.Code != 3 {
		t.Fatalf("run: %v, want exit 3", err)
	}
	calls := faketool.Calls(t, tool)
	if last := calls[len(calls)-1]; !slices.Equal(last, []string{"mcp", "add", "/Applications/Capy app/Capy"}) {
		t.Fatalf("ran with %q, want the arguments as given", last)
	}
}

// TestRootKeepsTestsFromRealInstalls looks up a program that exists at its
// absolute known location on every Mac: under a Root of the test's own,
// it is not found, so a test never probes or runs a real install.
func TestRootKeepsTestsFromRealInstalls(t *testing.T) {
	spec := Spec{Name: "sh", Known: []string{"/bin/sh"}, VersionArgs: []string{"-c", "echo 1.0.0"}, Minimum: "0.0.1"}
	if p := Locate(context.Background(), Env{Run: Exec, Timeout: 10 * time.Second}, spec, ""); !p.Found {
		t.Skipf("/bin/sh not usable here: %+v", p)
	}
	e := env(t)
	e.Root = t.TempDir()
	if p := Locate(context.Background(), e, spec, ""); p.Found {
		t.Fatalf("found %+v under a Root of the test's own", p)
	}
}

// TestChosenPathMustBeTheAgentsProgram keeps an edited agent.json from
// having Capy run another program as the agent.
func TestChosenPathMustBeTheAgentsProgram(t *testing.T) {
	other := faketool.Tool(t, filepath.Join(t.TempDir(), "evil"), "2.1.296 (Claude Code)")
	if p := Locate(context.Background(), env(t), spec(), other); p.Found {
		t.Fatalf("ran a chosen program not named claude: %+v", p)
	}
	if calls := faketool.Calls(t, other); len(calls) != 0 {
		t.Fatalf("the other program ran: %q", calls)
	}
}

func TestExecKeepsFoldersOutOfErrorsAndBoundsOutput(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Users", "ana")
	noexec := faketool.Write(t, filepath.Join(dir, "claude"), "#!/bin/sh\necho hi\n")
	if err := os.Chmod(noexec, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Exec(context.Background(), []string{noexec})
	if err == nil || strings.Contains(err.Error(), dir) {
		t.Fatalf("error %v; want one naming no folder", err)
	}
	loud := faketool.Write(t, filepath.Join(dir, "loud"), "#!/bin/sh\nhead -c 3000000 /dev/zero\n")
	out, err := Exec(context.Background(), []string{loud})
	if err != nil || len(out) != maxOutput {
		t.Fatalf("kept %d bytes, %v; want %d", len(out), err, maxOutput)
	}
}

// TestAnNpmInstallRunsWithTheLoginShellsPath finds a program installed
// with npm, whose #!/usr/bin/env node finds node only on the PATH the
// user's shell sets, not the Dock's.
func TestAnNpmInstallRunsWithTheLoginShellsPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "nvm", "bin")
	faketool.Write(t, filepath.Join(bin, "capy-fake-node"), "#!/bin/sh\necho '2.1.296 (Claude Code)'\n")
	tool := faketool.Write(t, filepath.Join(bin, "claude"), "#!/usr/bin/env capy-fake-node\n")
	shell := faketool.Write(t, filepath.Join(dir, "zsh"), "#!/bin/sh\necho 'Welcome back'\necho '"+bin+":/usr/bin:/bin'\n")
	e := env(t)
	if p := Locate(context.Background(), e, spec(tool), ""); p.Found {
		t.Skipf("found without the login PATH, %+v: capy-fake-node is on this PATH", p)
	}
	e.Run = ExecWithPath(ShellPath(shell))
	if p := Locate(context.Background(), e, spec(tool), ""); !p.Usable() || p.Version != "2.1.296" {
		t.Fatalf("with the login PATH: %+v, want it found", p)
	}
}
