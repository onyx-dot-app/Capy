package kit

import (
	"math"
	"time"

	"github.com/egoist/mygo/ui"
)

// IconButton is a borderless button showing icon over a dark background,
// named label for assistive technology.
func IconButton(c *ui.Context, icon *ui.SVG, label string) ui.Element {
	b := ui.ButtonBase(c).Label(label).Size(24, 24).Radius(6).Tooltip(label)
	if b.Hovered() {
		b.Background(ui.RGBA(255, 255, 255, 0.14))
	}
	b.Children(func() { ui.Icon(c, icon).Size(14, 14) })
	return b
}

// ToolbarButton is a toolbar's icon button, with no face until hovered.
func ToolbarButton(c *ui.Context, icon *ui.SVG, label string) ui.Element {
	p := Colors(c)
	b := ui.ButtonBase(c).Label(label).Tooltip(label).Size(30, 28).Radius(6).Center().TextColor(p.Secondary)
	if b.Hovered() {
		b.Background(p.Fill2).TextColor(p.Label)
	}
	b.Children(func() { ui.Icon(c, icon).Size(16, 16) })
	return b
}

// waveStep is how often the waves below are painted.
const waveStep = 50 * time.Millisecond

// wave goes from 0 to 1 and back once a period, delayed by lag periods.
func wave(now time.Time, period time.Duration, lag float64) float32 {
	t := float64(now.UnixNano()%int64(period))/float64(period) - lag
	return float32(0.5 - 0.5*math.Cos(2*math.Pi*t))
}

// WorkingDots are three dots brightening in turn, for speech being
// recognized or translated. They paint without running the view, at 20
// frames a second: enough for a slow wave, and light on the battery.
func WorkingDots(c *ui.Context, color ui.Color) ui.Element {
	return ui.Box(c).Size(22, 6).Label("Working").Draw(func(p *ui.Painter, r ui.Rect) {
		p.After(waveStep)
		for i := range 3 {
			a := 0.25 + 0.75*wave(p.Now(), 1200*time.Millisecond, float64(i)/6)
			p.Fill(ui.Rect{X: r.X + float32(i)*8, Y: r.Y, W: 6, H: 6}, color.Alpha(a), 3)
		}
	})
}

// LiveDot is the recording light: a dot that breathes while listening.
func LiveDot(c *ui.Context, color ui.Color) ui.Element {
	return BreathingDot(c, color, 1600*time.Millisecond)
}

// BreathingDot is a dot that fades and comes back once a period.
func BreathingDot(c *ui.Context, color ui.Color, period time.Duration) ui.Element {
	return ui.Box(c).Size(8, 8).Draw(func(p *ui.Painter, r ui.Rect) {
		p.After(waveStep)
		p.Fill(r, color.Alpha(0.4+0.6*wave(p.Now(), period, 0)), 4)
	})
}

// SectionHeader titles a group of settings.
func SectionHeader(c *ui.Context, title string) {
	ui.Text(c, title).FontWeight(600).Margin(0, 0, 8, 2)
}

// Group is an inset group of rows, as System Settings shows them, with a
// hairline between each.
func Group(c *ui.Context, rows ...func()) {
	p := Colors(c)
	ui.Column(c).Radius(10).Background(p.Group).Border(1, p.Separator).AlignItems(ui.Stretch).Children(func() {
		for i, row := range rows {
			if i > 0 {
				ui.Box(c).Height(1).Margin(0, 12).Background(p.Separator)
			}
			row()
		}
	})
}

// SettingRow is a row of a group: its label on the left, its control on
// the right.
func SettingRow(c *ui.Context, label string, control func()) {
	ui.Row(c).MinHeight(40).Padding(8, 12).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Text(c, label).Grow(1).SingleLine()
		control()
	})
}

// Footnote explains the group above it.
func Footnote(c *ui.Context, text string) ui.Element {
	return ui.Text(c, text).FontSize(12).TextColor(Colors(c).Secondary).Margin(6, 12, 20)
}

// CheckBadge is a check mark in a filled circle, as SF Symbols'
// checkmark.circle.fill.
func CheckBadge(c *ui.Context, color ui.Color) {
	ui.Box(c).Size(15, 15).Radius(7.5).Background(color).Center().Children(func() {
		ui.Icon(c, CheckIcon).Size(10, 10).TextColor(ui.RGB(255, 255, 255))
	})
}

// CheckCircle is a check mark in a filled circle, 12 wide, in a box of
// 16: a file done.
func CheckCircle(c *ui.Context, color ui.Color) {
	ui.Box(c).Size(16, 16).Center().Children(func() {
		ui.Box(c).Size(12, 12).Radius(6).Background(color).Center().Children(func() {
			ui.Icon(c, CheckIcon).Size(8, 8).TextColor(ui.RGB(255, 255, 255))
		})
	})
}

// DashedCircle is a ring of eight short dashes, 10 wide, in a box of 14:
// a file waiting.
func DashedCircle(c *ui.Context, color ui.Color) {
	ui.Box(c).Size(14, 14).Draw(func(p *ui.Painter, r ui.Rect) {
		cx, cy := r.X+r.W/2, r.Y+r.H/2
		const dashes, radius = 8, 4.7
		for i := range dashes {
			var dash ui.Path
			// Each dash is 42% of its eighth, as a 2.6 dash and 3.6 gap.
			for j := 0; j <= 4; j++ {
				a := 2*math.Pi*(float64(i)+0.42*float64(j)/4)/dashes - math.Pi/2
				x, y := cx+radius*float32(math.Cos(a)), cy+radius*float32(math.Sin(a))
				if j == 0 {
					dash.MoveTo(x, y)
				} else {
					dash.LineTo(x, y)
				}
			}
			p.StrokePath(&dash, 1.1, color)
		}
	})
}

// Ring is a progress ring 12 wide, in a box of 16: a track and, over it,
// the share done, from the top, clockwise.
func Ring(c *ui.Context, color ui.Color, done float32, label string) {
	ui.Box(c).Size(16, 16).Label(label).Draw(func(p *ui.Painter, r ui.Rect) {
		cx, cy := r.X+r.W/2, r.Y+r.H/2
		var track ui.Path
		track.Circle(cx, cy, 6)
		p.StrokePath(&track, 2, color.Alpha(0.25))
		if done <= 0 {
			return
		}
		// A circle from the top: a quarter turn back of where Path.Circle
		// starts, drawn as one arc of many short lines.
		var arc ui.Path
		const steps = 48
		for i := 0; i <= steps; i++ {
			a := -math.Pi/2 + 2*math.Pi*float64(i)/steps
			x, y := cx+6*float32(math.Cos(a)), cy+6*float32(math.Sin(a))
			if i == 0 {
				arc.MoveTo(x, y)
			} else {
				arc.LineTo(x, y)
			}
		}
		p.StrokePath(arc.Trim(0, min(done, 1)), 2, color)
	})
}
