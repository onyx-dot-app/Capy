// Package cli finds and runs agents that are command-line tools. A Mac app
// started from the Dock has PATH=/usr/bin:/bin:/usr/sbin:/sbin, so an agent
// is looked for where the user chose, where its installers put it, and
// last in the user's login shell.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
)

// probeTimeout bounds each run of a candidate, and the login shell's
// lookup.
const probeTimeout = 5 * time.Second

// Spec is what finding an agent's command-line tool takes.
type Spec struct {
	// Name is the command as the login shell knows it: "claude".
	Name string
	// Known are where its installers put it, first match wins; "~/" is the
	// home directory.
	Known []string
	// VersionArgs make it print its version: ["--version"].
	VersionArgs []string
	// Minimum is the oldest version Capy works with, "2.1.0".
	Minimum string
}

// Env is what finding and running a tool depends on, to replace in tests.
type Env struct {
	Home string
	// Root is the folder the known absolute locations are under, "" for
	// the file system's root: tests set a folder of their own, so that no
	// install of the real tool is ever found or run.
	Root string
	// Shell is the login shell asked last: "/bin/zsh".
	Shell string
	// Timeout bounds each probe; 0 is 5 s.
	Timeout time.Duration
	// Run runs argv, never through a shell, with stdin from /dev/null, and
	// returns its standard output; a nonzero exit is an *ExitError.
	Run func(ctx context.Context, argv []string) ([]byte, error)
	// Path is the PATH of the programs Start runs, nil for Capy's own.
	Path func(ctx context.Context) string
}

// DefaultEnv is the user's: their home, zsh, and their login shell's
// PATH for the programs it runs.
func DefaultEnv() Env {
	home, _ := os.UserHomeDir()
	path := ShellPath("/bin/zsh")
	return Env{Home: home, Shell: "/bin/zsh", Run: ExecWithPath(path), Path: path}
}

// ShellPath is the PATH of shell's login, read once, on the first call: a
// Mac app started from the Dock has /usr/bin:/bin:/usr/sbin:/sbin, where a
// program installed with npm (#!/usr/bin/env node) finds no node. Without
// a usable answer it is "", Capy's own.
func ShellPath(shell string) func(ctx context.Context) string {
	var once sync.Once
	var path string
	return func(ctx context.Context) string {
		once.Do(func() { path = loginPath(ctx, shell) })
		return path
	}
}

// ExecWithPath is Exec with the PATH path gives.
func ExecWithPath(path func(ctx context.Context) string) func(ctx context.Context, argv []string) ([]byte, error) {
	return func(ctx context.Context, argv []string) ([]byte, error) { return run(ctx, argv, path(ctx)) }
}

// loginPath is the PATH of shell's interactive login, as a terminal has
// it, or "" when it gives none whose every entry is absolute.
func loginPath(ctx context.Context, shell string) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	out, err := run(ctx, []string{shell, "-lic", "print -r -- $PATH"}, "")
	if err != nil {
		return ""
	}
	// .zshrc may print before the answer: it is the last line.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	path := strings.TrimSpace(lines[len(lines)-1])
	for _, dir := range strings.Split(path, ":") {
		if !filepath.IsAbs(dir) {
			return ""
		}
	}
	return path
}

// ExitError is a tool that exited with Code. It holds no output: what an
// agent prints may quote the user's data.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return "cli: exit status " + strconv.Itoa(e.Code) }

// ExitCode implements gateway.ExitCoder.
func (e *ExitError) ExitCode() int { return e.Code }

// maxOutput is the most of a tool's standard output Exec keeps: version
// lines and `mcp get` answers are far smaller.
const maxOutput = 1 << 20

// Exec runs argv with stdin from /dev/null and returns its standard
// output, up to maxOutput. Standard error is dropped. An error names the
// program, never its folder, which may hold the user's name.
func Exec(ctx context.Context, argv []string) ([]byte, error) { return run(ctx, argv, "") }

// run is Exec with path as the program's PATH, "" for Capy's own.
func run(ctx context.Context, argv []string, path string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("cli: no command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = environ(path)
	out := &limited{max: maxOutput}
	cmd.Stdout = out
	cmd.WaitDelay = time.Second // a child keeping the pipe open
	err := cmd.Run()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok && ctx.Err() == nil {
		return out.Bytes(), &ExitError{Code: ee.ExitCode()}
	}
	if err != nil {
		return out.Bytes(), fmt.Errorf("cli: run %s: %w", filepath.Base(argv[0]), cause(err))
	}
	return out.Bytes(), nil
}

// environ is Capy's environment with path as PATH, Capy's own for "".
func environ(path string) []string {
	if path == "" {
		return nil
	}
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "PATH=") })
	return append(env, "PATH="+path)
}

// limited is a buffer that keeps the first max bytes written to it and
// drops the rest, so that a tool cannot fill memory. It holds its buffer
// as a field: an embedded one's ReadFrom would let io.Copy write past max.
type limited struct {
	buf bytes.Buffer
	max int
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		l.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (l *limited) Bytes() []byte { return l.buf.Bytes() }

// cause is err without the paths fs and exec errors name.
func cause(err error) error {
	if pe, ok := errors.AsType[*fs.PathError](err); ok {
		return pe.Err
	}
	if ee, ok := errors.AsType[*exec.Error](err); ok {
		return ee.Err
	}
	return err
}

// version is a version number, major.minor.patch.
type version [3]int

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// parseVersion finds the first version number in s, as "2.1.296 (Claude
// Code)" or "codex-cli 0.162.0-alpha.17.2" print it. A pre-release suffix
// is ignored.
func parseVersion(s string) (version, bool) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return version{}, false
	}
	var v version
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return version{}, false
		}
		v[i] = n
	}
	return v, true
}

func (v version) less(w version) bool {
	for i := range v {
		if v[i] != w[i] {
			return v[i] < w[i]
		}
	}
	return false
}

func (v version) String() string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// nameRe is the command names the login shell is asked about: never
// anything it would interpret.
var nameRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// Locate looks for spec's tool: at chosen, the path the user chose, if
// any; then where it is known to be; then as the login shell finds it.
// Each candidate must print a version within the timeout; one that fails
// (missing, broken, a script without its interpreter) is skipped. One
// older than the minimum is reported only when no later one is usable.
func Locate(ctx context.Context, env Env, spec Spec, chosen string) gateway.Probe {
	minimum, _ := parseVersion(spec.Minimum)
	var tooOld gateway.Probe
	tried := map[string]bool{}
	try := func(path string) (gateway.Probe, bool) {
		path = filepath.Clean(path)
		if path == "." || !filepath.IsAbs(path) || tried[path] {
			return gateway.Probe{}, false
		}
		tried[path] = true
		v, ok := runVersion(ctx, env, path, spec.VersionArgs)
		if !ok {
			return gateway.Probe{}, false
		}
		p := gateway.Probe{Found: true, Path: path, Version: v.String()}
		if v.less(minimum) {
			if !tooOld.Found {
				p.TooOld = minimum.String()
				tooOld = p
			}
			return gateway.Probe{}, false
		}
		return p, true
	}
	candidates := []string{}
	// A chosen path is tried only if it names the agent's own program, so
	// that an edited agent.json cannot have Capy run another.
	if chosen != "" && filepath.Base(chosen) == spec.Name {
		candidates = append(candidates, chosen)
	}
	for _, k := range spec.Known {
		if rest, ok := strings.CutPrefix(k, "~/"); ok {
			if env.Home == "" {
				continue
			}
			k = filepath.Join(env.Home, rest)
		} else if env.Root != "" {
			k = filepath.Join(env.Root, k)
		}
		candidates = append(candidates, k)
	}
	for _, c := range candidates {
		if p, ok := try(c); ok {
			return p
		}
	}
	if path := shellLookup(ctx, env, spec.Name); path != "" {
		if p, ok := try(path); ok {
			return p
		}
	}
	return tooOld
}

func timeout(env Env) time.Duration {
	if env.Timeout > 0 {
		return env.Timeout
	}
	return probeTimeout
}

// runVersion runs path with args and parses the version it prints.
func runVersion(ctx context.Context, env Env, path string, args []string) (version, bool) {
	ctx, cancel := context.WithTimeout(ctx, timeout(env))
	defer cancel()
	out, err := env.Run(ctx, append([]string{path}, args...))
	if err != nil {
		return version{}, false
	}
	return parseVersion(string(out))
}

// shellLookup asks the interactive login shell where name is, as nvm and
// the like set PATH up in .zshrc. It returns "" when it is not there.
func shellLookup(ctx context.Context, env Env, name string) string {
	if env.Shell == "" || !nameRe.MatchString(name) {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, timeout(env))
	defer cancel()
	out, err := env.Run(ctx, []string{env.Shell, "-lic", "command -v " + name})
	if err != nil {
		return ""
	}
	// What .zshrc prints comes first; the answer is the last line.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); filepath.IsAbs(last) {
		return last
	}
	return ""
}

// Program is an agent's command-line tool, as its adapter uses it: found
// with Locate, then run at the path found.
type Program struct {
	Env  Env
	Spec Spec
	// Chosen is the path the user chose for it, or nil.
	Chosen func() string

	mu   sync.Mutex
	last gateway.Probe
}

// Probe locates the program and keeps what it found for Run.
func (p *Program) Probe(ctx context.Context) gateway.Probe {
	chosen := ""
	if p.Chosen != nil {
		chosen = p.Chosen()
	}
	found := Locate(ctx, p.Env, p.Spec, chosen)
	p.mu.Lock()
	p.last = found
	p.mu.Unlock()
	return found
}

// Path is where the last probe found the program, else its name.
func (p *Program) Path() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last.Found {
		return p.last.Path
	}
	return p.Spec.Name
}

// Run runs the program found with args. A program not found, or too old,
// is gateway.ErrUnavailable.
func (p *Program) Run(ctx context.Context, args ...string) ([]byte, error) {
	p.mu.Lock()
	last := p.last
	p.mu.Unlock()
	if !last.Usable() {
		return nil, gateway.ErrUnavailable
	}
	return p.Env.Run(ctx, append([]string{last.Path}, args...))
}
