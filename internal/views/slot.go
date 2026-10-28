package views

import (
	"github.com/egoist/mygo"

	"github.com/onyx-dot-app/Capy/internal/session"
)

// sessionSlot is a window's session from the moment it starts connecting,
// so that closing or stopping the window while it connects cannot leave a
// session capturing with nothing to stop it. Main thread only.
type sessionSlot struct {
	run      liveSession
	err      error
	starting bool
	closed   bool
	cancel   func()
}

// start connects a session for win. onChange runs, from any goroutine,
// when the session changes; done once the start completes.
func (s *sessionSlot) start(a *App, win *mygo.Window, cfg session.Config, onChange, done func()) {
	s.run, s.err, s.starting = nil, nil, true
	s.cancel = a.startSession(win, cfg, onChange, func(run *session.Run, err error) {
		if err != nil {
			s.attach(nil, err)
		} else {
			s.attach(run, nil)
		}
		done()
	})
}

// attach takes the outcome of start: the run, or why there is none.
func (s *sessionSlot) attach(run liveSession, err error) {
	s.starting = false
	switch {
	case err != nil:
		s.err = err
	case s.closed:
		run.Stop() // the window closed while it connected
	default:
		s.run = run
	}
}

// capturing reports whether the session is connecting or captures audio.
func (s *sessionSlot) capturing() bool {
	if s.starting {
		return true
	}
	if s.run == nil {
		return false
	}
	st := s.run.Snapshot().Status
	return st == session.StatusConnecting || st == session.StatusLive || st == session.StatusReconnecting
}

// stop ends capture, abandoning a start in progress.
func (s *sessionSlot) stop() {
	switch {
	case s.starting:
		s.cancel()
	case s.run != nil:
		s.run.Stop()
	}
}

// windowClosed stops whatever the slot holds; a start still in progress
// stops its run when it completes.
func (s *sessionSlot) windowClosed() {
	s.closed = true
	s.stop()
}
