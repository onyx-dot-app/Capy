package ui

import (
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/views/addon"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
)

// livePaneHeight is the least height the caption bar grows to while its
// panel shows, Q1.9.
const livePaneHeight = 340

// liveShortcut opens Ask while recording, from any app.
const liveShortcut = "Alt+Cmd+K"

// registerLive registers ⌥⌘K; another app may hold it, which is logged,
// and the pill's tooltip then names no shortcut.
func (a *AddOn) registerLive() {
	// Through Update, so that the bar draws the panel at once.
	press := func() { a.host.Update(a.askLive) }
	if err := a.host.RegisterShortcut(liveShortcut, press); err != nil {
		a.o.Log.Warn("global shortcut unavailable", "shortcut", liveShortcut, "err", err)
		return
	}
	a.liveKey = true
}

// askLive is ⌥⌘K: with a session recording, it opens the bar's panel, the
// bar expanded from its capsule, or closes it.
func (a *AddOn) askLive() {
	s := a.host.LiveSession()
	if s == nil || !a.askable() {
		return
	}
	if a.host.ExpandCaptionBar() || !a.liveOpen {
		a.openLive()
		return
	}
	a.closeLive()
}

func (a *AddOn) openLive() {
	a.liveOpen, a.live.focus = true, true
}

// closeLive closes the bar's panel: Catch me up covers what is said from
// now on.
func (a *AddOn) closeLive() {
	a.liveOpen = false
	if s := a.host.LiveSession(); s != nil {
		a.o.Ask.Seen(s.ID, time.Now())
	}
}

// CaptionHeader implements addon.AddOn: the Ask pill, pressed while the
// panel is open.
func (a *AddOn) CaptionHeader(c *ui.Context, s *addon.Session) {
	if !a.askable() || s == nil {
		return
	}
	tip := "Ask"
	if a.liveKey {
		tip = "Ask (⌥⌘K)"
	}
	pill := ui.ButtonBase(c).Label("Ask").Tooltip(tip).Height(24).Padding(0, 9).Radius(6).Gap(5).AlignItems(ui.Center)
	if a.liveOpen {
		pill.Background(ui.RGBA(255, 255, 255, 0.2)).TextColor(kit.BarText)
	} else {
		pill.Border(1, ui.RGBA(255, 255, 255, 0.22)).TextColor(ui.RGBA(255, 255, 255, 0.8))
	}
	pill.Children(func() {
		ui.Icon(c, askIcon).Size(13, 13)
		ui.Text(c, "Ask").FontSize(12).FontWeight(600).SingleLine()
	})
	if pill.Clicked() {
		if a.liveOpen {
			a.closeLive()
		} else {
			a.openLive()
		}
	}
}

// CaptionPanel implements addon.AddOn: Ask about the session being
// recorded, docked right of the bar, while it is open. It closes as the
// recording stops: the thread is the session's, in the inspector.
func (a *AddOn) CaptionPanel(c *ui.Context, s *addon.Session) *addon.Pane {
	if s == nil {
		a.liveOpen = false
		return nil
	}
	if !a.askable() || !a.liveOpen {
		return nil
	}
	if s.ID != a.live.session.ID { // a new recording: nothing of the last one's field
		a.live = newPanel(a, a.data, true)
		a.live.focus = true
	}
	pane := a.pane(&a.liveSize, func() { a.live.view(c, s) })
	pane.MinHeight = livePaneHeight
	return pane
}
