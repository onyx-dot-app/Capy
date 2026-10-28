package views

import (
	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/views/kit"
)

// segment is one choice of a segmented control: a label, after an icon;
// a disabled one cannot be chosen yet.
type segment struct {
	icon     *ui.SVG
	label    string
	disabled bool
}

// segmented is a segmented control in the style of AppKit's: a quiet
// track in which the chosen segment is raised. small is the size of
// settings rows; otherwise it switches the views of a window.
func segmented(c *ui.Context, chosen *int, label string, small bool, items ...segment) ui.Element {
	p := kit.Colors(c)
	height, font, outer, inner, minWidth := float32(22), float32(13), float32(7), float32(5), float32(110)
	if small {
		height, font, outer, inner, minWidth = 20, 12, 6, 4, 0
	}
	seg := ui.SegmentedBase(c, chosen, len(items))
	seg.Track.Label(label).Padding(2).Radius(outer).Background(p.Fill2).Children(func() {
		for i, it := range items {
			s := seg.Segment(i).Height(height).MinWidth(minWidth).Padding(0, 10).Gap(5).Radius(inner).
				Row().AlignItems(ui.Center).Justify(ui.Center).FontSize(font).FontWeight(500).Label(it.label)
			if i == *chosen {
				s.Background(p.Control).Shadow(0, 1, 2, 0, ui.RGBA(0, 0, 0, 0.14))
			}
			if it.disabled {
				s.Disabled(true).TextColor(p.Tertiary)
			}
			s.Children(func() {
				if it.icon != nil {
					ui.Icon(c, it.icon).Size(14, 14).TextColor(p.Secondary)
				}
				ui.Text(c, it.label).SingleLine()
			})
		}
	})
	return seg.Track
}

// popUp is a pop-up button as System Settings shows them: the value and
// up-down arrows, with no face until hovered, opening a menu of the
// system's. It returns the option chosen, if one was.
func popUp(c *ui.Context, value string, options []string, label string) (chosen string, ok bool) {
	p := kit.Colors(c)
	b := ui.ButtonBase(c).Label(label).Row().Gap(4).AlignItems(ui.Center).Padding(2, 4, 2, 8).Radius(5)
	if b.Hovered() {
		b.Background(p.Fill2)
	}
	b.Menu(func(m *ui.Menu) {
		for _, o := range options {
			if m.Item(o).Checked(o == value).Chosen() {
				chosen, ok = o, true
			}
		}
	})
	b.Children(func() {
		ui.Text(c, value).SingleLine()
		ui.Icon(c, kit.UpDownIcon).Size(12, 12).TextColor(p.Secondary)
	})
	return chosen, ok
}
