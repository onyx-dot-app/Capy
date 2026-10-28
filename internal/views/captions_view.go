package views

import (
	"slices"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/audio"
	"github.com/onyx-dot-app/Capy/internal/lang"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/views/addon"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
)

// reconnectAmber is the light while the caption bar reconnects.
var reconnectAmber = ui.Hex("#ff9f0a")

// statusLight is the session's light: the recording light while live,
// amber and slower while reconnecting, a spinner while starting or
// finishing, and grey once it stopped.
func statusLight(c *ui.Context, snap session.Snapshot, starting, finishing bool) {
	switch {
	case starting || finishing:
		ui.Spinner(c).Size(13, 13)
	case snap.Status == session.StatusLive:
		kit.LiveDot(c, kit.LiveRed)
	case snap.Status == session.StatusReconnecting:
		kit.BreathingDot(c, reconnectAmber, 2400*time.Millisecond)
	default:
		ui.Box(c).Size(8, 8).Radius(4).Background(ui.RGBA(255, 255, 255, 0.35))
	}
}

// capsuleView is the minimized bar, as Granola's: the recording light,
// the time listened (else the status), a button to show the bar again and,
// while listening, one to stop.
func (b *captionBar) capsuleView(c *ui.Context) {
	kit.BarTheme(c)
	c.Root().Background(ui.Transparent)
	snap := b.snapshot()
	b.att.follow(c.Now(), snap, b.app.prefs.HighlightNudge)
	b.fitCapsule(b.att.badge)
	text, listened := b.status(c, snap)
	if snap.Status == session.StatusLive && listened != "" {
		text = listened
	}
	finishing := b.stopping
	ui.Row(c).Fill().Padding(0, 6, 0, 14).Gap(8).Radius(capsuleHeight/2).AlignItems(ui.Center).
		Background(kit.BarTint.Alpha(0.88)).Border(1, ui.RGBA(255, 255, 255, 0.14)).
		DragWindow().TextColor(kit.BarText).Children(func() {
		statusLight(c, snap, b.starting, finishing)
		ui.Text(c, text).FontSize(13).FontWeight(600).FontFeatures(kit.Tabular).SingleLine()
		ui.Spacer(c)
		if b.att.badge && capsuleBadge(c) {
			if h, ok := highlightOf(snap.Highlights, b.att.badgeID); ok {
				b.showLine(h)
			} else {
				b.expand()
			}
		}
		if barButton(c, kit.ExpandIcon, "Show Caption Bar", 28, true).Clicked() {
			b.expand()
		}
		if b.running() {
			stop := ui.ButtonBase(c).Label("Stop Recording").Tooltip("Stop Recording").Size(28, 28).Radius(14).Center().
				Background(ui.RGBA(255, 255, 255, 0.14))
			if stop.Hovered() {
				stop.Background(ui.RGBA(255, 255, 255, 0.22))
			}
			if stop.Clicked() {
				b.app.ToggleCaptions()
			}
			stop.Children(func() { ui.Box(c).Size(10, 10).Radius(2.5).Background(kit.LiveRed) })
		}
	})
}

// capsuleBadgeWidth is how much wider the capsule is while it shows For
// you.
const capsuleBadgeWidth = 84

// fitCapsule widens the capsule for its badge, or narrows it back, by its
// left edge: it sits against the right of the screen.
func (b *captionBar) fitCapsule(badge bool) {
	want := capsuleWidth
	if badge {
		want += capsuleBadgeWidth
	}
	if b.capsule == nil || b.capsuleWide == badge {
		return
	}
	b.capsuleWide = badge
	win := b.capsule
	win.Update(func() {
		r := win.Bounds()
		win.SetBounds(mygo.Rectangle{X: r.X + r.Width - want, Y: r.Y, Width: want, Height: r.Height})
	})
}

// barButton is a borderless button over the bar's tint, showing icon,
// named label for assistive technology; round makes it a circle.
func barButton(c *ui.Context, icon *ui.SVG, label string, size float32, round bool) ui.Element {
	b := ui.ButtonBase(c).Label(label).Tooltip(label).Size(size, 24).Radius(6).Center().TextColor(kit.BarMuted)
	if round {
		b.Size(size, size).Radius(size / 2)
	}
	if b.Hovered() {
		b.Background(ui.RGBA(255, 255, 255, 0.14)).TextColor(kit.BarText)
	}
	b.Children(func() { ui.Icon(c, icon).Size(13, 13) })
	return b
}

// barScale is the caption bar's type, at its own size or maximized.
type barScale struct {
	header, timer, chip, stamp float32 // sizes
	stampWidth, turnGap        float32
	said, saidLine             float32 // what was said under its translation
	big, bigLine               float32 // the translation, or the captions alone
	side, sideLine             float32 // side by side
	padding                    [4]float32
	fade                       []float32 // opacity of the turns, from the latest
}

var (
	barNormal = barScale{
		header: 40, timer: 13, chip: 12, stamp: 11, stampWidth: 44, turnGap: 12,
		said: 14, saidLine: 19, big: 20, bigLine: 26, side: 16, sideLine: 22,
		padding: [4]float32{0, 24, 16, 16}, fade: []float32{1, 1, 0.7, 0.45, 0.25},
	}
	barMaximized = barScale{
		header: 56, timer: 15, chip: 13, stamp: 13, stampWidth: 72, turnGap: 26,
		said: 22, saidLine: 30, big: 36, bigLine: 46, side: 26, sideLine: 34,
		padding: [4]float32{0, 140, 88, 96}, fade: []float32{1, 1, 0.8, 0.6, 0.4},
	}
)

func (b *captionBar) scale() *barScale {
	if b.maximized {
		return &barMaximized
	}
	return &barNormal
}

func (b *captionBar) view(c *ui.Context) {
	kit.BarTheme(c)
	c.Root().Background(ui.Transparent)
	pane := b.panel.follow(b.app.ext.CaptionPanel(c, b.addonSession()), docked(b.maximized))
	if pane == nil {
		b.bar(c, nil).Fill()
		return
	}
	radius := float32(16)
	if b.maximized {
		radius = 0
	}
	width, _ := c.Size()
	splitPane(c, pane, width, barMin, func() { b.bar(c, pane).Fill() }, func() {
		// A conversation stays legible however clear the bar is.
		ui.Column(c).Fill().Radius(0, radius, radius, 0).AlignItems(ui.Stretch).
			Background(kit.BarTint.Alpha(max(b.opacity(), 0.88))).Border(1, ui.RGBA(255, 255, 255, 0.14)).
			TextColor(kit.BarText).Children(pane.View)
	}).Fill()
}

// opacity is that of the bar's background at the transparency set: 0% is
// solid, 100% shows only the text. A trace remains, as macOS passes clicks
// on fully clear pixels to the window below, and the bar must stay
// draggable.
func (b *captionBar) opacity() float32 {
	return max(1-float32(b.app.prefs.CaptionsTransparency)/100, 0.01)
}

// bar is the caption bar's panel, with the add-on's pane docked on its
// right when there is one.
func (b *captionBar) bar(c *ui.Context, pane *addon.Pane) ui.Element {
	snap := b.snapshot()
	b.att.follow(c.Now(), snap, b.app.prefs.HighlightNudge)
	if !b.minimized {
		b.att.badge = false // seen
	}
	s := b.scale()
	transparency := float32(b.app.prefs.CaptionsTransparency) / 100
	opacity := b.opacity()
	radius := float32(16)
	if b.maximized {
		radius = 0
	}
	panel := ui.Column(c).Radius(radius).AlignItems(ui.Stretch).
		Background(kit.BarTint.Alpha(opacity)).Border(1, ui.RGBA(255, 255, 255, 0.14*opacity)).
		DragWindow().TextColor(kit.BarText)
	if pane != nil {
		panel.Radius(radius, 0, 0, radius)
	}
	// A highlight for the user: one breath of terracotta in the bar's
	// border and shadow, or its border terracotta for a moment.
	switch glow, calm := b.att.glow(c); {
	case calm && glow > 0:
		panel.Border(1.5, glowColor)
	case glow > 0:
		panel.Border(1.5, glowColor.Alpha(0.95*glow)).Shadow(0, 0, 28, 6*glow, glowColor.Alpha(0.35*glow))
	}
	// Text alone hides the controls until the pointer is over the bar.
	hideHeader := transparency >= 1 && !panel.Hovered()
	return panel.Children(func() {
		header := ui.Row(c).Height(s.header).Padding(0, 10, 0, 16).Gap(10).AlignItems(ui.Center)
		if b.maximized {
			header.Padding(0, 24, 0, 32)
		}
		if hideHeader {
			header.Opacity(0)
		}
		header.Children(func() { b.header(c, snap, panel.Hovered()) })
		if err := b.failure(snap); err != nil {
			b.problemView(c, explain(err))
			return
		}
		b.transcriptView(c, snap, transparency)
	})
}

// header is the bar's top row: the light and the time listened (else the
// status), what it listens to, and the controls.
func (b *captionBar) header(c *ui.Context, snap session.Snapshot, hovered bool) {
	s := b.scale()
	failed := b.failure(snap) != nil
	status, listened := b.status(c, snap)
	if snap.Status == session.StatusLive && listened != "" && !b.stopping {
		status = listened
	}
	statusLight(c, snap, b.starting, b.stopping && b.capturing())
	if !failed && status != "" {
		ui.Text(c, status).FontSize(s.timer).FontWeight(600).FontFeatures(kit.Tabular).SingleLine()
	}
	// While it listens, the chip is a menu that changes the spoken language.
	chip := ui.Row(c)
	switchable := b.canSwitch()
	if switchable {
		chip = ui.ButtonBase(c).Label("Spoken Language").Tooltip("Change the Spoken Language").
			Row().Menu(func(m *ui.Menu) { b.spokenMenu(m) })
	}
	bg := ui.RGBA(255, 255, 255, 0.1)
	if switchable && chip.Hovered() {
		bg = ui.RGBA(255, 255, 255, 0.18)
	}
	chip.Height(s.chip+10).Padding(0, 8).Radius(6).Gap(5).AlignItems(ui.Center).
		Background(bg).TextColor(ui.RGBA(255, 255, 255, 0.72)).Children(func() {
		ui.Icon(c, sourceIcon(b.cfg.Source)).Size(s.chip, s.chip)
		short := "Mic"
		if b.cfg.Source == store.SourceSystem {
			short = "This Mac"
		}
		spoken, _, _ := strings.Cut(langName(b.cfg.LangA), " (")
		ui.Text(c, short+" · "+spoken).FontSize(s.chip).FontWeight(500).SingleLine()
		if switchable {
			ui.Icon(c, kit.ChevronDownIcon).Size(s.chip-3, s.chip-3)
		}
	})
	b.translatePill(c)
	ui.Spacer(c)
	b.highlightsHeader(c, snap)
	b.app.ext.CaptionHeader(c, b.addonSession())
	switch {
	case b.running():
		stop := ui.ButtonBase(c).Label("Stop").Tooltip("Stop Recording ("+shortcutText+")").
			Height(s.chip+12).Padding(0, 10).Radius(6).Gap(6).Row().AlignItems(ui.Center).
			FontSize(s.timer).FontWeight(600).TextColor(ui.Hex("#1d1a18")).Background(ui.RGB(255, 255, 255))
		if stop.Hovered() {
			stop.Background(ui.RGBA(255, 255, 255, 0.88))
		}
		if stop.Clicked() {
			b.app.ToggleCaptions()
		}
		stop.Children(func() {
			ui.Box(c).Size(8, 8).Radius(2).Background(kit.LiveRed)
			ui.Text(c, "Stop").SingleLine()
		})
	case b.stopping:
		return // finishing: nothing to press
	default: // it failed, or ended on its own
		close := ui.ButtonBase(c).Label("Close").Height(24).Padding(0, 10).Radius(6).Border(1, ui.RGBA(255, 255, 255, 0.25)).
			FontSize(13).TextColor(kit.BarText)
		if close.Hovered() {
			close.Background(ui.RGBA(255, 255, 255, 0.1))
		}
		if close.Clicked() {
			b.close()
		}
		close.Children(func() { ui.Text(c, "Close").SingleLine() })
	}
	// Minimize and Maximize show while the pointer is over the bar.
	controls := ui.Row(c).Gap(2)
	if !hovered {
		controls.Opacity(0)
	}
	controls.Children(func() {
		if b.running() && barButton(c, kit.MinimizeIcon, "Minimize", 26, false).Clicked() {
			b.minimize()
		}
		icon, label := kit.MaximizeIcon, "Maximize"
		if b.maximized {
			icon, label = kit.RestoreIcon, "Restore"
		}
		if barButton(c, icon, label, 26, false).Clicked() {
			b.win.ToggleMaximize()
		}
	})
}

// translatePill turns translation on and off with a click, showing the
// language translations are in: filled while on, outlined while off.
func (b *captionBar) translatePill(c *ui.Context) {
	s := b.scale()
	on := b.cfg.Translate
	target, _, _ := strings.Cut(langName(b.cfg.LangB), " (")
	pill := ui.ButtonBase(c).Label("Translate").Tooltip("Translate to "+langName(b.cfg.LangB)+" ("+translateShortcutText+")").
		Checked(on).Disabled(!b.running()).Height(s.chip+10).Padding(0, 8).Radius(6).Gap(5).Row().AlignItems(ui.Center)
	switch {
	case on:
		pill.Background(ui.RGBA(255, 255, 255, 0.9)).TextColor(ui.Hex("#1d1a18"))
	case pill.Hovered():
		pill.Background(ui.RGBA(255, 255, 255, 0.1)).TextColor(ui.RGBA(255, 255, 255, 0.72))
	default:
		pill.TextColor(kit.BarFaint)
	}
	pill.Border(1, ui.RGBA(255, 255, 255, 0.3))
	if pill.Clicked() {
		b.app.ToggleTranslation()
	}
	pill.Children(func() {
		ui.Icon(c, kit.TranslateIcon).Size(s.chip, s.chip)
		ui.Text(c, target).FontSize(s.chip).FontWeight(500).SingleLine()
	})
}

// spokenMenu lists the spoken languages, the session's checked.
func (b *captionBar) spokenMenu(m *ui.Menu) {
	for _, l := range lang.Spoken() {
		if m.Item(l.Name).Checked(l.Code == b.cfg.LangA).Chosen() {
			b.setSpoken(l.Code)
		}
	}
}

// sideBySide reports whether translations show to the right of what was
// said: when the setting asks for it, and there is a translation.
func (b *captionBar) sideBySide(lines []session.Line) bool {
	if b.app.prefs.CaptionsDisplay != DisplaySideBySide {
		return false
	}
	// Turned off, the lines translated before keep their column.
	return b.translating() || slices.ContainsFunc(lines, func(l session.Line) bool { return l.TranslationLang != "" })
}

// translating reports whether the next turns can be translated: translation
// is on, into another language than the one spoken.
func (b *captionBar) translating() bool { return b.cfg.Translate && b.cfg.LangA != b.cfg.LangB }

// transcriptView shows the session's turns, each at its time with its
// translation, and the turn being recognized, older turns fading. It
// glides to keep the latest in view until the user scrolls up, and again
// once they scroll back down. Over a clear background, the text gets a
// backing of its own to stay legible over whatever is behind the bar.
func (b *captionBar) transcriptView(c *ui.Context, snap session.Snapshot, transparency float32) {
	s := b.scale()
	sideBySide := b.sideBySide(snap.Lines)
	lines := snap.Lines
	n := len(lines)
	if b.running() || snap.Draft != "" {
		n++ // the turn being recognized
	}
	b.list.FollowEnd, b.list.FollowAnimation = true, &ui.Smooth
	b.list.Key = func(i int) any {
		if i < len(lines) {
			return lines[i].Seq
		}
		return "draft"
	}
	t := &turnStyle{scale: s, sideBySide: sideBySide, backing: max(transparency-0.5, 0) * 2 * 0.6,
		marks: shownHighlights(snap.Highlights)}
	t.tag = func(h store.Highlight, at ui.Rect, i int) {
		stamp := func(seq int) string { return stampOf(lines, seq) }
		tt := cardTurn{speaker: lines[i].Speaker, stamp: clock(time.Duration(lines[i].StartMs) * time.Millisecond), said: lines[i].Text,
			ref: refsOf(snap.Highlights, stamp), at: stamp}
		b.barCard(at, h, tt)
	}
	named := session.ManySpeakers(lines, lineSpeaker)
	ui.List(c, &b.list, n, func(i int) {
		row := ui.Column(c).AlignItems(ui.Stretch)
		if age := n - 1 - i; age < len(s.fade) {
			row.Opacity(s.fade[age])
		} else {
			row.Opacity(s.fade[len(s.fade)-1])
		}
		if b.stopping {
			row.Opacity(0.6)
		}
		row.Children(func() {
			if i < len(lines) {
				t.line(c, lines[i], i, speakerChange(lines, i, named, lineSpeaker))
				return
			}
			b.draftView(c, snap, t)
		})
	}).Grow(1).Justify(ui.End).Gap(s.turnGap).Padding(s.padding[0], s.padding[1], s.padding[2], s.padding[3])
	if b.tip && b.app.profile.Name == "" {
		ui.Column(c).Padding(0, s.padding[1], s.padding[2], s.padding[3]).AlignItems(ui.Stretch).Children(func() {
			name, closed := nameTip(c, &b.tipName)
			if name != "" {
				p := b.app.profile
				p.Name = name
				b.app.saveProfile(p)
			}
			if name != "" || closed {
				b.tip = false
			}
		})
	}
}

// highlightsHeader is the header's pill for a highlight for the user,
// which shows its line, and the count of highlights, a menu of them.
// Where the bar is narrow, the count alone shows, tinted.
func (b *captionBar) highlightsHeader(c *ui.Context, snap session.Snapshot) {
	width, _ := c.Size()
	narrow := width < barPillMin
	if b.att.pill != nil && !narrow {
		h, opened, closed := highlightsPill(c, snap.Highlights, b.att.pill, snap.Lines)
		if opened {
			b.showLine(h)
		}
		if closed {
			b.att.pill = nil
		}
		if b.att.pill != nil {
			c.After(pillFor - c.Now().Sub(b.att.since))
		}
	}
	if h, ok := highlightsMenu(c, snap.Highlights, snap.Lines, narrow && b.att.pill != nil); ok {
		b.showLine(h)
	}
}

// turnStyle sets the turns of the bar.
type turnStyle struct {
	scale      *barScale
	sideBySide bool
	backing    float32 // the opacity of the backing behind text, 0 for none
	// marks are the highlights, by seq; tag opens one's card from its
	// tag, at, the line i of the bar.
	marks map[int]store.Highlight
	tag   func(h store.Highlight, at ui.Rect, i int)
}

// text is a caption's text, on its backing when the bar is clear.
func (t *turnStyle) text(c *ui.Context, s string) ui.Element {
	e := ui.Text(c, s)
	if t.backing > 0 {
		e.Background(kit.BarTint.Alpha(t.backing)).Padding(1, 6).Radius(4).AlignSelf(ui.Start)
	}
	return e
}

const columnGap = 28 // between what was said and its translation, side by side

// line is one turn: its time, what was said and, larger, its translation,
// below it or, side by side, to its right. Captions without translation
// show what was said large. Who spoke heads the turn when it names them.
func (t *turnStyle) line(c *ui.Context, l session.Line, i int, speaker string) {
	s := t.scale
	if l.Gap {
		ui.Text(c, "Connection lost").FontSize(s.stamp).FontWeight(500).TextColor(kit.BarFaint).Margin(0, 0, 0, s.stampWidth)
		return
	}
	h, marked := t.marks[l.Seq]
	turn := ui.Column(c).AlignItems(ui.Stretch)
	if marked {
		markBarLine(turn, h.ShownKind(), drawn(c, h))
	}
	turn.Children(func() { t.lineBody(c, l, speaker, h, marked, i) })
}

// lineBody is a turn of the bar: who said it, its time, what was said and
// its translation, then a highlight's tag.
func (t *turnStyle) lineBody(c *ui.Context, l session.Line, speaker string, h store.Highlight, marked bool, i int) {
	s := t.scale
	if speaker != "" {
		t.text(c, speaker).FontSize(s.stamp).FontWeight(600).TextColor(kit.BarFaint).Margin(0, 0, 2, s.stampWidth)
	}
	ui.Row(c).AlignItems(ui.Start).Children(func() {
		t.text(c, clock(time.Duration(l.StartMs)*time.Millisecond)).FontSize(s.stamp).FontWeight(500).TextColor(kit.BarFaint).
			FontFeatures(kit.Tabular).Width(s.stampWidth).Margin(3, 0, 0, 0)
		if t.sideBySide && l.TranslationLang != "" {
			ui.Column(c).Grow(1).Basis(0).Margin(0, columnGap, 0, 0).Children(func() {
				t.text(c, l.Text).FontSize(s.side).LineHeight(s.sideLine / s.side).TextColor(kit.BarText.Alpha(0.72))
			})
			ui.Column(c).Grow(1).Basis(0).Children(func() {
				t.translation(c, l, s.side, s.sideLine)
			})
		} else {
			ui.Column(c).Grow(1).Basis(0).Gap(1).Children(func() {
				if l.TranslationLang == "" {
					t.text(c, l.Text).FontSize(s.big).LineHeight(s.bigLine / s.big).FontWeight(600)
					return
				}
				t.text(c, l.Text).FontSize(s.said).LineHeight(s.saidLine / s.said).TextColor(kit.BarMuted)
				t.translation(c, l, s.big, s.bigLine)
			})
		}
		if marked {
			tag := barTag(c, h.ShownKind(), s.stamp+1)
			if tag.Clicked() && t.tag != nil {
				t.tag(h, tag.Bounds(), i)
			}
		}
	})
}

// translation is a turn's translation, or dots while it is pending.
func (t *turnStyle) translation(c *ui.Context, l session.Line, size, line float32) {
	switch {
	case l.Translation != "":
		t.text(c, l.Translation).FontSize(size).LineHeight(line / size).FontWeight(600)
	case l.Translating:
		t.dots(c).Margin(line/2-3, 0, 0, 0)
	}
}

// dots show a turn being recognized or translated, on a backing when the
// bar is clear.
func (t *turnStyle) dots(c *ui.Context) ui.Element {
	if t.backing == 0 {
		return kit.WorkingDots(c, kit.BarText)
	}
	return ui.Row(c).Padding(5, 6).Radius(4).Background(kit.BarTint.Alpha(t.backing)).AlignSelf(ui.Start).Children(func() {
		kit.WorkingDots(c, kit.BarText)
	})
}

// draftView is the turn being recognized, or a hint before anyone speaks,
// with dots showing Capy listens. Side by side, it keeps to the column of
// what was said.
func (b *captionBar) draftView(c *ui.Context, snap session.Snapshot, t *turnStyle) {
	s := t.scale
	captionsOnly := !b.translating()
	ui.Row(c).Margin(0, 0, 0, s.stampWidth).Children(func() {
		draft := ui.Row(c).Grow(1).Basis(0).Gap(10).AlignItems(ui.Center)
		if t.sideBySide {
			draft.Margin(0, columnGap, 0, 0)
		}
		draft.Children(func() {
			switch {
			case snap.Draft != "":
				size := s.said
				switch {
				case captionsOnly:
					size = s.big
				case t.sideBySide:
					size = s.side
				}
				t.text(c, snap.Draft).FontSize(size).Italic().TextColor(ui.RGBA(255, 255, 255, 0.5))
			case len(snap.Lines) == 0:
				t.text(c, b.hint(snap)).FontSize(s.big).FontWeight(500).TextColor(ui.RGBA(255, 255, 255, 0.55))
			}
			if b.running() && !b.starting {
				t.dots(c)
			}
		})
		if t.sideBySide {
			ui.Box(c).Grow(1).Basis(0) // the translations' column
		}
	})
}

// hint is what the bar says before the first words.
func (b *captionBar) hint(snap session.Snapshot) string {
	system := b.cfg.Source == store.SourceSystem
	switch {
	case b.starting || snap.StartedAt.IsZero() && b.running():
		if system {
			return "Getting ready to listen to This Mac…"
		}
		return "Getting the microphone ready…"
	case system:
		return "Play something with speech…"
	}
	return "Start speaking…"
}

func (b *captionBar) problemView(c *ui.Context, p problem) {
	icon := kit.WarnIcon
	switch {
	case p.pane == audio.MicrophonePane:
		icon = kit.MicOffIcon
	case p.pane != "":
		icon = kit.LaptopIcon
	case p.setKey:
		icon = kit.KeyIcon
	}
	ui.Row(c).Grow(1).Gap(14).Padding(0, 26, 8, 16).AlignItems(ui.Center).Children(func() {
		ui.Icon(c, icon).Size(22, 22).TextColor(kit.BarMuted)
		ui.Column(c).Grow(1).Basis(0).Gap(3).Children(func() {
			ui.Text(c, p.message).FontSize(15).FontWeight(600)
			if p.detail != "" {
				ui.Text(c, p.detail).FontSize(13).TextColor(kit.BarMuted)
			}
		})
		action := ""
		switch {
		case p.pane != "":
			action = "Open System Settings"
		case p.setKey:
			action = "Settings…"
		}
		if action == "" {
			return
		}
		bt := ui.ButtonBase(c).Label(action).Height(28).Padding(0, 14).Radius(6).Center().
			FontWeight(600).TextColor(ui.Hex("#1d1a18")).Background(ui.RGB(255, 255, 255))
		if bt.Clicked() {
			if p.pane != "" {
				openPane(p.pane)
			} else {
				b.app.OpenSettings()
			}
		}
		bt.Children(func() { ui.Text(c, action).SingleLine() })
	})
}
