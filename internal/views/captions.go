package views

import (
	"context"
	"errors"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/views/addon"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

const (
	barWidth      = 760
	barHeight     = 176
	barMin        = 420 // the bar's least width, beside the add-on's pane too
	barMargin     = 48  // above the bottom of the work area
	capsuleWidth  = 216
	capsuleHeight = 40
	capsuleMargin = 16 // left of the right edge of the work area
)

// captionBar is the floating, always-on-top captions window. Minimized, it
// hides behind a capsule that shows it still listens.
type captionBar struct {
	app     *App
	win     *mygo.Window
	capsule *mygo.Window // created when first minimized
	// capsuleWide is the capsule widened for its badge.
	capsuleWide bool
	minimized   bool
	maximized   bool
	stopping    bool // stopped by the user, saving the transcript
	cfg         session.Config
	list        ui.ListState
	panel       docking // the add-on's pane
	att         attention
	card        *mygo.Window // a highlight's card, above the bar
	// tip asks for the user's name, in a first recording with Highlights
	// and no name in Profile; tipName is what is typed.
	tip     bool
	tipName string
	sessionSlot
}

func newCaptionBar(a *App) *captionBar {
	b := &captionBar{app: a, cfg: session.Config{
		Source:     a.prefs.CaptionsSource,
		LangA:      a.prefs.CaptionsFrom,
		LangB:      a.prefs.CaptionsTo,
		Translate:  a.prefs.CaptionsTranslate,
		Highlights: a.highlightConfig(),
	}}
	area := mygo.Screen.PrimaryDisplay().WorkArea
	b.win = mygo.NewWindow(mygo.WindowOptions{
		Title:       "Capy Captions",
		Width:       barWidth,
		Height:      barHeight,
		MinWidth:    barMin,
		MinHeight:   120,
		X:           area.X + (area.Width-barWidth)/2,
		Y:           area.Y + area.Height - barHeight - barMargin,
		StateKey:    "captions",
		Frameless:   true,
		Transparent: true,
		AlwaysOnTop: true,
		Content:     ui.View(b.view),
	})
	b.win.SetVisibleOnAllWorkspaces(true)
	if b.cfg.Highlights.On && a.profile.Name == "" && !a.prefs.HighlightNameAsked {
		b.tip, a.prefs.HighlightNameAsked = true, true // asked once
		a.savePrefs()
	}
	b.panel.frame, b.panel.min, b.panel.post = windowFrame{b.win}, mygo.Rectangle{Width: barMin, Height: 120}, b.win.Update
	b.win.OnClose(func(*mygo.CloseEvent) { b.panel.restore() })
	b.maximized = b.win.IsMaximized()
	b.win.OnMaximize(func() { b.maximized = true; b.win.Invalidate() })
	b.win.OnUnmaximize(func() { b.maximized = false; b.win.Invalidate() })
	b.win.OnClosed(func() {
		if b.capsule != nil {
			b.capsule.Close()
		}
		b.windowClosed()
		if a.captions == b {
			a.captions = nil
			a.captionsChanged()
		}
	})
	b.start(a, b.win, b.cfg, func() { b.win.Update(b.changed) }, func() {
		b.attached()
		a.captionsChanged()
		a.followRecording()
		b.changed()
	})
	return b
}

// running reports whether the session is connecting or captures audio,
// and the user has not stopped it: capture can take seconds to wind down.
func (b *captionBar) running() bool { return b.capturing() && !b.stopping }

// stop ends capture. Once the transcript is saved, the bar closes and
// Capy's window shows the session, where its recap is being written; a session
// that never got under way closes the bar at once.
func (b *captionBar) stop() {
	if b.run == nil {
		b.sessionSlot.stop()
		b.close()
		return
	}
	b.stopping = true
	b.run.Stop()
	b.changed()
}

// finished returns the session to show in Capy's window once the user
// stopped it and its transcript is saved.
func (b *captionBar) finished() (sessionID string, ok bool) {
	if !b.stopping {
		return "", false
	}
	snap := b.snapshot()
	switch snap.Status {
	case session.StatusSummarizing, session.StatusDone, session.StatusFailed:
		return snap.SessionID, true
	}
	return "", false
}

func (b *captionBar) close() { b.win.Close() }

// canSwitch reports whether the spoken language can change: once the
// session listens, until it is stopped.
func (b *captionBar) canSwitch() bool { return b.run != nil && b.running() }

// setSpoken changes the spoken language of the captions being recorded,
// and of the next ones.
func (b *captionBar) setSpoken(code string) {
	if !b.canSwitch() {
		return
	}
	if err := b.run.SetSpoken(code); err != nil {
		b.app.log.Warn("switch spoken language", "err", err)
		return
	}
	b.cfg.LangA = code
	b.app.prefs.CaptionsFrom = code
	b.app.savePrefs()
}

// attached hands the session, once connected, translation turned on or off,
// the target chosen and what Highlights marks, as they are now.
func (b *captionBar) attached() {
	if b.run == nil {
		return
	}
	cfg := b.run.Snapshot().Config
	if cfg.Translate != b.cfg.Translate {
		b.run.SetTranslate(b.cfg.Translate)
	}
	if cfg.LangB != b.cfg.LangB {
		if err := b.run.SetTarget(b.cfg.LangB); err != nil {
			b.app.log.Warn("set translation target", "err", err)
		}
	}
	// Settings and Profile may have changed meanwhile, as the name tip does.
	b.run.SetHighlights(b.app.highlightConfig())
}

// live is s as the session it records now: the saved session learns that
// it translates, and into what, only from the turns it saved.
func (b *captionBar) live(s store.SessionSummary) store.SessionSummary {
	if b.cfg.Translate {
		s.Translated, s.LangB = true, b.cfg.LangB
	}
	return s
}

// setTranslate turns translation on or off for the next turns: at once
// while the session listens, once it attaches while it connects. A
// stopped session keeps what it had.
func (b *captionBar) setTranslate(on bool) {
	if !b.running() {
		return
	}
	b.cfg.Translate = on
	if b.run != nil {
		b.run.SetTranslate(on)
	}
}

// setTarget changes the language the next turns are translated into, as
// setTranslate turns translation on or off.
func (b *captionBar) setTarget(code string) {
	if !b.running() {
		return
	}
	if b.run != nil {
		if err := b.run.SetTarget(code); err != nil {
			b.app.log.Warn("set translation target", "err", err)
			return
		}
	}
	b.cfg.LangB = code
}

// minimize hides the bar behind the capsule.
func (b *captionBar) minimize() {
	if b.capsule == nil {
		area := mygo.Screen.PrimaryDisplay().WorkArea
		b.capsule = mygo.NewWindow(mygo.WindowOptions{
			Title:             "Capy Captions",
			Width:             capsuleWidth,
			Height:            capsuleHeight,
			X:                 area.X + area.Width - capsuleWidth - capsuleMargin,
			Y:                 area.Y + (area.Height-capsuleHeight)/2,
			StateKey:          "captions-capsule",
			Hidden:            true,
			Frameless:         true,
			Transparent:       true,
			AlwaysOnTop:       true,
			DisableResize:     true,
			DisableMaximize:   true,
			DisableFullScreen: true,
			Content:           ui.View(b.capsuleView),
		})
		// The capsule keeps where it was dragged, not an older design's size.
		b.capsule.SetSize(capsuleWidth, capsuleHeight)
		b.capsule.SetVisibleOnAllWorkspaces(true)
	}
	b.minimized = true
	b.capsule.ShowInactive()
	b.win.Hide()
	b.app.windowChanged()
}

// dismiss marks highlight id of the recording not important.
func (b *captionBar) dismiss(id string) {
	run, log := b.run, b.app.log
	if run == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := run.Dismiss(ctx, id); err != nil {
			log.Warn("dismiss highlight", "err", err)
		}
	}()
}

// showLine scrolls the bar's lines to highlight h's, the bar shown.
func (b *captionBar) showLine(h store.Highlight) {
	if b.minimized {
		b.expand()
	}
	if i := lineOf(b.snapshot().Lines, h.Seq); i >= 0 {
		b.list.ScrollTo(i, ui.Center)
	}
	b.att.pill = nil
}

// expand shows the bar again in place of the capsule: what the capsule's
// badge pointed at is seen.
func (b *captionBar) expand() {
	b.minimized, b.att.badge = false, false
	b.capsule.Hide()
	b.win.Show()
	b.app.windowChanged()
}

// changed follows the session on the main thread. Stopped by the user, the
// bar gives way to Capy's window once the transcript is saved. Minimized, the
// capsule shows the status until capture fails or stops on its own, which
// brings the bar back, without taking the focus from the app in use, to
// show why.
func (b *captionBar) changed() {
	if b.closed {
		return
	}
	b.app.holdImports()
	b.app.windowChanged() // it shows the lines too
	if err := b.snapshot().HighlightsErr; err != nil {
		b.app.highlightsDenied = errors.Is(err, zoomai.ErrUnauthorized)
	}
	if id, ok := b.finished(); ok {
		b.app.OpenWindow("")
		b.app.window.showRecording(id)
		b.close()
		return
	}
	if !b.minimized {
		return
	}
	if !b.capsuleWanted() {
		b.minimized = false
		b.capsule.Hide()
		b.win.ShowInactive()
		return
	}
	b.capsule.Invalidate()
}

// capsuleWanted reports whether the session still captures, or saves what
// it captured, without a problem, which the capsule stands for.
func (b *captionBar) capsuleWanted() bool {
	return (b.running() || b.stopping) && b.failure(b.snapshot()) == nil
}

// failure is why the session did not start, or stopped, if it did.
func (b *captionBar) failure(snap session.Snapshot) error {
	if b.err != nil {
		return b.err
	}
	return snap.Err
}

// addonSession is the session being recorded, for the add-on's slots: nil
// before it starts and once it stops.
func (b *captionBar) addonSession() *addon.Session {
	id := b.snapshot().SessionID
	if id == "" || !b.app.sessionLive(id) {
		return nil
	}
	return &addon.Session{ID: id, Live: true}
}

func (b *captionBar) snapshot() session.Snapshot {
	if b.run == nil {
		return session.Snapshot{}
	}
	return b.run.Snapshot()
}

// status is the session's status, with the time listened ticking each
// second while it captures.
func (b *captionBar) status(c *ui.Context, snap session.Snapshot) (status, listened string) {
	status = statusText(snap.Status)
	switch {
	case b.starting:
		status = "Starting…"
	case b.stopping && b.capturing():
		status = statusText(session.StatusFinishing)
	}
	if b.running() && !snap.StartedAt.IsZero() {
		elapsed := c.Now().Sub(snap.StartedAt)
		c.After(time.Second - elapsed%time.Second)
		listened = clock(elapsed)
	}
	return status, listened
}
