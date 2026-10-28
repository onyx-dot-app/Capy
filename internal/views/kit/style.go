// Package kit is Capy's look, shared by the views and an add-on's UI: the
// palette and themes, the icons, and the widgets they have in common.
package kit

import (
	"github.com/egoist/mygo/ui"
)

// Capy's colors: capybara brown on the theme.
var (
	capyBrown = ui.Hex("#9a6b45")
	capyCream = ui.Hex("#f4e9dc")
)

// Palette is Capy's colors in the light or the dark appearance: macOS's
// system colors, warmed toward the capybara brown, as the design's
// foundations give them.
type Palette struct {
	Label, Secondary, Tertiary ui.Color // text
	Separator                  ui.Color
	Window, Sidebar            ui.Color // backgrounds of the main window
	Settings, Group            ui.Color // the Settings page and its groups
	Sheet                      ui.Color // a sheet over the window
	Fill, Fill2                ui.Color // quiet fills: chips, tracks, disabled buttons
	Control                    ui.Color // the face of a chosen segment
	Popover                    ui.Color
	Accent, Link               ui.Color // brown fills; brown text and links
	Ink, InkHover, OnInk       ui.Color // the Record button and its label
	RecordDot                  ui.Color
	Highlight                  ui.Color
	Danger, Success            ui.Color
	Warning                    ui.Color // systemOrange: a problem to look at
}

var (
	lightPalette = Palette{
		Label: ui.RGBA(0, 0, 0, 0.85), Secondary: ui.RGBA(0, 0, 0, 0.5), Tertiary: ui.RGBA(0, 0, 0, 0.26),
		Separator: ui.RGBA(0, 0, 0, 0.1),
		Window:    ui.Hex("#ffffff"), Sidebar: ui.Hex("#edebe9"),
		Settings: ui.Hex("#f3f2f1"), Group: ui.Hex("#ffffff"), Sheet: ui.Hex("#f3f2f1"),
		Fill: ui.RGBA(0, 0, 0, 0.05), Fill2: ui.RGBA(0, 0, 0, 0.075),
		Control: ui.Hex("#ffffff"), Popover: ui.Hex("#f5f3f1"),
		Accent: capyBrown, Link: capyBrown,
		Ink: ui.Hex("#2a2522"), InkHover: ui.Hex("#3a332f"), OnInk: ui.Hex("#ffffff"),
		RecordDot: ui.Hex("#ff4f55"),
		Highlight: capyCream,
		Danger:    ui.Hex("#d93a3f"), Success: ui.Hex("#2fa84f"), Warning: ui.Hex("#c26a00"),
	}
	darkPalette = Palette{
		Label: ui.RGBA(255, 255, 255, 0.88), Secondary: ui.RGBA(255, 255, 255, 0.55), Tertiary: ui.RGBA(255, 255, 255, 0.28),
		Separator: ui.RGBA(255, 255, 255, 0.1),
		Window:    ui.Hex("#1e1d1c"), Sidebar: ui.Hex("#2a2826"),
		Settings: ui.Hex("#1e1d1c"), Group: ui.Hex("#2a2928"), Sheet: ui.Hex("#2e2c2a"),
		Fill: ui.RGBA(255, 255, 255, 0.06), Fill2: ui.RGBA(255, 255, 255, 0.1),
		Control: ui.RGBA(255, 255, 255, 0.2), Popover: ui.Hex("#2e2c2a"),
		Accent: capyBrown, Link: ui.Hex("#c9976c"),
		Ink: ui.Hex("#f2eeea"), InkHover: ui.Hex("#ffffff"), OnInk: ui.Hex("#1d1a18"),
		RecordDot: LiveRed,
		Highlight: ui.RGBA(201, 151, 108, 0.3),
		Danger:    ui.Hex("#ff6b6f"), Success: ui.Hex("#32c25a"), Warning: ui.Hex("#ff9f0a"),
	}
)

// LiveRed is the recording light, and the Stop button while live.
var LiveRed = ui.Hex("#e5484d")

// The caption bar's colors: white text over a warm, near-black tint.
var (
	BarTint  = ui.Hex("#1c1a19")
	BarText  = ui.RGBA(255, 255, 255, 0.96)
	BarMuted = ui.RGBA(255, 255, 255, 0.62)
	BarFaint = ui.RGBA(255, 255, 255, 0.42)
)

// Colors is the palette of the view's appearance, a copy: changing it
// changes no other view.
func Colors(c *ui.Context) Palette {
	if c.Theme().Dark {
		return darkPalette
	}
	return lightPalette
}

// Theme gives a view Capy's palette over the system theme: the brown
// accent and warm system grays.
func Theme(c *ui.Context) {
	t := *c.Theme()
	p := Colors(c)
	t.Accent = capyBrown
	t.AccentHover = capyBrown.Mix(ui.RGB(0, 0, 0), 0.1)
	t.AccentPressed = capyBrown.Mix(ui.RGB(0, 0, 0), 0.2)
	t.AccentText = ui.RGB(255, 255, 255)
	t.Text, t.TextMuted = p.Label, p.Secondary
	t.Danger, t.Success = p.Danger, p.Success
	c.SetTheme(&t)
}

// BarTheme is Capy's theme in the dark, as the caption bar always is:
// widgets such as spinners draw in its text colors, and a divider as the
// edge of a pane docked to the bar, which nothing shows through.
func BarTheme(c *ui.Context) {
	Theme(c)
	t := *c.Theme()
	t.Dark, t.Text, t.TextMuted = true, BarText, BarMuted
	t.Border = BarTint.Mix(ui.RGB(255, 255, 255), 0.14).Alpha(0.9)
	c.SetTheme(&t)
}

// Tabular sets digits at one width, so that a ticking time does not jiggle.
const Tabular = "tnum"
