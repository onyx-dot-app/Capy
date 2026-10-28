package views

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
)

// How a highlight for the user draws attention while recording.
const (
	// nudgeMargin is how much surer than θ Decisions must be of a
	// highlight for the user for it to draw attention.
	nudgeMargin = 0.1
	// nudgeEvery is how long after one nudge the next only joins its pill.
	nudgeEvery = 2 * time.Minute
	// pillFor is how long the pill stays before it folds into the count.
	pillFor = 3 * time.Minute
	// glowFor is the bar's one breath: in, then out; calmFor how long its
	// border is terracotta instead while the desktop asks for less motion.
	glowFor = 2400 * time.Millisecond
	calmFor = 3 * time.Second
	// drawFor is how long a new highlight's rule takes to draw down.
	drawFor = 400 * time.Millisecond
	// barPillMin is the narrowest bar that shows the pill: below, the
	// count alone, tinted.
	barPillMin = 560
	tick       = 16 * time.Millisecond // a frame of a motion
)

// glowColor is the bar's terracotta, and youOnBar the pill's text on it.
var (
	glowColor = ui.RGB(232, 124, 80)
	youOnBar  = ui.Hex("#FFB596")
)

// attention is what a recording's highlights ask of the views: the pill
// naming those for the user, the bar's glow, the capsule's badge and the
// edge pill of Capy's window. Each highlight is weighed once, on the main
// thread, as the first view to draw it sees it.
type attention struct {
	known map[string]bool
	// pill is the highlights the pill names, oldest first, nil for none;
	// since is when it came, last when the last of them did.
	pill        []string
	since, last time.Time
	glows       bool // the pill came with the glow, not within nudgeEvery of another
	// badge is the capsule's For you, until the bar shows again, for the
	// highlight badgeID.
	badge   bool
	badgeID string
	// edge is the highlight Capy's window points at while its line is out
	// of view, edgeAt when it came.
	edge   string
	edgeAt time.Time
}

// follow weighs the highlights new since the last frame: one for the user
// that Decisions is surest of draws attention when nudge allows it. One
// within nudgeEvery of the last joins its pill, quietly.
func (a *attention) follow(now time.Time, snap session.Snapshot, nudge bool) {
	if a.known == nil {
		a.known = map[string]bool{}
	}
	sure := snap.Config.Highlights.Theta() + nudgeMargin
	for _, h := range snap.Highlights {
		if a.known[h.ID] {
			continue
		}
		a.known[h.ID] = true
		if !nudge || h.Dismissed || !h.Kind.ForYou() || h.Confidence < sure {
			continue
		}
		recent := !a.last.IsZero() && now.Sub(a.last) < nudgeEvery
		switch {
		case a.pill != nil && recent:
			a.pill = append(a.pill, h.ID)
		default:
			a.pill, a.since, a.glows = []string{h.ID}, now, !recent
		}
		a.last, a.badge, a.badgeID, a.edge, a.edgeAt = now, true, h.ID, h.ID, now
	}
	// One dismissed meanwhile, on the bar's card, leaves the pill.
	a.pill = slices.DeleteFunc(a.pill, func(id string) bool {
		h, ok := highlightOf(snap.Highlights, id)
		return !ok || h.Dismissed
	})
	if len(a.pill) == 0 {
		a.pill = nil
	}
	if a.pill != nil && now.Sub(a.since) >= pillFor {
		a.pill = nil
	}
	if a.edge != "" && now.Sub(a.edgeAt) >= pillFor {
		a.edge = ""
	}
}

// glow is how much the bar glows now, from 0 to 1, and whether it is the
// calm one, a terracotta border without motion.
func (a *attention) glow(c *ui.Context) (float32, bool) {
	if a.pill == nil || !a.glows {
		return 0, false
	}
	t := c.Now().Sub(a.since)
	if c.Preferences().ReduceMotion {
		if t >= calmFor {
			return 0, true
		}
		c.After(calmFor - t)
		return 1, true
	}
	if t >= glowFor {
		return 0, false
	}
	c.After(tick)
	return float32(math.Sin(math.Pi * float64(t) / float64(glowFor))), false
}

// highlightOf is highlight id among hs.
func highlightOf(hs []store.Highlight, id string) (store.Highlight, bool) {
	i := slices.IndexFunc(hs, func(h store.Highlight) bool { return h.ID == id })
	if i < 0 {
		return store.Highlight{}, false
	}
	return hs[i], true
}

// lineOf is the index of turn seq among lines, -1 when it has none.
func lineOf(lines []session.Line, seq int) int {
	return slices.IndexFunc(lines, func(l session.Line) bool { return l.Seq == seq })
}

// drawn is how far a highlight that just came has drawn, from 0 to 1:
// its rule down the gutter and its wash in. One older, or with less
// motion asked for, is drawn.
func drawn(c *ui.Context, h store.Highlight) float32 {
	t := c.Now().Sub(h.CreatedAt)
	if t >= drawFor || t < 0 || c.Preferences().ReduceMotion {
		return 1
	}
	c.After(tick)
	return float32(t) / float32(drawFor)
}

// stampOf is when turn seq of lines was said, as a transcript shows it.
func stampOf(lines []session.Line, seq int) string {
	if i := lineOf(lines, seq); i >= 0 {
		return clock(time.Duration(lines[i].StartMs) * time.Millisecond)
	}
	return ""
}

// firstWords is the start of what was said, for a menu.
func firstWords(s string) string {
	const n = 6
	words := strings.Fields(s)
	if len(words) <= n {
		return strings.Join(words, " ")
	}
	return strings.Join(words[:n], " ") + "…"
}

// barTag is a highlight's tag on a line of the caption bar, in its kind's
// bright tint. It reports a click.
func barTag(c *ui.Context, k store.HighlightKind, size float32) ui.Element {
	look := kindLooks[k]
	b := ui.ButtonBase(c).Label(k.Label()).Row().Gap(5).AlignItems(ui.Center).Padding(4, 4, 2, 4).Radius(5).
		TextColor(look.dark).Margin(0, 0, 0, 10)
	if b.Hovered() {
		b.Background(look.dark.Alpha(0.16))
	}
	b.Children(func() {
		ui.Icon(c, look.icon).Size(size, size)
		ui.Text(c, k.Label()).FontSize(size - 0.5).FontWeight(600).SingleLine()
	})
	return b
}

// markBarLine draws a highlighted line of the bar: the rule and a wash in
// the kind's bright tint.
func markBarLine(row ui.Element, k store.HighlightKind, drawn float32) {
	color := kindLooks[k].dark
	row.Background(color.Alpha(0.16*drawn)).Radius(7).Padding(5, 8, 5, 10).Margin(-5, -8, -5, -10).
		Draw(func(p *ui.Painter, r ui.Rect) {
			p.Fill(ui.Rect{X: r.X, Y: r.Y + 6, W: 3, H: max(r.H-12, 0) * drawn}, color, 1.5)
		})
}

// highlightsPill is the bar header's pill for the user's highlights,
// naming the newest, and its ×. It returns the highlight clicked, if any,
// and whether × was.
func highlightsPill(c *ui.Context, hs []store.Highlight, ids []string, lines []session.Line) (open store.Highlight, opened, closed bool) {
	newest, ok := highlightOf(hs, ids[len(ids)-1])
	if !ok {
		return store.Highlight{}, false, false
	}
	kind := newest.ShownKind()
	look := kindLooks[kind]
	text := kind.Label() + " · " + stampOf(lines, newest.Seq)
	switch {
	case len(ids) > 1:
		text = kind.Label() + " · " + strconv.Itoa(len(ids))
	case newest.Sentence() != "":
		text = newest.Sentence()
	}
	ui.Row(c).Height(26).Radius(13).Background(glowColor.Alpha(0.2)).TextColor(youOnBar).AlignItems(ui.Center).Children(func() {
		b := ui.ButtonBase(c).Label(text).Tooltip("Show the line").Row().Gap(6).AlignItems(ui.Center).Padding(0, 6, 0, 10).Height(26).Radius(13)
		if b.Hovered() {
			b.Background(glowColor.Alpha(0.12))
		}
		opened = b.Clicked()
		b.Children(func() {
			ui.Icon(c, look.icon).Size(13, 13)
			ui.Text(c, text).FontSize(12.5).FontWeight(500).SingleLine().FontFeatures(kit.Tabular)
		})
		x := ui.ButtonBase(c).Label("Dismiss").Tooltip("Dismiss").Size(20, 20).Radius(10).Center().Margin(0, 4, 0, 0)
		if x.Hovered() {
			x.Background(glowColor.Alpha(0.16))
		}
		closed = x.Clicked()
		x.Children(func() { ui.Icon(c, kit.CloseIcon).Size(9, 9) })
	})
	return newest, opened, closed
}

// highlightsMenu is the bar header's count: a menu of the session's
// highlights, newest first, terracotta when one is for the user and tint
// asks for it. It returns the one chosen.
func highlightsMenu(c *ui.Context, hs []store.Highlight, lines []session.Line, tint bool) (store.Highlight, bool) {
	shown := slices.DeleteFunc(slices.Clone(hs), func(h store.Highlight) bool { return h.Dismissed })
	if len(shown) == 0 {
		return store.Highlight{}, false
	}
	color := ui.RGBA(255, 255, 255, 0.78)
	if tint && slices.ContainsFunc(shown, func(h store.Highlight) bool { return h.ShownKind().ForYou() }) {
		color = youOnBar
	}
	var chosen store.Highlight
	picked := false
	count := strconv.Itoa(len(shown))
	b := ui.ButtonBase(c).Label(count+" highlights").Tooltip("Highlights").Height(26).Padding(0, 10).Radius(13).Row().Gap(6).
		AlignItems(ui.Center).Background(ui.RGBA(255, 255, 255, 0.12)).TextColor(color)
	if b.Hovered() {
		b.Background(ui.RGBA(255, 255, 255, 0.18))
	}
	b.Menu(func(m *ui.Menu) {
		for i := len(shown) - 1; i >= 0; i-- {
			h := shown[i]
			said := h.Sentence()
			if j := lineOf(lines, h.Seq); said == "" && j >= 0 {
				said = firstWords(lines[j].Text)
			}
			if m.Item(h.ShownKind().Label() + " · " + stampOf(lines, h.Seq) + " · " + said).Chosen() {
				chosen, picked = h, true
			}
		}
	})
	b.Children(func() {
		ui.Icon(c, sparkIcon).Size(13, 13)
		ui.Text(c, count).FontSize(12.5).FontWeight(500).FontFeatures(kit.Tabular).SingleLine()
	})
	return chosen, picked
}

// capsuleBadge is the capsule's For you, while a highlight for the user
// waits to be seen. It reports a click.
func capsuleBadge(c *ui.Context) bool {
	b := ui.ButtonBase(c).Label("For you").Tooltip("Show the line").Height(22).Padding(0, 8, 0, 6).Radius(11).Row().Gap(4).
		AlignItems(ui.Center).Background(glowColor.Alpha(0.25)).TextColor(youOnBar)
	if b.Hovered() {
		b.Background(glowColor.Alpha(0.35))
	}
	b.Children(func() {
		ui.Icon(c, askedIcon).Size(12, 12)
		ui.Text(c, "For you").FontSize(12).SingleLine()
	})
	return b.Clicked()
}

// edgePill points Capy's window at a highlight for the user whose line is
// out of view, above or below: its kind, who said it and how long ago. It
// returns whether it was clicked, and whether × was.
func edgePill(c *ui.Context, h store.Highlight, speaker string, ago time.Duration, above bool) (opened, closed bool) {
	p := kit.Colors(c)
	arrow := downIcon
	if above {
		arrow = upIcon
	}
	when := "just now"
	if m := int(ago.Minutes()); m == 1 {
		when = "1 min ago"
	} else if m > 1 {
		when = strconv.Itoa(m) + " min ago"
	}
	detail := " · " + when
	if speaker != "" {
		detail = " · " + speaker + detail
	}
	kind := h.ShownKind()
	color := kindColor(c, kind)
	ui.Row(c).Height(28).Radius(14).Padding(0, 6, 0, 0).Background(p.Window).Border(0.5, ui.RGBA(0, 0, 0, 0.16)).
		Shadow(0, 4, 14, 0, ui.RGBA(0, 0, 0, 0.12)).AlignItems(ui.Center).Children(func() {
		b := ui.ButtonBase(c).Label(kind.Label()+detail).Height(28).Padding(0, 4, 0, 10).Radius(14).Row().Gap(7).AlignItems(ui.Center)
		opened = b.Clicked()
		b.Children(func() {
			ui.Icon(c, arrow).Size(13, 13).TextColor(color)
			ui.RichText(c).FontSize(12.5).SingleLine().Children(func() {
				ui.Text(c, kind.Label()).FontWeight(600).TextColor(color)
				ui.Text(c, detail).TextColor(p.Secondary)
			})
		})
		x := ui.ButtonBase(c).Label("Dismiss").Tooltip("Dismiss").Size(18, 18).Radius(9).Center().TextColor(p.Tertiary)
		if x.Hovered() {
			x.Background(p.Fill2)
		}
		closed = x.Clicked()
		x.Children(func() { ui.Icon(c, kit.CloseIcon).Size(9, 9) })
	})
	return opened, closed
}

var (
	upIcon   = kit.StrokeIcon(`<path d="M12 19V5M6 11l6-6 6 6"/>`)
	downIcon = kit.StrokeIcon(`<path d="M12 5v14M6 13l6 6 6-6"/>`)
)

// awayRibbon tells what was highlighted while the user was scrolled up:
// how many, a chip for each that jumps to its line, and Jump to Latest. It
// returns the highlight chosen, and whether Jump to Latest was.
func awayRibbon(c *ui.Context, hs []store.Highlight) (chosen store.Highlight, picked, latest bool) {
	p := kit.Colors(c)
	n := "1 highlight"
	if len(hs) > 1 {
		n = strconv.Itoa(len(hs)) + " highlights"
	}
	ui.Row(c).Gap(10).Padding(8, 10, 8, 14).Radius(10).Background(p.Window.Alpha(0.97)).Border(0.5, ui.RGBA(0, 0, 0, 0.16)).
		Shadow(0, 6, 20, 0, ui.RGBA(0, 0, 0, 0.12)).AlignItems(ui.Center).Children(func() {
		ui.Icon(c, sparkIcon).Size(14, 14).TextColor(p.Link)
		ui.RichText(c).FontSize(12.5).SingleLine().Children(func() {
			ui.Text(c, n).FontWeight(600)
			ui.Text(c, " since you scrolled up").TextColor(p.Secondary)
		})
		ui.Row(c).Gap(6).Grow(1).Basis(0).MinWidth(0).Clip().Children(func() {
			for _, h := range hs {
				if noteButton(c, h.ShownKind(), false).Key(h.ID).Clicked() {
					chosen, picked = h, true
				}
			}
		})
		jump := ui.ButtonBase(c).Label("Jump to Latest").Height(24).Padding(0, 9).Radius(6).Row().Gap(5).AlignItems(ui.Center).
			Background(p.Control).Border(0.5, ui.RGBA(0, 0, 0, 0.2)).Shadow(0, 1, 1.5, 0, ui.RGBA(0, 0, 0, 0.1))
		latest = jump.Clicked()
		jump.Children(func() {
			ui.Icon(c, downIcon).Size(11, 11)
			ui.Text(c, "Jump to Latest").FontSize(12).SingleLine()
		})
	})
	return chosen, picked, latest
}

// nameTip asks, once, the name of a user who has none in Profile, so that
// Highlights can mark the lines for them. It returns the name saved, and
// whether × was clicked.
func nameTip(c *ui.Context, name *string) (saved string, closed bool) {
	ui.Row(c).Gap(10).Padding(8, 10, 8, 12).Radius(9).Background(ui.RGBA(255, 255, 255, 0.08)).AlignItems(ui.Center).
		Margin(10, 0, 0, 0).Children(func() {
		ui.Icon(c, sparkIcon).Size(14, 14).TextColor(kindLooks[store.KindAskedYou].dark)
		ui.Text(c, "Capy can mark lines that mention you. What’s your name?").FontSize(12.5).TextColor(ui.RGBA(255, 255, 255, 0.8)).
			Grow(1).Basis(0)
		in := ui.TextInputBase(c, name).Label("Your name").Placeholder("Your name").Width(150).Height(24).Padding(0, 8).Radius(6).
			Background(ui.RGBA(255, 255, 255, 0.12)).TextColor(kit.BarText).FontSize(12.5)
		save := ui.ButtonBase(c).Label("Save").Height(24).Padding(0, 10).Radius(12).Center().Background(ui.RGBA(255, 255, 255, 0.12)).
			Disabled(strings.TrimSpace(*name) == "")
		if (save.Clicked() || in.Submitted()) && strings.TrimSpace(*name) != "" {
			saved = *name
		}
		save.Children(func() { ui.Text(c, "Save").FontSize(12.5).FontWeight(500).SingleLine() })
		x := ui.ButtonBase(c).Label("Not now").Tooltip("Not now").Size(24, 24).Radius(12).Center().TextColor(ui.RGBA(255, 255, 255, 0.6))
		closed = x.Clicked()
		x.Children(func() { ui.Icon(c, kit.CloseIcon).Size(10, 10) })
	})
	return saved, closed
}

// barCard opens highlight h's card above the caption bar, in a window of
// its own: the bar is too short to hold it.
func (b *captionBar) barCard(anchor ui.Rect, h store.Highlight, t cardTurn) {
	if b.card != nil {
		b.card.Close()
	}
	s := b.addonSession()
	acts := cardActions{dismiss: func() { b.dismiss(h.ID) }}
	if s != nil && b.app.ext.CanAsk() {
		acts.ask = func() { b.app.ext.Ask(*s, askAbout(t.stamp), true) }
	}
	var fl *mygo.Window
	fl = mygo.NewFlyout(mygo.FlyoutOptions{
		Parent:    b.win,
		Anchor:    mygo.Rectangle{X: int(anchor.X), Y: int(anchor.Y), Width: int(anchor.W), Height: int(anchor.H)},
		Placement: mygo.PlacementTopEnd,
		Gap:       8,
		Width:     cardWidth,
		Height:    cardHeight(h),
		Focusable: true,
		Content: ui.View(func(c *ui.Context) {
			kit.Theme(c)
			c.Root().Background(ui.Transparent)
			acts.copy = func() {
				c.WriteClipboard(insightText(h, t))
				fl.Close()
			}
			done := func() { fl.Close() }
			cardPanel(c, ui.Column(c).Fill(), h, t, acts, done)
		}),
	})
	b.card = fl
}

// barCardHeight is the card's height above the bar, for its kind, a
// sentence of up to four lines and its actions; cardHeight adds what
// polish found.
const barCardHeight = 150
