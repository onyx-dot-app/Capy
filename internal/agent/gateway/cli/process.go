package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
)

// Bounds of a started program.
const (
	// maxLine is the longest line of output read: a tool's result comes
	// back in one, a page of turns JSON-escaped. A longer one is skipped.
	maxLine = 16 << 20
	// maxStderr is how much of the end of standard error is kept, for the
	// classifier.
	maxStderr = 64 << 10
	// stopGrace is how long a program has to exit on SIGTERM before its
	// group is killed.
	stopGrace = 2 * time.Second
)

// Process is an agent's program started for one run, in a process group
// of its own: it and every process it starts, such as Capy mcp, end
// together.
type Process struct {
	cmd    *exec.Cmd
	stdout *os.File
	stderr *tail
	// exited is closed once the program is reaped and its group killed;
	// code is then its exit status, and drained closed once stderr is read.
	exited  chan struct{}
	drained chan struct{}
	code    int
	once    sync.Once
}

// drainWait bounds the reading of a pipe after the program and its group
// ended, what is left in it included: a process that left the group may
// still hold it.
const drainWait = 5 * time.Second

// Start runs argv in dir: never through a shell, with stdin from
// /dev/null, the PATH env gives, in its own process group. Its output
// goes to pipes of Capy's own, so that a child holding one cannot keep
// the program from being reaped. Its standard error is kept only for
// Wait. An error names the program, never its folder.
func Start(env Env, argv []string, dir string) (*Process, error) {
	if len(argv) == 0 {
		return nil, errors.New("cli: no command")
	}
	path := ""
	if env.Path != nil {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		path = env.Path(ctx)
		cancel()
	}
	fail := func(err error) error { return fmt.Errorf("cli: start %s: %w", filepath.Base(argv[0]), cause(err)) }
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, fail(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, fail(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = environ(path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = outW, errW
	err = cmd.Start()
	// The program has its own copies; with Capy's closed, the pipes end
	// when it and its group do.
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		return nil, fail(err)
	}
	p := &Process{cmd: cmd, stdout: outR, stderr: &tail{max: maxStderr}, exited: make(chan struct{}), drained: make(chan struct{})}
	go func() {
		defer close(p.drained)
		defer errR.Close()
		_, _ = io.Copy(p.stderr, errR)
	}()
	go p.reap(errR)
	return p, nil
}

// reap waits for the program to exit, then kills what it left in its
// group, and bounds the reading of its pipes.
func (p *Process) reap(errR *os.File) {
	err := p.cmd.Wait()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		p.code = ee.ExitCode()
	} else if err != nil {
		p.code = -1
	}
	// The leader is reaped, but its group may live on; its ID cannot be
	// another's while it does.
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	deadline := time.Now().Add(drainWait)
	_ = p.stdout.SetReadDeadline(deadline)
	_ = errR.SetReadDeadline(deadline)
	close(p.exited)
}

// Lines calls fn with each line the program prints, without its newline,
// until its output ends. A line longer than maxLine is skipped. fn must
// not keep the line. Call it once.
func (p *Process) Lines(fn func(line []byte)) {
	defer p.stdout.Close()
	r := bufio.NewReaderSize(afterExit{p}, 64<<10)
	var line []byte
	skipping := false
	for {
		chunk, err := r.ReadSlice('\n')
		if !skipping {
			line = append(line, chunk...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(line) > maxLine { // with no newline yet
				skipping, line = true, line[:0]
			}
			continue
		}
		if text := bytes.TrimRight(line, "\r\n"); !skipping && len(text) > 0 && len(text) <= maxLine {
			fn(text)
		}
		line, skipping = line[:0], false
		if err != nil {
			return
		}
	}
}

// afterExit reads the program's output, and once it exited, gives each
// read drainWait: what is left is read however slowly it is taken, and a
// process that left the group cannot hold the pipe open for good.
type afterExit struct{ p *Process }

func (r afterExit) Read(b []byte) (int, error) {
	select {
	case <-r.p.exited:
		_ = r.p.stdout.SetReadDeadline(time.Now().Add(drainWait))
	default:
	}
	return r.p.stdout.Read(b)
}

// Wait waits for the program to exit and returns its exit status, -1 when
// a signal ended it, and the end of its standard error: for the
// classifier only, never a log. Any process the program left in its
// group is killed.
func (p *Process) Wait() (code int, stderr []byte) {
	<-p.exited
	<-p.drained
	return p.code, p.stderr.Bytes()
}

// Stop ends the program and its group: SIGTERM, then SIGKILL if it has
// not exited within stopGrace. Wait still reports how it ended.
func (p *Process) Stop() {
	p.once.Do(func() {
		select {
		case <-p.exited:
			return
		default:
		}
		pgid := p.cmd.Process.Pid
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		go func() {
			select {
			case <-p.exited:
			case <-time.After(stopGrace):
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}()
	})
}

// tail keeps the last max bytes written to it.
type tail struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tail) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return bytes.Clone(t.buf)
}

// Start starts the program found with args in dir. A program not found,
// or too old, is gateway.ErrUnavailable.
func (p *Program) Start(dir string, args ...string) (*Process, error) {
	p.mu.Lock()
	last := p.last
	p.mu.Unlock()
	if !last.Usable() {
		return nil, gateway.ErrUnavailable
	}
	return Start(p.Env, append([]string{last.Path}, args...), dir)
}

// Stream is how an agent's program reports a run, line by line.
type Stream interface {
	// Line turns a line of output into events: neither Done nor Failed,
	// which only End gives.
	Line(line []byte) []gateway.Event
	// End is the run's last event, once the program exited with code and
	// stderr: the Done or Failed its output ended with, else what stderr
	// tells.
	End(code int, stderr []byte) gateway.Event
}

// NewRun is the run of a started program, reporting what s reads of its
// output. Stop, or ctx ending, stops the program and its group, and the
// run fails as Stopped.
func NewRun(ctx context.Context, p *Process, s Stream) gateway.Run {
	r := &programRun{events: make(chan gateway.Event, 64), p: p}
	go func() {
		defer close(r.events)
		defer context.AfterFunc(ctx, r.Stop)()
		p.Lines(func(line []byte) {
			for _, e := range s.Line(line) {
				r.events <- e
			}
		})
		code, stderr := p.Wait()
		if r.stopped.Load() {
			r.events <- gateway.Failed{Kind: gateway.Stopped, Code: code}
			return
		}
		r.events <- s.End(code, stderr)
	}()
	return r
}

type programRun struct {
	events  chan gateway.Event
	p       *Process
	stopped atomic.Bool
}

func (r *programRun) Events() <-chan gateway.Event { return r.events }

func (r *programRun) Stop() {
	r.stopped.Store(true)
	r.p.Stop()
}

// Found is where the last probe found the program, if it is usable.
func (p *Program) Found() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last.Path, p.last.Usable()
}
