package views

import (
	"context"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/views/addon"
)

// host is the app as the add-on sees it.
type host struct{ app *App }

var _ addon.Host = host{}

func (h host) ShowSession(id string) { h.app.OpenWindow(id) }

func (h host) ShowTranscript(id string, atMs int64) {
	h.app.OpenWindow(id)
	h.app.window.showTranscript(id, atMs)
	h.app.windowChanged()
}

func (h host) LibraryChanged() {
	if h.app.window != nil {
		h.app.window.reload()
	}
	h.app.refreshMenu()
}

func (h host) LiveSession() *addon.Session {
	if h.app.captions == nil {
		return nil
	}
	return h.app.captions.addonSession()
}

func (h host) ExpandCaptionBar() bool {
	b := h.app.captions
	if b == nil {
		return false
	}
	capsule := b.minimized
	if capsule {
		b.expand()
	}
	b.win.Focus()
	return capsule
}

func (h host) RegisterShortcut(accelerator string, fn func()) error {
	return mygo.GlobalShortcut.Register(accelerator, fn)
}

func (h host) Toast(t ui.Toast) {
	if h.app.window == nil {
		return
	}
	h.app.window.toasts = append(h.app.window.toasts, t)
	h.app.windowChanged()
}

func (h host) Update(fn func()) {
	mygo.RunOnMain(func() {
		fn()
		h.app.redraw()
	})
}

// Polished merges the polish into the recording, when it is of session
// id, and reloads Capy's window on it.
func (h host) Polished(id string) {
	mygo.RunOnMain(func() {
		if b := h.app.captions; b != nil && b.run != nil && b.run.Snapshot().SessionID == id {
			run := b.run
			go func() { // reads the store: off the main thread
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := run.MergePolish(ctx); err != nil {
					h.app.log.Warn("merge polish", "session", id, "err", err)
				}
			}()
		}
		if w := h.app.window; w != nil && w.selected == id {
			w.load(id)
		}
	})
}

func (h host) Window() *mygo.Window {
	if h.app.window == nil {
		return nil
	}
	return h.app.window.win
}
