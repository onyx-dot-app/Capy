package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/internal/faketool"
)

// script writes a /bin/sh script of body to a folder of the test's own.
func script(t *testing.T, body string) string {
	t.Helper()
	return faketool.Write(t, filepath.Join(t.TempDir(), "agent"), "#!/bin/sh\n"+body+"\n")
}

// ended is how a process ended: Wait's results.
type ended struct {
	code   int
	stderr []byte
}

// started starts body and reads it as NewRun does, Lines then Wait, in a
// goroutine: the first line comes on first, the end on end.
func started(t *testing.T, body string) (p *Process, first <-chan string, end <-chan ended) {
	t.Helper()
	p, err := Start(Env{}, []string{script(t, body)}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	firstc, endc := make(chan string, 1), make(chan ended, 1)
	go func() {
		p.Lines(func(line []byte) {
			select {
			case firstc <- string(line):
			default:
			}
		})
		code, stderr := p.Wait()
		endc <- ended{code, stderr}
	}()
	return p, firstc, endc
}

func receive[T any](t *testing.T, c <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-c:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("no %s within 10 s", what)
		panic("unreachable")
	}
}

// pid reads the pid a script printed.
func pid(t *testing.T, line string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n <= 1 {
		t.Fatalf("first line %q, want a child's pid", line)
	}
	return n
}

// gone reports whether pid has exited and been reaped, within 3 s.
func gone(pid int) bool {
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
	}
	return false
}

// TestStopEndsTheProgramsWholeGroup guards against a Stop that ends only
// the agent's program, leaving what it started, such as Capy mcp, running;
// and against one that waits forever on a program ignoring SIGTERM.
func TestStopEndsTheProgramsWholeGroup(t *testing.T) {
	for name, body := range map[string]string{
		"exits on SIGTERM": "sleep 60 &\necho $!\nwait",
		"ignores SIGTERM":  "trap '' TERM\nsleep 60 &\necho $!\nwait",
	} {
		t.Run(name, func(t *testing.T) {
			p, first, end := started(t, body)
			child := pid(t, receive(t, first, "pid"))
			p.Stop()
			e := receive(t, end, "end after Stop")
			if !gone(child) {
				syscall.Kill(child, syscall.SIGKILL)
				t.Fatalf("child %d still running after Stop and Wait", child)
			}
			if e.code != -1 {
				t.Fatalf("code %d, want -1 for a program a signal ended", e.code)
			}
		})
	}
}

// TestWaitKillsWhatANormalExitLeftInItsGroup guards against a program
// that answered and exited leaving a child of its group running.
func TestWaitKillsWhatANormalExitLeftInItsGroup(t *testing.T) {
	_, first, end := started(t, "sleep 60 >/dev/null 2>&1 &\necho $!")
	child := pid(t, receive(t, first, "pid"))
	if e := receive(t, end, "end"); e.code != 0 {
		t.Fatalf("code %d, want 0", e.code)
	}
	if !gone(child) {
		syscall.Kill(child, syscall.SIGKILL)
		t.Fatalf("child %d still running after Wait", child)
	}
}

// TestWaitDoesNotWaitForAChildHoldingStderr guards against a run that
// answered and exited being held until a child it left, which kept its
// standard error, exits on its own: then Wait never gets to kill the
// group, and the gateway reports a finished run Stalled.
func TestWaitDoesNotWaitForAChildHoldingStderr(t *testing.T) {
	_, first, end := started(t, "sleep 60 >/dev/null &\necho $!")
	child := pid(t, receive(t, first, "pid"))
	select {
	case e := <-end:
		if e.code != 0 {
			t.Fatalf("code %d, want 0", e.code)
		}
	case <-time.After(3 * time.Second):
		syscall.Kill(child, syscall.SIGKILL)
		<-end
		t.Fatal("Wait still waiting 3 s after the program exited, for a child holding its stderr")
	}
	if !gone(child) {
		syscall.Kill(child, syscall.SIGKILL)
		t.Fatalf("child %d still running after Wait", child)
	}
}

// TestStartRunsInDirWithoutStdinAndWithEnvsPath guards against a run in
// Capy's own folder (a repository's CLAUDE.md would load), one reading
// Capy's stdin, and one without the login shell's PATH, where an npm
// install finds no node.
func TestStartRunsInDirWithoutStdinAndWithEnvsPath(t *testing.T) {
	dir := t.TempDir()
	const path = "/capy/test/bin:/usr/bin:/bin"
	e := Env{Path: func(context.Context) string { return path }}
	p, err := Start(e, []string{script(t, `pwd; cat; echo "$PATH"`)}, dir)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	p.Lines(func(line []byte) { lines = append(lines, string(line)) })
	if code, _ := p.Wait(); code != 0 || len(lines) != 2 {
		t.Fatalf("exit %d, printed %q; want pwd and PATH, nothing read from stdin", code, lines)
	}
	want, _ := filepath.EvalSymlinks(dir)
	if got, _ := filepath.EvalSymlinks(lines[0]); got != want {
		t.Fatalf("ran in %q, want %q", lines[0], dir)
	}
	if lines[1] != path {
		t.Fatalf("PATH %q, want %q", lines[1], path)
	}

	// Without Env.Path, Capy's own environment.
	p, err = Start(Env{}, []string{script(t, `echo "$PATH"`)}, dir)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	p.Lines(func(line []byte) { got = string(line) })
	p.Wait()
	if got != os.Getenv("PATH") {
		t.Fatalf("PATH %q without Env.Path, want Capy's %q", got, os.Getenv("PATH"))
	}
}

// lines starts body and returns every line it printed.
func lines(t *testing.T, body string) []string {
	t.Helper()
	p, err := Start(Env{}, []string{script(t, body)}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	p.Lines(func(line []byte) { got = append(got, string(line)) })
	p.Wait()
	return got
}

// repeat prints n bytes of c, with no newline.
func repeat(n int, c string) string {
	return "head -c " + strconv.Itoa(n) + " /dev/zero | tr '\\0' " + c
}

// TestLinesSkipsAnOverlongLineOnly guards against a tool result longer
// than the buffer being cut into pieces, an overlong one ending the
// stream or bleeding into the next line, and a last line without a
// newline being dropped.
func TestLinesSkipsAnOverlongLineOnly(t *testing.T) {
	got := lines(t, "echo first\n"+repeat(200<<10, "a")+"; echo\n"+repeat(maxLine+1, "b")+"; echo\necho after\nprintf last")
	if len(got) != 4 {
		t.Fatalf("%d lines, want 4: first, the long one, after, last", len(got))
	}
	if got[0] != "first" || got[2] != "after" || got[3] != "last" {
		t.Fatalf("lines %q, %q, %q; want first, after, last", got[0], got[2], got[3])
	}
	if got[1] != strings.Repeat("a", 200<<10) {
		t.Fatalf("a line of 200 KiB came as %d bytes, want it whole", len(got[1]))
	}
}

// TestLinesDeliversALineOfMaxLine guards the bound itself: Lines skips a
// line longer than maxLine, so one of exactly maxLine bytes is read.
func TestLinesDeliversALineOfMaxLine(t *testing.T) {
	got := lines(t, repeat(maxLine, "c")+"; echo\necho after")
	if len(got) != 2 || len(got[0]) != maxLine || got[1] != "after" {
		t.Fatalf("got %d lines, want the %d-byte line, then after", len(got), maxLine)
	}
}

// TestWaitGivesTheExitStatusAndTheEndOfStderr guards against a lost exit
// code, which the classifier reports, and an unbounded stderr.
func TestWaitGivesTheExitStatusAndTheEndOfStderr(t *testing.T) {
	_, _, end := started(t, "printf BEGIN >&2\n"+repeat(maxStderr+1000, "x")+" >&2\nprintf END >&2\nexit 3")
	e := receive(t, end, "end")
	if e.code != 3 {
		t.Fatalf("code %d, want 3", e.code)
	}
	if len(e.stderr) != maxStderr || !bytes.HasSuffix(e.stderr, []byte("xEND")) || bytes.Contains(e.stderr, []byte("BEGIN")) {
		t.Fatalf("stderr of %d bytes ending %q; want its last %d bytes", len(e.stderr), e.stderr[max(0, len(e.stderr)-8):], maxStderr)
	}
}

// echoStream makes a TextDelta and a Message of each line, and ends as
// Exited with the exit code, keeping the stderr it was given.
type echoStream struct{ stderr []byte }

func (s *echoStream) Line(line []byte) []gateway.Event {
	return []gateway.Event{gateway.TextDelta{Text: string(line)}, gateway.Message{Text: string(line)}}
}

func (s *echoStream) End(code int, stderr []byte) gateway.Event {
	s.stderr = stderr
	return gateway.Failed{Kind: gateway.Exited, Code: code}
}

// events reads r until its channel closes.
func events(t *testing.T, r gateway.Run) []gateway.Event {
	t.Helper()
	var got []gateway.Event
	timeout := time.After(10 * time.Second)
	for {
		select {
		case e, ok := <-r.Events():
			if !ok {
				return got
			}
			got = append(got, e)
		case <-timeout:
			t.Fatalf("events not closed within 10 s, after %v", got)
		}
	}
}

// TestNewRunReportsEveryLineThenEnd guards against events lost or out of
// order, End's event missing or sent twice, and a channel left open.
func TestNewRunReportsEveryLineThenEnd(t *testing.T) {
	p, err := Start(Env{}, []string{script(t, "echo a\necho b\necho oops >&2\nexit 4")}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &echoStream{}
	got := events(t, NewRun(context.Background(), p, s))
	want := []gateway.Event{
		gateway.TextDelta{Text: "a"}, gateway.Message{Text: "a"},
		gateway.TextDelta{Text: "b"}, gateway.Message{Text: "b"},
		gateway.Failed{Kind: gateway.Exited, Code: 4},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	if string(s.stderr) != "oops\n" {
		t.Fatalf("End given stderr %q, want the program's", s.stderr)
	}
}

// TestARunStoppedFailsAsStopped guards against a run the user stopped,
// or whose ctx ended, reporting End's failure (an Exited, shown as the
// agent crashing) or never ending.
func TestARunStoppedFailsAsStopped(t *testing.T) {
	for name, stop := range map[string]func(gateway.Run, context.CancelFunc){
		"Stop":       func(r gateway.Run, _ context.CancelFunc) { r.Stop() },
		"ctx cancel": func(_ gateway.Run, cancel context.CancelFunc) { cancel() },
	} {
		t.Run(name, func(t *testing.T) {
			p, err := Start(Env{}, []string{script(t, "echo ready\nexec sleep 60")}, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := NewRun(ctx, p, &echoStream{})
			if e := receive(t, r.Events(), "first event"); e != (gateway.TextDelta{Text: "ready"}) {
				t.Fatalf("first event %v, want ready", e)
			}
			stop(r, cancel)
			got := events(t, r)
			if len(got) != 2 || got[0] != (gateway.Message{Text: "ready"}) {
				t.Fatalf("events after stopping %v, want the rest of ready, then Failed", got)
			}
			if f, ok := got[1].(gateway.Failed); !ok || f.Kind != gateway.Stopped {
				t.Fatalf("last event %v, want Failed Stopped", got[1])
			}
			r.Stop() // after the end: nothing to stop
		})
	}
}

// TestProgramStartsWhatItFound guards against a run of a program never
// probed, or too old, and one of another path or without its arguments.
func TestProgramStartsWhatItFound(t *testing.T) {
	tool := faketool.Tool(t, filepath.Join(t.TempDir(), "claude"), "2.1.296 (Claude Code)")
	p := &Program{Env: env(t), Spec: spec(tool)}
	if _, ok := p.Found(); ok {
		t.Fatal("found before a probe")
	}
	if _, err := p.Start(t.TempDir(), "-p"); !errors.Is(err, gateway.ErrUnavailable) {
		t.Fatalf("start before a probe: %v, want ErrUnavailable", err)
	}
	p.Probe(context.Background())
	if path, ok := p.Found(); !ok || path != tool {
		t.Fatalf("found %q, %v; want %q", path, ok, tool)
	}
	proc, err := p.Start(t.TempDir(), "-p", "--", "-a prompt")
	if err != nil {
		t.Fatal(err)
	}
	proc.Lines(func([]byte) {})
	proc.Wait()
	calls := faketool.Calls(t, tool)
	if last := calls[len(calls)-1]; !slices.Equal(last, []string{"-p", "--", "-a prompt"}) {
		t.Fatalf("ran with %q, want the arguments as given", last)
	}

	faketool.Set(t, tool, "version", "2.0.4 (Claude Code)\n")
	p.Probe(context.Background())
	if _, ok := p.Found(); ok {
		t.Fatal("a program too old is found")
	}
	if _, err := p.Start(t.TempDir()); !errors.Is(err, gateway.ErrUnavailable) {
		t.Fatalf("start of a program too old: %v, want ErrUnavailable", err)
	}
}
