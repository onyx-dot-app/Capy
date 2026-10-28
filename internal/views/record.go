package views

import (
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/lang"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
)

// recordState is what the Record button shows of captions.
type recordState int

const (
	recordIdle      recordState = iota // Record starts captions
	recordStarting                     // connecting, permission prompts included
	recordLive                         // Stop ends them
	recordFinishing                    // stopped, saving the transcript
)

// recordState is what Record does now, and since when the session has
// listened while it is live.
func (a *App) recordState() (recordState, time.Time) {
	b := a.captions
	switch {
	case b == nil:
		return recordIdle, time.Time{}
	case b.stopping:
		return recordFinishing, time.Time{}
	case !b.running():
		return recordIdle, time.Time{} // it failed or ended: Record starts anew
	}
	snap := b.snapshot()
	if b.starting || snap.StartedAt.IsZero() {
		return recordStarting, time.Time{}
	}
	return recordLive, snap.StartedAt
}

const (
	// recordWidth is the split button's, which Stop · 00:00 and Starting…
	// keep, so that nothing shifts as Record changes.
	recordWidth  = 156
	chevronWidth = 26 // with the divider before it
)

// recordSize is where a Record button sits.
type recordSize int

const (
	recordToolbar recordSize = iota // the toolbar's, in place of the split button
	recordJoined                    // the left part of the split button
	recordLarge                     // the empty window's
)

// recordButton is the main action, in the color that only it has: Record,
// Starting…, Stop with the time listened, or Finishing…. It reports a
// click.
func recordButton(c *ui.Context, a *App, size recordSize) bool {
	p := kit.Colors(c)
	state, since := a.recordState()
	height, padding, font, dot, gap := float32(28), float32(14), float32(13), float32(9), float32(7)
	if size == recordLarge {
		height, padding, font, dot, gap = 44, 26, 17, 12, 10
	}
	b := ui.ButtonBase(c).Height(height).Padding(0, padding).Gap(gap).
		Row().AlignItems(ui.Center).Justify(ui.Center).FontSize(font).FontWeight(600)
	switch size {
	case recordToolbar:
		b.Width(recordWidth).Radius(6)
	case recordJoined:
		b.Grow(1).Radius(6, 0, 0, 6)
	case recordLarge:
		b.Radius(10)
	}
	label, tooltip := "Record", "Record ("+shortcutText+")"
	switch state {
	case recordIdle:
		bg := p.Ink
		if b.Hovered() {
			bg = p.InkHover
		}
		b.Background(bg).TextColor(p.OnInk)
	case recordLive:
		elapsed := c.Now().Sub(since)
		c.After(time.Second - elapsed%time.Second)
		label, tooltip = "Stop · "+clock(elapsed), "Stop Recording ("+shortcutText+")"
		b.Background(kit.LiveRed).TextColor(ui.RGB(255, 255, 255))
	case recordStarting:
		label, tooltip = "Starting…", ""
	case recordFinishing:
		label, tooltip = "Finishing…", ""
	}
	if state == recordStarting || state == recordFinishing {
		b.Disabled(true).Background(p.Fill2).TextColor(p.Secondary)
	} else if size != recordJoined {
		b.Shadow(0, 1, 2, 0, ui.RGBA(0, 0, 0, 0.18))
	}
	name := label
	if state == recordLive {
		name = "Stop Recording" // the time ticks; its name stays
	}
	b.Label(name)
	if tooltip != "" {
		b.Tooltip(tooltip)
	}
	b.Children(func() {
		switch state {
		case recordIdle:
			ui.Box(c).Size(dot, dot).Radius(dot / 2).Background(p.RecordDot)
		case recordLive:
			ui.Box(c).Size(dot, dot).Radius(2).Background(ui.RGB(255, 255, 255))
		default:
			ui.Spinner(c).Size(13, 13)
		}
		ui.Text(c, label).SingleLine().FontFeatures(kit.Tabular)
	})
	return b.Clicked()
}

// recordSplitButton is the toolbar's Record with a chevron beside it whose
// menu sets up, before recording, what Capy listens to and in which
// languages: the same preferences as Settings. Once Record is pressed, it
// is one button as wide, without the chevron. It reports a click on
// Record.
func recordSplitButton(c *ui.Context, a *App) bool {
	if state, _ := a.recordState(); state != recordIdle {
		return recordButton(c, a, recordToolbar)
	}
	p := kit.Colors(c)
	var clicked bool
	ui.Row(c).Width(recordWidth).Height(28).Radius(6).Background(p.Ink).Shadow(0, 1, 2, 0, ui.RGBA(0, 0, 0, 0.18)).Children(func() {
		clicked = recordButton(c, a, recordJoined)
		ui.Box(c).Size(1, 28).Background(p.OnInk.Alpha(0.18))
		chevron := ui.ButtonBase(c).Label("Recording setup").Tooltip("Recording setup").
			Width(chevronWidth-1).Height(28).Radius(0, 6, 6, 0).Center().TextColor(p.OnInk)
		if chevron.Hovered() { // the menu opens under the pointer: it stays lit while open
			chevron.Background(p.OnInk.Alpha(0.12))
		}
		chevron.Menu(func(m *ui.Menu) { a.recordSetup(m) }).Children(func() {
			ui.Icon(c, kit.ChevronDownIcon).Size(12, 12)
		})
	})
	return clicked
}

// recordSetup fills the Record button's menu: the source, the spoken
// language, the translation and whether Highlights marks lines, saved as
// Settings saves them, and Caption Settings… for the rest.
func (a *App) recordSetup(m *ui.Menu) {
	p := &a.prefs
	m.Item("Listen To").Disabled(true)
	for _, src := range []struct {
		source store.Source
		name   string
	}{{store.SourceMic, "Microphone"}, {store.SourceSystem, "This Mac"}} {
		if m.Item(src.name).Checked(p.CaptionsSource == src.source).Chosen() {
			p.CaptionsSource = src.source
			a.savePrefs()
		}
	}
	m.Separator()
	languages := func(label, code string, options []lang.Language, set func(code string)) {
		m.Submenu(label, func(m *ui.Menu) {
			for _, l := range options {
				if m.Item(l.Name).Checked(l.Code == code).Chosen() {
					set(l.Code)
				}
			}
		})
	}
	languages("Spoken Language", p.CaptionsFrom, lang.Spoken(), func(code string) {
		p.CaptionsFrom = code
		a.savePrefs()
	})
	translate := m.Item("Translate").Checked(p.CaptionsTranslate)
	if shownShortcut(translateShortcut) != "" {
		translate.Shortcut(ui.Alt|ui.Cmd, ui.KeyT)
	}
	if translate.Chosen() {
		a.ToggleTranslation()
	}
	languages("Translate To", p.CaptionsTo, lang.Targets(), a.setTarget)
	if a.highlightsOn() && m.Item("Highlights").Checked(p.Highlights).Chosen() {
		p.Highlights = !p.Highlights
		a.savePrefs()
	}
	m.Separator()
	if m.Item("Caption Settings…").Shortcut(ui.Cmd, ui.KeyComma).Chosen() {
		a.OpenSettings()
	}
}

// shortcutText is the captions shortcut as macOS writes it.
const shortcutText = "⌥⌘C"

// translateShortcutText is the translation shortcut as macOS writes it.
const translateShortcutText = "⌥⌘T"

// importButton is Import…, the toolbar's control left of Record: a face
// of its own, quieter than Record's. It reports a click.
func importButton(c *ui.Context) bool {
	p := kit.Colors(c)
	b := ui.ButtonBase(c).Label("Import…").Tooltip("Import Audio Files (⌘O)").Height(28).Padding(0, 11, 0, 9).Radius(6).Gap(6).
		Row().AlignItems(ui.Center).FontWeight(500).Background(p.Control).Shadow(0, 1, 1.5, 0, ui.RGBA(0, 0, 0, 0.1)).
		Border(0.5, ui.RGBA(0, 0, 0, 0.2))
	if b.Hovered() {
		b.Background(p.Control.Mix(ui.RGB(0, 0, 0), 0.03))
	}
	b.Children(func() {
		ui.Icon(c, kit.ImportIcon).Size(15, 15)
		ui.Text(c, "Import…").SingleLine()
	})
	return b.Clicked()
}

// sessionLive reports whether id is the session the caption bar records.
func (a *App) sessionLive(id string) bool {
	if a.captions == nil || id == "" {
		return false
	}
	snap := a.captions.snapshot()
	return snap.SessionID == id && snap.Status != session.StatusSummarizing && snap.Status != session.StatusDone &&
		snap.Status != session.StatusFailed
}
