package views

import (
	"cmp"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
)

// The glyphs of the kinds of highlight, and of the card's actions.
var (
	askedIcon     = kit.StrokeIcon(`<path d="M20 12a8 8 0 0 1-11.6 7.1L4 20l.9-4.1A8 8 0 1 1 20 12z"/><path d="M9.8 9.6a2.3 2.3 0 0 1 4.4.8c0 1.6-2.2 1.9-2.2 3.2"/><path d="M12 16.2h.01"/>`)
	mentionedIcon = kit.StrokeIcon(`<circle cx="12" cy="12" r="3.6"/><path d="M15.6 8.4v4.8a2.6 2.6 0 0 0 5.2 0V12a8.8 8.8 0 1 0-3.6 7.1"/>`)
	decisionIcon  = kit.StrokeIcon(`<path d="M12 3.2l8.8 8.8-8.8 8.8L3.2 12z"/><path d="M8.6 12.2l2.3 2.3 4.6-4.8"/>`)
	actionIcon    = kit.StrokeIcon(`<rect x="4" y="4" width="16" height="16" rx="4"/><path d="M8.5 12.2l2.5 2.5 4.5-5"/>`)
	announceIcon  = kit.StrokeIcon(`<path d="M4 10v4h3l7 4.5v-13L7 10z"/><path d="M17.5 9a4.2 4.2 0 0 1 0 6"/>`)
	headsUpIcon   = kit.StrokeIcon(`<path d="M12 4.2l8.6 15H3.4z"/><path d="M12 10v4.2M12 16.9h.01"/>`)
	debateIcon    = kit.StrokeIcon(`<path d="M4 8.5h14.5L15 5"/><path d="M20 15.5H5.5L9 19"/>`)
	sparkIcon     = kit.StrokeIcon(`<path d="M12 3.5l1.9 5.4 5.4 1.9-5.4 1.9L12 18.1l-1.9-5.4-5.4-1.9 5.4-1.9z"/><path d="M18.5 16v4M16.5 18h4"/>`)
	askAboutIcon  = thinIcon(`<path d="M4.5 5h15A1.5 1.5 0 0 1 21 6.5v9a1.5 1.5 0 0 1-1.5 1.5H11l-4.5 3.5V17h-2A1.5 1.5 0 0 1 3 15.5v-9A1.5 1.5 0 0 1 4.5 5z"/>`)
	copyLineIcon  = thinIcon(`<rect x="8.5" y="8" width="11" height="13" rx="2"/><path d="M5 16V5.5A2.5 2.5 0 0 1 7.5 3H15"/>`)
)

// thinIcon is a 24×24 icon of round 1.8-pixel strokes, as the card's
// buttons draw theirs.
func thinIcon(paths string) *ui.SVG {
	return ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="#000" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">` + paths + `</svg>`))
}

// kindLook is how a kind of highlight shows: its glyph, its color on
// Capy's light window and on a dark one or the caption bar, and the
// card's sentence of why the turn was marked, which is never the model's.
// Its note in the margin is the kind's Label.
type kindLook struct {
	icon        *ui.SVG
	light, dark ui.Color
	why         string
}

var kindLooks = map[store.HighlightKind]kindLook{
	store.KindAskedYou:     {askedIcon, ui.Hex("#B54E24"), ui.Hex("#F08A5D"), "Someone asked you to do something."},
	store.KindMentionedYou: {mentionedIcon, ui.Hex("#B54E24"), ui.Hex("#F08A5D"), "Someone talked about you."},
	store.KindDecision:     {decisionIcon, ui.Hex("#2F7D5B"), ui.Hex("#5FC79A"), "The group made a decision."},
	store.KindAction:       {actionIcon, ui.Hex("#3A6EA5"), ui.Hex("#78A9E0"), "Someone took on a task."},
	store.KindAnnouncement: {announceIcon, ui.Hex("#7E57B0"), ui.Hex("#B593E6"), "Something was announced that changes things."},
	store.KindHeadsUp:      {headsUpIcon, ui.Hex("#B5476B"), ui.Hex("#E57FA0"), "Someone raised a risk or a blocker."},
	store.KindDebate:       {debateIcon, ui.Hex("#9A5B0B"), ui.Hex("#E9A94B"), "People disagreed on what to do."},
}

// kindColor is kind k's color in the view's appearance.
func kindColor(c *ui.Context, k store.HighlightKind) ui.Color {
	if c.Theme().Dark {
		return kindLooks[k].dark
	}
	return kindLooks[k].light
}

// forYouColor is the color of what is for the user, as the meta line's
// count is when something is.
func forYouColor(c *ui.Context) ui.Color { return kindColor(c, store.KindAskedYou) }

// Widths of the transcript's margin, where the highlights are.
const (
	// insightColumn is the column of insight cards right of the text, the
	// gap before it included; minInsightText the least width the text
	// keeps beside it, below which the margin has glyphs.
	insightColumn  = 268
	minInsightText = 340
	glyphColumn    = 24 // a glyph's, where the pane is too narrow for cards
)

// shownHighlights are the highlights not dismissed, by the turn's seq.
func shownHighlights(hs []store.Highlight) map[int]store.Highlight {
	out := map[int]store.Highlight{}
	for _, h := range hs {
		if !h.Dismissed {
			out[h.Seq] = h
		}
	}
	return out
}

// countText is how many highlights a session has, and how many are for
// the user: "2 highlights · 1 for you"; "" for none. One folded into
// another is not counted, as it is not listed.
func countText(shown map[int]store.Highlight) (text string, forYou int) {
	n := 0
	all := slices.Collect(maps.Values(shown))
	for _, h := range shown {
		if h.FoldedAmong(all) {
			continue
		}
		n++
		if h.ShownKind().ForYou() {
			forYou++
		}
	}
	switch n {
	case 0:
		return "", 0
	case 1:
		text = "1 highlight"
	default:
		text = strconv.Itoa(n) + " highlights"
	}
	if forYou > 0 {
		text += " · " + strconv.Itoa(forYou) + " for you"
	}
	return text, forYou
}

// highlightCount is the meta line's count, in the color of what is for
// the user when something is. It reports a click.
func highlightCount(c *ui.Context, shown map[int]store.Highlight) bool {
	text, forYou := countText(shown)
	if text == "" {
		return false
	}
	p := kit.Colors(c)
	color := p.Secondary
	if forYou > 0 {
		color = forYouColor(c)
	}
	b := ui.ButtonBase(c).Label(text).Row().Gap(4).AlignItems(ui.Center).TextColor(color).Height(22).Radius(5).Padding(0, 4)
	if b.Hovered() {
		b.Background(p.Fill2)
	}
	b.Children(func() {
		ui.Icon(c, sparkIcon).Size(12, 12)
		t := ui.Text(c, text).SingleLine().FontFeatures(kit.Tabular)
		if forYou > 0 {
			t.FontWeight(600)
		}
	})
	return b.Clicked()
}

// turnMark is how a turn of a transcript shows its highlight: its kind,
// "" for none, and the width kept right of the text, for its note, with
// note building it there.
type turnMark struct {
	// under builds what goes under the turn's text: where the pane is too
	// narrow for the insight column, its card, expanded.
	under  func()
	kind   store.HighlightKind
	margin float32
	note   func()
	drawn  float32 // how far a new highlight has drawn in, 0 for drawn
}

// markInset is how far left of a highlighted turn's text its wash and
// rule begin, in the time column.
const markInset = 12

// markTurn draws a highlighted turn's body: the kind's rule in the gutter
// and its wash behind the lines, the text staying where it was; a new one
// draws in, drawn from 0 to 1.
func markTurn(c *ui.Context, body ui.Element, k store.HighlightKind, drawn float32) {
	color := kindColor(c, k)
	wash := float32(0.08)
	if c.Theme().Dark {
		wash = 0.16
	}
	body.Background(color.Alpha(wash*drawn)).Radius(6).Padding(3, 10, 5, markInset).Margin(-3, -10, -5, -markInset).
		Draw(func(p *ui.Painter, r ui.Rect) {
			p.Fill(ui.Rect{X: r.X, Y: r.Y + 5, W: 3, H: max(r.H-10, 0) * drawn}, color, 1.5)
		})
}

// noteButton is a highlight's note in the margin: its glyph and kind, in
// its color, ringed while its card is open.
func noteButton(c *ui.Context, k store.HighlightKind, open bool) ui.Element {
	look, color := kindLooks[k], kindColor(c, k)
	b := ui.ButtonBase(c).Label(k.Label()).Row().Gap(4).AlignItems(ui.Center).Padding(2, 7, 2, 5).Radius(5).
		Background(color.Alpha(0.11)).TextColor(color)
	if open {
		b.Border(1.5, color)
	} else if b.Hovered() {
		b.Background(color.Alpha(0.17))
	}
	b.Children(func() {
		ui.Icon(c, look.icon).Size(12, 12)
		ui.Text(c, k.Label()).FontSize(11.5).FontWeight(600).LineHeight(16.0 / 11.5).SingleLine()
	})
	return b
}

// noteGlyph is a highlight where the pane is too narrow for its note: its
// glyph, its kind in a tooltip.
func noteGlyph(c *ui.Context, k store.HighlightKind) ui.Element {
	look := kindLooks[k]
	b := ui.ButtonBase(c).Label(k.Label()).Tooltip(k.Label()).Size(20, 20).Radius(5).Center().TextColor(kindColor(c, k))
	if b.Hovered() {
		b.Background(kindColor(c, k).Alpha(0.11))
	}
	b.Children(func() { ui.Icon(c, look.icon).Size(14, 14) })
	return b
}

// cardTurn is the turn a card is of: who said it and when, and what was
// said, which Copy copies of a mark; and ref, the time and sentence of
// another highlight of the session, by ID, which a polished card names;
// at, the time of a turn by seq, "" for one not in the transcript, which a
// debate's sides show; and jump, nil where there is no transcript to go
// to, which takes it to a turn.
type cardTurn struct {
	speaker, stamp, said string
	ref                  func(id string) (stamp, sentence string, ok bool)
	at                   func(seq int) string
	jump                 func(seq int)
}

// refsOf looks hs up by ID for a card: the time of each one's turn, by
// stamp, and its sentence.
func refsOf(hs []store.Highlight, stamp func(seq int) string) func(id string) (string, string, bool) {
	return func(id string) (string, string, bool) {
		for _, h := range hs {
			if h.ID == id && !h.Dismissed {
				return stamp(h.Seq), h.Sentence(), true
			}
		}
		return "", "", false
	}
}

// refText names a highlight a card refers to: its sentence, else the
// time of its turn.
func refText(t cardTurn, id string) (string, bool) {
	if t.ref == nil || id == "" {
		return "", false
	}
	stamp, sentence, ok := t.ref(id)
	if !ok {
		return "", false
	}
	if sentence != "" {
		return sentence + " · " + stamp, true
	}
	return "the highlight at " + stamp, true
}

// cardActions are what the card offers: Ask about this, where Ask can
// answer, Copy and Not important.
type cardActions struct {
	ask, copy, dismiss func()
}

// insightCard is highlight h beside its turn, in the insight column or,
// where the pane is narrow, under the turn: its kind, then what polish
// found, or why it was marked, and its facts on one line. Expanded, it has
// all of them, a debate's sides and outcome, and what can be done. It never
// repeats the turn, which is beside it. It reports a click that expands or
// collapses it.
func insightCard(c *ui.Context, h store.Highlight, t cardTurn, expanded bool, acts cardActions, collapse func()) (toggled bool) {
	p := kit.Colors(c)
	kind := h.ShownKind()
	color := kindColor(c, kind)
	label := kind.Label() + ": " + cmp.Or(h.Sentence(), kindLooks[kind].why) + sidesLabel(h)
	if !expanded {
		b := ui.ButtonBase(c).Key(h.ID).Label(label).Tooltip("Show more").Column().AlignItems(ui.Stretch).Gap(3).
			Padding(7, 10, 8).Radius(8).Background(color.Alpha(0.06)).Border(0.5, color.Alpha(0.3))
		if b.Hovered() {
			b.Background(color.Alpha(0.11))
		}
		b.Children(func() {
			insightHeader(c, h, color)
			insightLead(c, h, 3)
			if pol := h.Polish; pol != nil && pol.Debate != nil {
				debateSides(c, *pol, t, 2).FontSize(12)
			}
			if facts := insightFacts(h, t); facts != "" {
				ui.Text(c, facts).FontSize(11.5).TextColor(p.Secondary).SingleLine()
			}
		})
		return b.Clicked()
	}
	ui.Column(c).Key(h.ID).AlignItems(ui.Stretch).Gap(4).Padding(7, 10, 8).Radius(8).Background(p.Popover).
		Border(1, color.Alpha(0.55)).Shadow(0, 4, 14, 0, ui.RGBA(0, 0, 0, 0.08)).Role(ui.RoleGroup).Label(label).Children(func() {
		head := ui.ButtonBase(c).Label("Show less").Tooltip("Show less").Column().AlignItems(ui.Stretch)
		toggled = head.Clicked()
		head.Children(func() { insightHeader(c, h, color) })
		insightLead(c, h, 0)
		insightDetails(c, h, t)
		ui.Box(c).Height(1).Background(p.Separator).Margin(4, 0, 2, 0)
		insightActions(c, acts, collapse, true)
	})
	return toggled
}

// insightHeader is a highlight's kind in its color, a debate's status in
// the color of where it stands, "tentative" for one the agent doubted, and
// which agent polished it.
func insightHeader(c *ui.Context, h store.Highlight, color ui.Color) {
	p := kit.Colors(c)
	kind := h.ShownKind()
	ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
		ui.Icon(c, kindLooks[kind].icon).Size(12, 12).TextColor(color)
		ui.Text(c, kind.Label()).FontSize(11.5).FontWeight(600).TextColor(color).SingleLine()
		if pol := h.Polish; pol != nil {
			if pol.Debate != nil {
				status := statusColor(c, pol.Debate.Status)
				ui.Text(c, string(pol.Debate.Status)).FontSize(10.5).FontWeight(600).TextColor(status).Padding(0, 5).Radius(4).
					Background(status.Alpha(0.12)).SingleLine()
			}
			if pol.Tentative() {
				ui.Text(c, "tentative").FontSize(10.5).TextColor(p.Secondary).Padding(0, 5).Radius(4).Background(p.Fill2).SingleLine()
			}
			ui.Text(c, "✦ "+pol.ByAgent).FontSize(10.5).TextColor(p.Tertiary).SingleLine().Margin(0, 0, 0, ui.Auto)
		}
	})
}

// insightLead is what a highlight says first: polish's sentence, or a
// debate's question; before polish, why the turn was marked. lines bounds
// it, 0 for none.
func insightLead(c *ui.Context, h store.Highlight, lines int) {
	p := kit.Colors(c)
	pol := h.Polish
	var text ui.Element
	switch {
	case pol != nil && pol.Debate != nil:
		text = ui.Text(c, pol.Debate.Topic).FontSize(13).FontWeight(600).LineHeight(18.0 / 13)
	case pol != nil:
		text = ui.Text(c, pol.Text).FontSize(13).LineHeight(18.0 / 13)
		if pol.Tentative() {
			text.TextColor(p.Secondary)
		}
	default:
		text = ui.Text(c, kindLooks[h.ShownKind()].why).FontSize(12.5).LineHeight(17.0 / 12.5).TextColor(p.Secondary)
	}
	if lines > 0 {
		text.MaxLines(lines)
	}
}

// insightFacts is a collapsed card's facts on one line: the fields polish
// found, the decision a debate led to, the highlight it restates.
func insightFacts(h store.Highlight, t cardTurn) string {
	pol := h.Polish
	if pol == nil {
		return ""
	}
	var facts []string
	if pol.Debate != nil {
		if ref, ok := refStamp(t, pol.Debate.Decision); ok {
			facts = append(facts, "→ decision at "+ref)
		}
	}
	for _, f := range polishFields(*pol) {
		facts = append(facts, f[0]+" "+f[1])
	}
	if ref, ok := refStamp(t, pol.FoldedInto); ok {
		facts = append(facts, "Restates "+ref)
	}
	return strings.Join(facts, " · ")
}

// insightDetails is an expanded card's insight: each field polish found,
// a debate's sides in full and the decision it led to, and the highlight
// it restates.
func insightDetails(c *ui.Context, h store.Highlight, t cardTurn) {
	p := kit.Colors(c)
	pol := h.Polish
	if pol == nil {
		return
	}
	if d := pol.Debate; d != nil {
		debateSides(c, *pol, t, 0).FontSize(12.5).Margin(2, 0, 0, 0)
		if ref, ok := refText(t, d.Decision); ok {
			ui.Text(c, "→ "+ref).FontSize(12).TextColor(p.Secondary).Margin(2, 0, 0, 0)
		}
	}
	for _, f := range polishFields(*pol) {
		ui.Row(c).Gap(8).Children(func() {
			ui.Text(c, f[0]).FontSize(11.5).TextColor(p.Tertiary).Width(68).SingleLine()
			ui.Text(c, f[1]).FontSize(12).Grow(1).Basis(0)
		})
	}
	if ref, ok := refText(t, pol.FoldedInto); ok {
		ui.Text(c, "Restates "+ref).FontSize(11.5).TextColor(p.Tertiary)
	}
}

// insightActions are what a card offers: Ask about this, where Ask can
// answer, Copy and Not important; done closes the card first. Narrow
// shows Copy by its icon, for the three to fit the insight column.
func insightActions(c *ui.Context, acts cardActions, done func(), narrow bool) {
	ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
		if acts.ask != nil && cardButton(c, askAboutIcon, "Ask about this", false, false) {
			done()
			acts.ask()
		}
		if cardButton(c, copyLineIcon, "Copy", false, narrow) {
			acts.copy()
		}
		ui.Box(c).Grow(1)
		if cardButton(c, nil, "Not important", true, false) {
			done()
			acts.dismiss()
		}
	})
}

// insightText is what Copy copies of a highlight: its insight once
// polished, else the turn, which is all a mark has.
func insightText(h store.Highlight, t cardTurn) string {
	pol := h.Polish
	switch {
	case pol == nil:
		return t.said
	case pol.Debate != nil:
		lines := []string{pol.Debate.Topic + " (" + string(pol.Debate.Status) + ")"}
		for _, side := range pol.Debate.Sides {
			lines = append(lines, "- "+sideSpeakers(side)+": "+side.Position)
		}
		return strings.Join(lines, "\n")
	}
	lines := []string{pol.Text}
	for _, f := range polishFields(*pol) {
		lines = append(lines, f[0]+": "+f[1])
	}
	return strings.Join(lines, "\n")
}

// cardPanel styles panel as a highlight's card above the caption bar and
// builds its insight; done closes it, after Ask about this or Not
// important.
func cardPanel(c *ui.Context, panel ui.Element, h store.Highlight, t cardTurn, acts cardActions, done func()) {
	p := kit.Colors(c)
	kind := h.ShownKind()
	color := kindColor(c, kind)
	panel.Padding(12, 14, 10).Radius(10).Background(p.Popover).Border(0.5, ui.RGBA(0, 0, 0, 0.2)).
		AlignItems(ui.Stretch).Gap(4).Role(ui.RoleDialog).Label("Highlight: " + kind.Label())
	panel.Children(func() {
		insightHeader(c, h, color)
		insightLead(c, h, 0)
		insightDetails(c, h, t)
		ui.Spacer(c) // a card of a fixed height keeps its actions at its foot
		ui.Box(c).Height(1).Background(p.Separator).Margin(6, 0, 5, 0)
		insightActions(c, acts, done, false)
	})
}

// refStamp is the time of the turn of the highlight a card refers to.
func refStamp(t cardTurn, id string) (string, bool) {
	if t.ref == nil || id == "" {
		return "", false
	}
	stamp, _, ok := t.ref(id)
	return stamp, ok
}

// polishFields are a polish's fields that are set, each its name and
// value.
func polishFields(pol store.Polish) [][2]string {
	var out [][2]string
	for _, f := range [][2]string{{"Owner", pol.Owner}, {"Due", pol.Due}, {"Decided by", decider(pol.DecidedBy)}, {"If", pol.Condition}} {
		if f[1] != "" {
			out = append(out, f)
		}
	}
	return out
}

// decider is who decided, as a card says it: "The group" for the group.
func decider(who string) string {
	if who == "group" {
		return "The group"
	}
	return who
}

// debateSides are a debate's sides, pol's, a text each: who held it in
// bold, then its position and, unbounded, the time of its earliest turn, a
// link where t can jump there. lines bounds each side, 0 for none. A
// tentative debate's are muted. Their size is the caller's, on the column.
func debateSides(c *ui.Context, pol store.Polish, t cardTurn, lines int) ui.Element {
	p := kit.Colors(c)
	who := kindColor(c, store.KindDebate)
	col := ui.Column(c).Gap(3).AlignItems(ui.Stretch)
	if pol.Tentative() {
		who = p.Secondary
		col.TextColor(p.Secondary)
	}
	col.Children(func() {
		for _, side := range pol.Debate.Sides {
			text := ui.RichText(c).LineHeight(17.0 / 12.5)
			if lines > 0 {
				text.MaxLines(lines)
			}
			text.Children(func() {
				ui.Text(c, sideSpeakers(side)).FontWeight(600).TextColor(who)
				ui.Text(c, "  "+side.Position)
				seq, stamp, ok := sideTurn(t, side)
				if lines > 0 || !ok {
					return
				}
				ui.Text(c, " ·\u00a0").TextColor(p.Tertiary) // the time never wraps apart from its dot
				if t.jump == nil {
					ui.Text(c, stamp).TextColor(p.Tertiary).FontFeatures(kit.Tabular)
				} else if ui.Link(c, stamp, "").TextColor(p.Link).FontFeatures(kit.Tabular).Label("Where " + sideSpeakers(side) + " said it, " + stamp).
					Tooltip("Show where it was said").Clicked() {
					t.jump(seq)
				}
			})
		}
	})
	return col
}

// sideSpeakers are who held side, as its text says them: "Someone" for
// none.
func sideSpeakers(side store.Side) string {
	return cmp.Or(strings.Join(side.Speakers, ", "), "Someone")
}

// sidesLabel is what a debate's card or row adds to its label for
// assistive technology, which reads the label, not the texts in it: each
// side as "; Tom: position". "" for any other highlight.
func sidesLabel(h store.Highlight) string {
	if h.Polish == nil || h.Polish.Debate == nil {
		return ""
	}
	var b strings.Builder
	for _, side := range h.Polish.Debate.Sides {
		b.WriteString("; " + sideSpeakers(side) + ": " + side.Position)
	}
	return b.String()
}

// sideTurn is side's earliest turn and its time, by t; false for a side of
// no turns, or one not in the transcript.
func sideTurn(t cardTurn, side store.Side) (seq int, stamp string, ok bool) {
	if len(side.Seqs) == 0 || t.at == nil {
		return 0, "", false
	}
	seq = slices.Min(side.Seqs)
	stamp = t.at(seq)
	return seq, stamp, stamp != ""
}

// statusColor is the color of where a debate stands: settled in the
// Decision's, parked muted, open in the debate's own.
func statusColor(c *ui.Context, s store.DebateStatus) ui.Color {
	switch s {
	case store.DebateDecided, store.DebateConsensus:
		return kindColor(c, store.KindDecision)
	case store.DebateParked:
		return kit.Colors(c).Secondary
	}
	return kindColor(c, store.KindDebate)
}

// sideLines is about how many lines side wraps to in the caption bar's
// card, its time included: a wide rune, as Chinese, Japanese and Korean
// are, counts twice. It errs long, for the card's actions to fit.
func sideLines(side store.Side) int {
	const perLine = 42 // narrow runes of 12.5-point text in the card
	n := 0
	for _, r := range sideSpeakers(side) + "  " + side.Position + " · 00:00" {
		n++
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			n++
		}
	}
	return (n + perLine - 1) / perLine
}

// cardHeight is how tall h's card is above the caption bar: its insight
// and its actions, without the turn, which the bar shows.
func cardHeight(h store.Highlight) int {
	pol := h.Polish
	switch {
	case pol == nil:
		return barCardHeight
	case pol.Debate != nil:
		extra := 20 // the decision it led to
		for _, side := range pol.Debate.Sides {
			extra += 17*sideLines(side) + 3
		}
		return barCardHeight + extra
	}
	extra := 18 * len(polishFields(*pol))
	if pol.FoldedInto != "" {
		extra += 18
	}
	return barCardHeight + extra
}

// cardButton is a button of the card: a white face, or none for a plain
// one; iconOnly shows its icon alone, its label a tooltip. It reports a
// click.
func cardButton(c *ui.Context, icon *ui.SVG, label string, plain, iconOnly bool) bool {
	p := kit.Colors(c)
	b := ui.ButtonBase(c).Label(label).Height(22).Padding(0, 9).Radius(5).Row().Gap(5).AlignItems(ui.Center)
	if iconOnly && icon != nil {
		b.Tooltip(label).Padding(0, 6)
	}
	if plain {
		b.TextColor(p.Secondary).Padding(0, 5)
		if b.Hovered() {
			b.Background(p.Fill2)
		}
	} else {
		b.Background(p.Control).Border(0.5, ui.RGBA(0, 0, 0, 0.2)).Shadow(0, 1, 1.5, 0, ui.RGBA(0, 0, 0, 0.1))
		if b.Hovered() {
			b.Background(p.Control.Mix(ui.RGB(0, 0, 0), 0.03))
		}
	}
	b.Children(func() {
		if icon != nil {
			ui.Icon(c, icon).Size(12, 12)
		}
		if !iconOnly || icon == nil {
			ui.Text(c, label).FontSize(12).SingleLine()
		}
	})
	return b.Clicked()
}

// askAbout is the question Ask about this sends, citing the turn as Ask's
// citations do.
func askAbout(stamp string) string {
	return "What does [" + stamp + "] mean for me, and what should I do?"
}

// highlightRows lists highlights as the Summary does: each its glyph and
// kind, its polish's sentence or else the turn's words on one line, and
// its time, in time order; a tentative one muted, and the times of the
// highlights folded into one after its sentence. A debate has its sides
// under its topic, a line each, and the decision it led to, which ref
// finds. A click on one opens it.
func highlightRows(c *ui.Context, hs []store.Highlight, said func(seq int) (text string, atMs int64), open func(atMs int64),
	also map[string][]int64, ref func(id string) (stamp, sentence string, ok bool)) {
	p := kit.Colors(c)
	ui.Column(c).Radius(10).Border(0.5, ui.RGBA(0, 0, 0, 0.12)).Clip().AlignItems(ui.Stretch).Children(func() {
		for i, h := range hs {
			text, atMs := said(h.Seq)
			if s := h.Sentence(); s != "" {
				text = s
				if h.Polish.Debate != nil {
					text += " · " + string(h.Polish.Debate.Status)
				}
			}
			kind := h.ShownKind()
			look, color := kindLooks[kind], kindColor(c, kind)
			if i > 0 {
				ui.Box(c).Height(1).Background(p.Separator)
			}
			tentative := h.Polish != nil && h.Polish.Tentative()
			label := kind.Label() + ", " + clock(time.Duration(atMs)*time.Millisecond) + ": " + text
			if tentative {
				label += ", tentative"
			}
			label += sidesLabel(h)
			debate := h.Polish != nil && h.Polish.Debate != nil
			align := ui.Center
			if debate {
				align = ui.Start
			}
			row := ui.ButtonBase(c).Key(h.ID).Label(label).Row().Gap(8).AlignItems(align).Padding(8, 12)
			if row.Hovered() {
				row.Background(p.Fill)
			}
			if row.Clicked() {
				open(atMs)
			}
			row.Children(func() {
				ui.Icon(c, look.icon).Size(15, 15).TextColor(color).Margin(0, 7, 0, 0)
				ui.Text(c, kind.Label()).FontSize(12.5).FontWeight(600).TextColor(color).Width(120).SingleLine()
				if debate {
					t := cardTurn{ref: ref}
					ui.Column(c).Gap(4).Grow(1).Basis(0).MinWidth(0).AlignItems(ui.Stretch).Children(func() {
						body := ui.Text(c, text).FontSize(13).SingleLine()
						if tentative {
							body.TextColor(p.Secondary)
						}
						debateSides(c, *h.Polish, t, 1).FontSize(12.5)
						if ref, ok := refText(t, h.Polish.Debate.Decision); ok {
							ui.Text(c, "→ "+ref).FontSize(12).TextColor(p.Secondary).SingleLine()
						}
					})
				} else {
					body := ui.Text(c, text).FontSize(13).SingleLine().Grow(1).Basis(0).MinWidth(0)
					if tentative {
						body.TextColor(p.Secondary)
					}
				}
				if times := also[h.ID]; len(times) > 0 {
					var at []string
					for _, ms := range times {
						at = append(at, clock(time.Duration(ms)*time.Millisecond))
					}
					ui.Text(c, "also at "+strings.Join(at, ", ")).FontSize(11.5).TextColor(p.Tertiary).FontFeatures(kit.Tabular).SingleLine()
				}
				ui.Text(c, clock(time.Duration(atMs)*time.Millisecond)).FontSize(12).TextColor(p.Link).FontFeatures(kit.Tabular).
					Width(42).TextAlign(ui.End)
			})
		}
	})
}

// splitForYou is shown's highlights in time order, those for the user
// apart from the others, each as it shows; one folded into another is
// left out, its turn's time, by said, listed in also under the one it
// restates.
func splitForYou(shown map[int]store.Highlight, said func(seq int) (string, int64)) (forYou, others []store.Highlight, also map[string][]int64) {
	also = map[string][]int64{}
	all := slices.Collect(maps.Values(shown))
	for _, h := range shown {
		switch {
		case h.FoldedAmong(all):
			_, ms := said(h.Seq)
			also[h.Polish.FoldedInto] = append(also[h.Polish.FoldedInto], ms)
		case h.ShownKind().ForYou():
			forYou = append(forYou, h)
		default:
			others = append(others, h)
		}
	}
	bySeq := func(a, b store.Highlight) int { return a.Seq - b.Seq }
	slices.SortFunc(forYou, bySeq)
	slices.SortFunc(others, bySeq)
	for _, times := range also {
		slices.Sort(times)
	}
	return forYou, others, also
}

// summaryGroups are the kinds the Summary's Highlights lists apart once
// polish has sorted them, in order: what is for the user is above the
// recap.
var summaryGroups = []struct {
	title string
	kind  store.HighlightKind
}{
	{"Decisions", store.KindDecision}, {"Debates", store.KindDebate}, {"Actions", store.KindAction},
	{"Heads-ups", store.KindHeadsUp}, {"Announcements", store.KindAnnouncement},
}

// grouped is hs by summaryGroups, the groups with none left out; nil when
// none of hs is polished, which the Summary lists as one.
func grouped(hs []store.Highlight) (titles []string, groups [][]store.Highlight) {
	if !slices.ContainsFunc(hs, func(h store.Highlight) bool { return h.Polish != nil }) {
		return nil, nil
	}
	for _, g := range summaryGroups {
		var in []store.Highlight
		for _, h := range hs {
			if h.ShownKind() == g.kind {
				in = append(in, h)
			}
		}
		if len(in) > 0 {
			titles, groups = append(titles, g.title), append(groups, in)
		}
	}
	return titles, groups
}

// cardWidth is the card's: the design's 300, and room for its buttons'
// labels as macOS sets them.
const cardWidth = 320
