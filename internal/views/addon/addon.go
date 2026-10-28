// Package addon is the contract between Capy's views and the one add-on
// they may host: the slots the views offer it, called as they are built,
// and the Host the add-on may call. The views know the slots, never what
// fills them; an add-on builds its UI with the views' look from
// internal/views/kit.
//
// Every method of both runs on the main thread, but AddOn.Highlighted
// and Host.Polished, which say where they run.
package addon

import (
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// Session is the session a slot is shown for: the one in view, or the
// one being recorded.
type Session struct {
	ID    string
	Title string // "" until the recap or a rename names it
	Live  bool   // being recorded
}

// Batch is a batch of imported files: its saved files' sessions, in
// order.
type Batch struct {
	ID, Name   string
	SessionIDs []string
}

// Pane is a pane an add-on docks beside a view, Width wide. The user
// resizes it between MinWidth and MaxWidth, where the view allows it,
// with the divider before it; MinWidth ≤ MaxWidth.
type Pane struct {
	Width, MinWidth, MaxWidth float32
	// MinHeight is the least height the caption bar grows to while it
	// shows the pane.
	MinHeight float32
	// View builds the pane's content.
	View func()
	// Resized reports the width the user dragged the divider to, within
	// MinWidth and MaxWidth; nil leaves the pane at Width.
	Resized func(width float32)
}

// AddOn fills the slots the views offer. A slot that has nothing to show
// builds nothing, or returns nil or no sections. Slots run on every frame
// of their view: they must not block.
type AddOn interface {
	// Start hands the add-on the Host, once, as the app starts, before
	// any slot runs: it may register its global shortcuts then.
	Start(h Host)
	// Toolbar builds in the toolbar of Capy's window, left of Record. s
	// is the session in view, nil when there is none.
	Toolbar(c *ui.Context, s *Session)
	// Shortcuts takes the keys of Capy's window it handles, with
	// c.Shortcut, before the window's own.
	Shortcuts(c *ui.Context, s *Session)
	// Inspector is the pane on the right of Capy's window.
	Inspector(c *ui.Context, s *Session) *Pane
	// CaptionHeader builds in the caption bar's header, left of Stop. s
	// is the session being recorded, nil before it starts.
	CaptionHeader(c *ui.Context, s *Session)
	// CaptionPanel is the pane docked right of the caption bar.
	CaptionPanel(c *ui.Context, s *Session) *Pane
	// Settings builds a section of Settings, after Captions and
	// Highlights.
	Settings(c *ui.Context)
	// HighlightsSettings builds the add-on's rows last in Settings ›
	// Highlights.
	HighlightsSettings(c *ui.Context)
	// Highlighted hears that Highlights saved highlights of a session:
	// final after its last check, or after Find Highlights. It is called
	// on the checks' goroutine, not the main thread, and must not block.
	Highlighted(sessionID string, final bool)
	// Export is the Markdown sections that Copy as Markdown and Export…
	// append to the session's document, each starting with its heading.
	Export(s Session) []string
	// BatchMenu adds items to a batch's context menu, after Rename.
	BatchMenu(m *ui.Menu, b Batch)
	// CanAsk reports whether the add-on can take a question about a
	// session now, as a highlight's Ask about this offers it.
	CanAsk() bool
	// Ask asks question about session s, in a thread of its own: in the
	// pane docked to the caption bar when bar, else in the inspector,
	// opened.
	Ask(s Session, question string, bar bool)
}

// Host is what the add-on may ask of the app.
type Host interface {
	// ShowSession opens Capy's window on session id, at its Summary.
	ShowSession(id string)
	// ShowTranscript opens Capy's window on session id's Transcript,
	// scrolled to the turn said at atMs into it, highlighted for a moment.
	ShowTranscript(id string, atMs int64)
	// LibraryChanged refreshes Capy's session views and recent-session menu
	// after the add-on changes the library in this process.
	LibraryChanged()
	// LiveSession is the session being recorded, or nil.
	LiveSession() *Session
	// ExpandCaptionBar shows the caption bar in place of its capsule and
	// brings it forward, to take what is typed, and reports whether it was
	// the capsule.
	ExpandCaptionBar() bool
	// RegisterShortcut registers a global shortcut, such as "Alt+Cmd+K";
	// it fails when another app holds it.
	RegisterShortcut(accelerator string, fn func()) error
	// Toast shows t in Capy's window, if it is open.
	Toast(t ui.Toast)
	// Window is Capy's window, to parent a dialog to, or nil while it is
	// closed.
	Window() *mygo.Window
	// Update runs fn on the main thread, from any goroutine, then builds
	// Capy's windows anew: how the add-on shows what it did off the main
	// thread.
	Update(fn func())
	// Polished shows the polish the add-on saved on session id's
	// highlights, in the recording and in Capy's window. It may be called
	// from any goroutine.
	Polished(sessionID string)
}

// None is the add-on that fills no slot.
var None AddOn = none{}

type none struct{}

func (none) Start(Host)                               {}
func (none) Toolbar(*ui.Context, *Session)            {}
func (none) Shortcuts(*ui.Context, *Session)          {}
func (none) Inspector(*ui.Context, *Session) *Pane    { return nil }
func (none) CaptionHeader(*ui.Context, *Session)      {}
func (none) CaptionPanel(*ui.Context, *Session) *Pane { return nil }
func (none) Settings(*ui.Context)                     {}
func (none) HighlightsSettings(*ui.Context)           {}
func (none) Highlighted(string, bool)                 {}
func (none) Export(Session) []string                  { return nil }
func (none) BatchMenu(*ui.Menu, Batch)                {}
func (none) CanAsk() bool                             { return false }
func (none) Ask(Session, string, bool)                {}
