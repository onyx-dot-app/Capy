package views

import (
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/views/addon"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
)

// detailMin is the narrowest the detail of Capy's window gets to make room
// for an inspector.
const detailMin = 420

// frame is what docking a pane changes of a window.
type frame interface {
	Bounds() mygo.Rectangle
	SetBounds(mygo.Rectangle)
	IsMaximized() bool
	IsFullScreen() bool
	// WorkArea is that of the display the window is on.
	WorkArea() mygo.Rectangle
}

// windowFrame is a window's frame on the screen.
type windowFrame struct{ *mygo.Window }

func (f windowFrame) WorkArea() mygo.Rectangle {
	return mygo.Screen.DisplayMatching(f.Bounds()).WorkArea
}

// growth is what docking a pane changed of a window's frame, to undo as
// the pane closes.
type growth struct{ dx, dy, dw, dh int }

func grown(from, to mygo.Rectangle) growth {
	return growth{to.X - from.X, to.Y - from.Y, to.Width - from.Width, to.Height - from.Height}
}

// undo takes the growth back from r, the frame as it is now: where the
// user dragged the window meanwhile, it stays, and it stays at least min
// wide and tall.
func (g growth) undo(r, min mygo.Rectangle) mygo.Rectangle {
	return mygo.Rectangle{X: r.X - g.dx, Y: r.Y - g.dy,
		Width: max(r.Width-g.dw, min.Width), Height: max(r.Height-g.dh, min.Height)}
}

// docking follows the pane docked to a window: as it opens, the window's
// frame grows to fit it, which closing it, or the window, undoes, so that
// the frame the window keeps for the next launch is its own.
//
// A frame lays out at the size the window had as it began, so the window
// takes its new frame after the frame that opens or closes the pane, which
// shows what the last did: resized within it, the window would show that
// frame stretched to its new size.
type docking struct {
	frame frame          // nil while the window has no frame, as in tests
	min   mygo.Rectangle // the window's minimum Width and Height
	// post runs fn after this frame, then builds the window anew:
	// Window.Update, or the test's queue.
	post   func(fn func())
	open   bool
	grew   growth
	moving bool        // the frame changes after this frame
	shown  *addon.Pane // the pane the last frame showed
}

// follow takes the pane of this frame, nil for none, and returns the pane
// the frame shows. fit is the frame grown for the pane in the work area,
// if it fits.
func (d *docking) follow(pane *addon.Pane, fit func(r, area mygo.Rectangle, p *addon.Pane) (mygo.Rectangle, bool)) *addon.Pane {
	switch {
	case d.moving:
		return d.shown
	case pane != nil && !d.open:
		d.open, d.grew = true, growth{}
		if d.frame == nil {
			break
		}
		from := d.frame.Bounds()
		if to, ok := fit(from, d.frame.WorkArea(), pane); ok {
			d.move(func() {
				d.frame.SetBounds(to)
				d.grew = grown(from, d.frame.Bounds()) // as the system placed it
			})
			return d.shown
		}
	case pane == nil && d.open:
		to, ok := d.undone()
		d.open, d.grew = false, growth{}
		if ok {
			d.move(func() { d.frame.SetBounds(to) })
			return d.shown
		}
	}
	d.shown = pane
	return pane
}

// move changes the window's frame with fn after this frame, unless the
// window closes first.
func (d *docking) move(fn func()) {
	d.moving = true
	d.post(func() {
		if d.moving {
			d.moving = false
			fn()
		}
	})
}

// undone is the window's frame with the growth undone, if it changes: a
// window maximized or full screen meanwhile keeps its frame, which the
// system restores.
func (d *docking) undone() (mygo.Rectangle, bool) {
	if d.frame == nil || d.grew == (growth{}) || d.frame.IsMaximized() || d.frame.IsFullScreen() {
		return mygo.Rectangle{}, false
	}
	return d.grew.undo(d.frame.Bounds(), d.min), true
}

// restore undoes the growth at once, as the window closes.
func (d *docking) restore() {
	to, ok := d.undone()
	d.open, d.grew, d.moving, d.shown = false, growth{}, false, nil
	if ok {
		d.frame.SetBounds(to)
	}
}

// paneWidth is the pane's width within its bounds.
func paneWidth(p *addon.Pane, w float32) float32 {
	return min(max(w, p.MinWidth), p.MaxWidth)
}

// widened is Capy's window r grown by the inspector's width, moved left
// as far as it takes to stay in area; it does not fit a narrower area,
// where the detail narrows instead.
func widened(r, area mygo.Rectangle, p *addon.Pane) (mygo.Rectangle, bool) {
	r.Width += int(paneWidth(p, p.Width))
	if r.Width > area.Width {
		return mygo.Rectangle{}, false
	}
	r.X = min(r.X, area.X+area.Width-r.Width)
	return r, true
}

// docked is the caption bar r with the pane docked on its right, or on its
// left when area has no room on the right, and grown up to the pane's
// MinHeight. A maximized bar keeps its frame.
func docked(maximized bool) func(r, area mygo.Rectangle, p *addon.Pane) (mygo.Rectangle, bool) {
	return func(r, area mygo.Rectangle, p *addon.Pane) (mygo.Rectangle, bool) {
		if maximized {
			return mygo.Rectangle{}, false
		}
		w := int(p.Width)
		width := min(r.Width+w, area.Width)
		if r.X+width > area.X+area.Width {
			r.X = max(min(r.X-w, area.X+area.Width-width), area.X)
		}
		r.Width = width
		if h := int(p.MinHeight); r.Height < h {
			r.Y = max(r.Y+r.Height-h, area.Y) // the bottom edge stays
			r.Height = h
		}
		return r, true
	}
}

// fittedWidth is the width of pane p in room total wide: its own within
// its bounds, narrowed to keep what is left of it keep wide, but not below
// its MinWidth.
func fittedWidth(p *addon.Pane, total, keep float32) float32 {
	return max(min(paneWidth(p, p.Width), total-1-keep), p.MinWidth)
}

// splitPane lays out first with pane p on its right, in room total wide,
// behind a divider that resizes p and keeps first keep wide where it can.
func splitPane(c *ui.Context, p *addon.Pane, total, keep float32, first, second func()) ui.Element {
	size := total - 1 - fittedWidth(p, total, keep)
	split := ui.Split(c, &size, first, second)
	if split.Changed() && p.Resized != nil {
		p.Resized(paneWidth(p, total-1-size))
	}
	return split
}

// withInspector lays out content with the inspector on its right, behind
// a divider that resizes it.
func (w *mainWindow) withInspector(c *ui.Context, pane *addon.Pane, content func()) {
	width, _ := c.Size()
	total := width - w.split - 1 // right of the sidebar and its divider
	splitPane(c, pane, total, detailMin, content, func() {
		ui.Column(c).Fill().AlignItems(ui.Stretch).Background(kit.Colors(c).Window).Children(pane.View)
	}).Fill()
}
