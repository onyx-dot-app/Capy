package views

import (
	"cmp"
	_ "embed"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/lang"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/views/addon"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

//go:embed icon.png
var appIconPNG []byte

// appIcon is the app's icon, as the welcome shows it.
var appIcon, _ = ui.DecodeBitmap(appIconPNG)

// contentWidth is the widest the summary and transcript set their lines:
// about 70 characters.
const contentWidth = 640

// content is the window's right side: the toolbar over the session shown,
// Recently Deleted or Settings, or the welcome or empty state when there
// is none.
func (w *mainWindow) content(c *ui.Context) {
	p := kit.Colors(c)
	d := w.shown()
	background := p.Window
	if w.settings != nil {
		background = p.Settings
	}
	pane := ui.Column(c).Fill().Background(background).AlignItems(ui.Stretch)
	w.paneWidth = pane.Bounds().W
	pane.Children(func() {
		w.toolbar(c, d)
		switch {
		case w.settings != nil:
			w.settings.view(c)
		case w.trashOpen:
			w.trashPage(c)
		case w.batch != "":
			w.batchPage(c)
		case w.file != "":
			w.filePage(c)
		case d != nil && w.app.sessionLive(d.ID):
			w.liveView(c, d)
		case d != nil:
			w.sessionView(c, d)
		case w.loaded && len(w.sessions) == 0 && len(w.batches) == 0 && w.searchQuery() == "" && w.keyState == keyNone:
			w.welcome(c)
		case w.loaded && len(w.sessions) == 0 && len(w.batches) == 0 && w.searchQuery() == "":
			w.ready(c)
		case w.loaded && w.selected == "":
			ui.Column(c).Grow(1).Center().Children(func() {
				ui.Text(c, "Select a session").TextColor(p.Secondary)
			})
		}
	})
	w.shortcuts(c, d)
	w.purgeAlert(c)
	w.batchSheets(c)
}

// centered is a column of contentWidth at most, centered in the pane.
func centered(c *ui.Context) ui.Element { return centeredIn(c, contentWidth) }

// centeredIn is a column of width at most, centered in the pane.
func centeredIn(c *ui.Context, width float32) ui.Element {
	return ui.Column(c).WidthPercent(100).MaxWidth(width).Margin(0, ui.Auto).AlignItems(ui.Stretch)
}

// sidePadding is the padding left and right of a session's page.
const sidePadding = 40

// margin is the width a transcript keeps right of its lines for the
// highlights shown: a note's where the pane has room for the lines and
// the notes, else a glyph's; none for no highlight.
func (w *mainWindow) margin(shown map[int]store.Highlight) float32 {
	switch {
	case len(shown) == 0:
		return 0
	case w.paneWidth-2*sidePadding >= stampColumn+minInsightText+insightColumn:
		return insightColumn
	}
	return glyphColumn
}

// welcome greets a first launch, without an API key yet.
func (w *mainWindow) welcome(c *ui.Context) {
	p := kit.Colors(c)
	ui.Column(c).Grow(1).Center().Children(func() {
		ui.Column(c).MaxWidth(380).AlignItems(ui.Center).Margin(0, 0, 46, 0).Children(func() {
			ui.Image(c, appIcon).Size(96, 96)
			ui.Text(c, "Welcome to Capy").FontSize(22).Bold().Margin(20, 0, 8, 0)
			ui.Text(c, "Live, translated captions of anything you hear, saved with an AI recap.").
				FontSize(15).TextColor(p.Secondary).TextAlign(ui.Center).LineHeight(1.4)
			if ui.PrimaryButton(c, "Add API Key…").Height(32).Padding(0, 18).FontWeight(500).Margin(24, 0, 12, 0).Clicked() {
				w.app.OpenSettings()
			}
			ui.Link(c, "Get a key", apiKeyHelp).FontWeight(500).TextColor(p.Link)
			ui.Text(c, "Capy uses Zoom AI Services. You need a key with Scribe, Translator and Summarizer access.").
				FontSize(12).TextColor(p.Tertiary).TextAlign(ui.Center).Margin(28, 0, 0, 0)
		})
	})
}

// ready is the window before the first session: Record, large, what it
// will listen to, and the other way in, Import.
func (w *mainWindow) ready(c *ui.Context) {
	p := kit.Colors(c)
	prefs := w.app.prefs
	ui.Column(c).Grow(1).Center().Padding(0, 0, 64, 0).Children(func() {
		ui.Column(c).MaxWidth(380).AlignItems(ui.Center).Children(func() {
			if recordButton(c, w.app, recordLarge) {
				w.record()
			}
			ui.Text(c, "or press "+shortcutText).TextColor(p.Secondary).Margin(12, 0, 0, 0)
			setup := ui.ButtonBase(c).Label("Caption settings").Height(26).Padding(0, 8).Radius(6).Gap(6).
				Row().AlignItems(ui.Center).Background(p.Fill).TextColor(p.Secondary).Margin(22, 0, 0, 0)
			if setup.Hovered() {
				setup.Background(p.Fill2)
			}
			if setup.Clicked() {
				w.app.OpenSettings()
			}
			setup.Children(func() {
				ui.Icon(c, sourceIcon(prefs.CaptionsSource)).Size(14, 14)
				ui.Text(c, sourceName(prefs.CaptionsSource)+" · "+captionLangs(prefs.CaptionsFrom, prefs.CaptionsTo, prefs.CaptionsTranslate)).SingleLine()
				ui.Icon(c, kit.ChevronRightIcon).Size(11, 11)
			})
			ui.Box(c).Size(220, 1).Background(p.Separator).Margin(32, 0, 22, 0)
			ui.Text(c, "Have recordings already?").FontWeight(500).Margin(0, 0, 10, 0)
			if quietButton(c, kit.ImportIcon, "Import Audio Files…") {
				w.chooseFiles()
			}
			ui.Text(c, "or drop them anywhere in this window. Each file gets a transcript and a recap.").FontSize(12).
				LineHeight(16.0/12).TextColor(p.Secondary).TextAlign(ui.Center).Margin(10, 0, 0, 0)
			ui.Text(c, "Capy keeps the transcript, never the audio.").FontSize(12).TextColor(p.Tertiary).
				TextAlign(ui.Center).Margin(28, 0, 0, 0)
		})
	})
}

// quietButton is a button of 28 with a white face, an icon before its
// label, as macOS's push buttons: the empty window's Import Audio Files….
func quietButton(c *ui.Context, icon *ui.SVG, label string) bool {
	p := kit.Colors(c)
	b := ui.ButtonBase(c).Label(label).Height(28).Padding(0, 14).Radius(6).Gap(6).Row().AlignItems(ui.Center).
		Background(p.Control).Border(0.5, ui.RGBA(0, 0, 0, 0.2)).Shadow(0, 1, 1.5, 0, ui.RGBA(0, 0, 0, 0.1))
	if b.Hovered() {
		b.Background(p.Control.Mix(ui.RGB(0, 0, 0), 0.03))
	}
	b.Children(func() {
		ui.Icon(c, icon).Size(14, 14)
		ui.Text(c, label).SingleLine()
	})
	return b.Clicked()
}

func sourceIcon(s store.Source) *ui.SVG {
	switch s {
	case store.SourceSystem:
		return kit.LaptopIcon
	case store.SourceFile:
		return kit.WaveformIcon
	}
	return kit.MicIcon
}

func sourceName(s store.Source) string {
	switch s {
	case store.SourceSystem:
		return "This Mac"
	case store.SourceFile:
		return "A file"
	}
	return "Microphone"
}

// header is the session's title, when it started and how long it ran,
// then its highlights' count, or Find Highlights where it has none yet,
// the Info button opening the rest and, once stopped, the session's
// actions. dot leads it while recording.
func (w *mainWindow) header(c *ui.Context, title, started, length string, d *store.SessionDetail, live bool) {
	p := kit.Colors(c)
	ui.Text(c, title).FontSize(26).Bold().Selectable()
	ui.Row(c).Gap(6).AlignItems(ui.Center).Margin(6, 0, 0, 0).TextColor(p.Secondary).Wrap().Children(func() {
		if live {
			kit.LiveDot(c, kit.LiveRed)
		}
		ui.Text(c, started).SingleLine()
		if length != "" {
			ui.Text(c, "·").TextColor(p.Tertiary)
			ui.Text(c, length).FontFeatures(kit.Tabular).SingleLine()
		}
		ui.Row(c).Gap(2).AlignItems(ui.Center).Margin(0, 0, 0, 2).Children(func() {
			if shown := shownHighlights(d.Highlights); len(shown) > 0 {
				if highlightCount(c, shown) && !live {
					w.tab, w.toHighlights = tabSummary, true
				}
			} else if !live && w.app.highlightsOn() && !d.EndedAt.IsZero() && turnCount(d) > 0 &&
				metaButton(c, sparkIcon, "Find Highlights").Clicked() && w.busy == "" {
				w.findHighlights(d.ID)
			}
			w.infoButton(c, d, length, live)
			if live {
				return
			}
			if metaButton(c, kit.CopyIcon, "Copy as Markdown").Clicked() {
				w.copyMarkdown(c, d)
			}
			if metaButton(c, kit.ShareIcon, "Export…").Clicked() {
				w.export(*d)
			}
			if !d.EndedAt.IsZero() && metaButton(c, kit.TrashIcon, "Delete").Clicked() {
				w.delete(d.ID)
			}
		})
	})
}

// metaButton is an icon button of the meta line, quieter than a
// toolbar's.
func metaButton(c *ui.Context, icon *ui.SVG, label string) ui.Element {
	p := kit.Colors(c)
	b := ui.ButtonBase(c).Label(label).Tooltip(label).Size(22, 22).Radius(5).Center().TextColor(p.Tertiary)
	if b.Hovered() {
		b.Background(p.Fill2).TextColor(p.Secondary)
	}
	b.Children(func() { ui.Icon(c, icon).Size(15, 15) })
	return b
}

// infoButton opens Session Info, as ⌘I does.
func (w *mainWindow) infoButton(c *ui.Context, d *store.SessionDetail, length string, live bool) {
	w.infoPopover(c, "Session Info", func() { sessionInfo(c, d, length, live, w.highlightsProblem(live)) })
}

// highlightsProblem is why Highlights stopped for the recording, as
// Session Info tells it; "" while it works, or for a session not live.
func (w *mainWindow) highlightsProblem(live bool) string {
	if !live || w.app.captions == nil {
		return ""
	}
	switch err := w.app.captions.snapshot().HighlightsErr; {
	case err == nil:
		return ""
	case errors.Is(err, zoomai.ErrUnauthorized):
		return "Your API key can’t use Zoom Decisions."
	}
	return "Stopped: Zoom Decisions kept failing."
}

// infoPopover is the Info button of a meta line, opening content in a
// popover under it.
func (w *mainWindow) infoPopover(c *ui.Context, label string, content func()) {
	p := kit.Colors(c)
	b := metaButton(c, kit.InfoIcon, label)
	if b.Clicked() {
		w.info = !w.info
	}
	if w.info {
		b.Background(p.Fill2).TextColor(p.Secondary)
	}
	ui.PopoverBase(c, b, &w.info, func(panel ui.Element) {
		panel.AttachTo(b, ui.AnchorBottom, ui.AnchorTop).Margin(8, 0, 0, 0).Width(320).Padding(12, 14, 11).
			Radius(10).Background(p.Popover).Border(1, p.Separator).Shadow(0, 10, 30, 0, ui.RGBA(0, 0, 0, 0.18)).
			AlignItems(ui.Stretch)
		content()
	})
}

// sessionInfo is what the header leaves out: the full start, the source,
// the languages, the transcript's length and, for a file, its tracking ID.
func sessionInfo(c *ui.Context, d *store.SessionDetail, length string, live bool, highlights string) {
	rows := [][2]string{{"Started", d.StartedAt.Local().Format("Monday, Jan 2, 2006 at 15:04")}}
	if length != "" {
		if live {
			length += " so far"
		}
		rows = append(rows, [2]string{"Length", length})
	}
	from := sourceName(d.Source)
	if d.Source == store.SourceFile {
		from = d.FileName
	}
	rows = append(rows, [2]string{"Audio from", from})
	codes := session.SpokenLanguages(*d)
	var spoken []string
	for _, code := range codes {
		spoken = append(spoken, langName(code))
	}
	switch {
	case d.Mode == store.ModeConversation:
		rows = append(rows, [2]string{"Languages", langPair(d.SessionSummary, false)})
	case !d.Translated || len(codes) == 1 && codes[0] == d.LangB: // nothing was translated
		rows = append(rows, [2]string{"Spoken", strings.Join(spoken, ", ")})
	default:
		rows = append(rows, [2]string{"Spoken", strings.Join(spoken, ", ")}, [2]string{"Translated to", langName(d.LangB)})
	}
	if !live {
		lines := strconv.Itoa(turnCount(d)) + " lines"
		if lines == "1 lines" {
			lines = "1 line"
		}
		rows = append(rows, [2]string{"Transcript", lines})
	}
	if d.TrackingID != "" { // for Zoom to find the file's request in its logs
		rows = append(rows, [2]string{"Tracking ID", d.TrackingID})
	}
	if highlights != "" {
		rows = append(rows, [2]string{"Highlights", highlights})
	}
	infoRows(c, rows)
}

// infoRows are an info popover's rows, label and value, over the line
// that Capy keeps no audio. A value wraps within the popover, breaking a
// word longer than its line, as a tracking ID is.
func infoRows(c *ui.Context, rows [][2]string) {
	p := kit.Colors(c)
	ui.Column(c).Gap(6).Selectable().AlignItems(ui.Stretch).Children(func() {
		for _, r := range rows {
			ui.Row(c).AlignItems(ui.Start).Children(func() {
				ui.Text(c, r[0]).FontSize(12).TextColor(p.Secondary).Width(88)
				ui.Text(c, r[1]).FontSize(12).FontFeatures(kit.Tabular).Grow(1).Basis(0).MinWidth(0)
			})
		}
	})
	ui.Box(c).Height(1).Background(p.Separator).Margin(10, 0, 9, 0)
	ui.Text(c, "Capy keeps the transcript, never the audio.").FontSize(11).TextColor(p.Secondary)
}

// turnCount is the transcript's number of turns: its segments but the
// gaps where the connection dropped.
func turnCount(d *store.SessionDetail) int {
	n := 0
	for _, seg := range d.Segments {
		if seg.Lang != store.GapLang {
			n++
		}
	}
	return n
}

// fileStartedText is when file f of batch b was recorded, "Recorded Sep 26
// at 15:40", from its metadata; else when it was imported.
func fileStartedText(b store.Batch, f store.BatchFile, now time.Time) string {
	if f.RecordedAt.IsZero() {
		return "Imported " + dayText(b.CreatedAt, now)
	}
	return "Recorded " + dayText(f.RecordedAt, now)
}

// dayText is when t was, after a verb: "today at 09:58", "yesterday at
// 16:05", else "Sep 26 at 15:40", with the year when it is not this one.
func dayText(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	at := " at " + t.Format("15:04")
	if day, older := dayGroup(t, now); !older {
		return strings.ToLower(day) + at
	}
	if t.Year() != now.Year() {
		return t.Format("Jan 2, 2006") + at
	}
	return t.Format("Jan 2") + at
}

// startedText is when a session started, in words: "Today at 09:30",
// "Yesterday at 16:05", else "Monday, Oct 5 at 20:12", with the year
// when it is not this one.
func startedText(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	at := " at " + t.Format("15:04")
	if day, older := dayGroup(t, now); !older {
		return day + at
	}
	if t.Year() != now.Year() {
		return t.Format("Monday, Jan 2, 2006") + at
	}
	return t.Format("Monday, Jan 2") + at
}

// sessionView is a stopped session: its header, Summary and Transcript.
func (w *mainWindow) sessionView(c *ui.Context, d *store.SessionDetail) {
	p := kit.Colors(c)
	width := float32(contentWidth)
	if w.tab == tabTranscript && w.margin(shownHighlights(d.Highlights)) == insightColumn {
		width += insightColumn
	}
	ui.Scroll(c).Key(d.ID+strconv.Itoa(w.tab)).Grow(1).Padding(12, sidePadding, 40).Children(func() {
		centeredIn(c, width).Children(func() {
			title := d.Title
			if title == "" {
				title = untitled(d.SessionSummary)
			}
			length := ""
			if !d.EndedAt.IsZero() {
				length = lengthText(d.EndedAt.Sub(d.StartedAt))
			}
			started := startedText(d.StartedAt, c.Now())
			if b, f, ok := fileOfSession(w.batches, d.ID); ok {
				w.crumb(c, b.Batch)
				started = fileStartedText(b.Batch, f, c.Now())
			}
			w.header(c, title, started, length, d, false)
			if w.notice != "" {
				ui.Text(c, w.notice).TextColor(p.Secondary).Margin(10, 0, 0, 0)
			}
			ui.Row(c).Margin(20, 0, 0, 0).Children(func() {
				segmented(c, &w.tab, "View", false, segment{label: "Summary"}, segment{label: "Transcript"})
			})
			if w.tab == tabTranscript {
				w.transcript(c, d)
				return
			}
			w.summary(c, d)
		})
	})
}

// summary is what was for the user, the recap as a document, or how it
// is coming along, then the other highlights.
func (w *mainWindow) summary(c *ui.Context, d *store.SessionDetail) {
	p := kit.Colors(c)
	said := func(seq int) (string, int64) {
		for _, seg := range d.Segments {
			if seg.Seq == seq {
				return seg.Text, seg.StartMs
			}
		}
		return "", 0
	}
	forYou, others, also := splitForYou(shownHighlights(d.Highlights), said)
	refs := refsOf(d.Highlights, func(seq int) string { return segmentStamp(d.Segments, seq) })
	open := func(atMs int64) { w.tab, w.jump = tabTranscript, jump{session: d.ID, atMs: atMs} }
	section := func(title string) ui.Element {
		return ui.Text(c, title).FontSize(15).Bold().Margin(0, 0, 8, 0)
	}
	if len(forYou) > 0 {
		ui.Column(c).Margin(28, 0, 0, 0).AlignItems(ui.Stretch).Children(func() {
			section("For you")
			highlightRows(c, forYou, said, open, also, refs)
		})
	}
	ui.Column(c).Margin(28, 0, 0, 0).AlignItems(ui.Stretch).Children(func() {
		if len(forYou) > 0 || len(others) > 0 {
			section("Recap")
		}
		switch {
		case recapFailed(d):
			ui.Row(c).Gap(10).AlignItems(ui.Start).Children(func() {
				ui.Icon(c, kit.WarnIcon).Size(16, 16).TextColor(p.Secondary).Margin(1, 0, 0, 0)
				ui.Column(c).Grow(1).Basis(0).Gap(2).Children(func() {
					ui.Text(c, "The recap couldn’t be written.")
					ui.Text(c, "Your transcript is saved.").FontSize(12).TextColor(p.Secondary)
				})
				retry := ui.Button(c, "").Label("Retry").Height(24).Padding(0, 12).Gap(6).Disabled(w.busy != "")
				if retry.Clicked() {
					w.retry("recap", d.ID)
				}
				retry.Children(func() {
					ui.Icon(c, kit.RetryIcon).Size(13, 13)
					ui.Text(c, "Retry").SingleLine()
				})
			})
		case d.RecapStatus == store.RecapPending:
			ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
				ui.Spinner(c).Size(14, 14)
				ui.Text(c, "Writing the recap…").TextColor(p.Secondary)
			})
			ui.RichText(c).FontSize(12).TextColor(p.Secondary).Margin(6, 0, 0, 24).Children(func() {
				ui.Text(c, "The transcript is ready in the meantime. ")
				if ui.Link(c, "Show Transcript", "").TextColor(p.Link).Clicked() {
					w.tab = tabTranscript
				}
			})
		case d.Recap == "":
			ui.Text(c, "Nothing was said in this session.").TextColor(p.Secondary)
		case d.Source == store.SourceFile:
			markdownText(c, d.Recap, &w.recap) // titled by its file: its first heading is no title
		default:
			markdownText(c, recapBody(d.Recap), &w.recap)
		}
	})
	if len(others) > 0 {
		ui.Column(c).Margin(28, 0, 0, 0).AlignItems(ui.Stretch).Children(func() {
			title := section("Highlights")
			if w.toHighlights {
				title.ScrollIntoView()
			}
			titles, groups := grouped(others)
			if groups == nil {
				highlightRows(c, others, said, open, also, refs)
				return
			}
			for i, g := range groups {
				ui.Text(c, titles[i]).FontSize(12).FontWeight(600).TextColor(p.Secondary).Margin(float32(min(i, 1)*10), 0, 6, 2)
				highlightRows(c, g, said, open, also, refs)
			}
		})
	}
	w.toHighlights = false
}

// recapBody is the recap without its first line when that is a heading:
// the session's title, shown above it.
func recapBody(recap string) string {
	first, rest, _ := strings.Cut(strings.TrimLeft(recap, "\n"), "\n")
	if strings.HasPrefix(first, "#") {
		return rest
	}
	return recap
}

// stampColumn is the width of the transcript's time column: room for an
// hour's time, "1:07:10", clear of a highlight's rule, markInset left of
// the text.
const stampColumn = 68

// transcript is every turn of a stopped session at its time, with its
// translation.
func (w *mainWindow) transcript(c *ui.Context, d *store.SessionDetail) {
	p := kit.Colors(c)
	ended := !d.EndedAt.IsZero()
	named := session.ManySpeakers(d.Segments, session.SegmentSpeaker)
	target := ""
	if w.jump.session == d.ID {
		target = turnAt(d.Segments, w.jump.atMs)
	}
	shown := shownHighlights(d.Highlights)
	margin := w.margin(shown)
	at := func(seq int) string { return segmentStamp(d.Segments, seq) }
	refs := refsOf(d.Highlights, at)
	jumpTo := func(seq int) {
		for _, seg := range d.Segments {
			if seg.Seq == seq {
				w.jump = jump{session: d.ID, atMs: seg.StartMs}
				return
			}
		}
	}
	ui.Column(c).Margin(28, 0, 0, 0).Gap(18).Selectable().AlignItems(ui.Stretch).Children(func() {
		for i, seg := range d.Segments {
			if seg.Lang == store.GapLang {
				connectionLost(c).Key(seg.ID)
				continue
			}
			untranslated := ended && session.Untranslated(seg)
			speaker := speakerChange(d.Segments, i, named, session.SegmentSpeaker)
			stamp := clock(time.Duration(seg.StartMs) * time.Millisecond)
			m := turnMark{margin: margin}
			if h, ok := shown[seg.Seq]; ok {
				t := cardTurn{speaker: seg.Speaker, stamp: stamp, said: seg.Text, ref: refs, at: at, jump: jumpTo}
				m.kind, m.note = h.ShownKind(), func() { w.note(c, h, margin, t, d.SessionSummary, false) }
				m.under = w.under(c, h, margin, t, d.SessionSummary, false)
			}
			row := markedTurn(c, seg.ID, stamp, speaker, seg.Text, m, func() {
				switch {
				case seg.Translation != "":
					ui.Text(c, seg.Translation).FontSize(15).LineHeight(1.45).TextColor(p.Link)
				case untranslated:
					ui.Row(c).Gap(10).Children(func() {
						ui.Text(c, "Not translated").FontSize(13).TextColor(p.Tertiary)
						if ui.Link(c, "Retry Translations", "").FontSize(13).FontWeight(500).TextColor(p.Link).
							Clicked() && w.busy == "" {
							w.retry("translations", d.ID)
						}
					})
				}
			})
			if seg.ID == target {
				w.highlight(c, row)
			}
		}
	})
}

// segmentStamp is the time of turn seq of segs, "" for none.
func segmentStamp(segs []store.Segment, seq int) string {
	for _, seg := range segs {
		if seg.Seq == seq {
			return clock(time.Duration(seg.StartMs) * time.Millisecond)
		}
	}
	return ""
}

// highlight brings the turn jumped to into view as it first shows, and
// highlights it for highlightTime.
func (w *mainWindow) highlight(c *ui.Context, row ui.Element) {
	if w.jump.since.IsZero() {
		w.jump.since = c.Now()
		row.ScrollIntoView()
	}
	left := highlightTime - c.Now().Sub(w.jump.since)
	if left <= 0 {
		w.jump = jump{}
		return
	}
	c.After(left)
	// The margin takes back the padding: the turn stays where it is.
	row.Background(kit.Colors(c).Highlight).Radius(6).Padding(4, 8).Margin(-4, -8)
}

// turnAt is the turn said at atMs: the last that started in the same
// second or before, as its time reads, else the first.
func turnAt(segs []store.Segment, atMs int64) string {
	id := ""
	for _, seg := range segs {
		if seg.Lang == store.GapLang {
			continue
		}
		if id == "" || seg.StartMs/1000 <= atMs/1000 {
			id = seg.ID
		}
		if seg.StartMs/1000 > atMs/1000 {
			break
		}
	}
	return id
}

// turn is a line of a transcript: its time, who said it when they did not
// say the line before, what was said, and under it what below builds, its
// translation.
func turn(c *ui.Context, key any, stamp, speaker, said string, below func()) ui.Element {
	return markedTurn(c, key, stamp, speaker, said, turnMark{}, below)
}

// markedTurn is turn with m's highlight: the kind's rule and wash, and
// its note in the margin m keeps, which every turn of a transcript with a
// highlight keeps, so that their lines wrap alike.
func markedTurn(c *ui.Context, key any, stamp, speaker, said string, m turnMark, below func()) ui.Element {
	p := kit.Colors(c)
	return ui.Row(c).Key(key).AlignItems(ui.Start).Children(func() {
		ui.Text(c, stamp).FontSize(12).TextColor(p.Secondary).FontFeatures(kit.Tabular).Width(stampColumn).Margin(3, 0, 0, 0)
		body := ui.Column(c).Grow(1).Basis(0).Gap(2)
		if m.kind != "" {
			markTurn(c, body, m.kind, cmp.Or(m.drawn, 1))
		}
		body.Children(func() {
			if speaker != "" {
				ui.Text(c, speaker).FontSize(12).FontWeight(600).TextColor(p.Secondary).Margin(2, 0, 2, 0)
			}
			if said != "" {
				ui.Text(c, said).FontSize(15).LineHeight(1.45)
			}
			below()
			if m.under != nil { // a narrow pane's card, expanded under the turn, never over it
				ui.Box(c).Height(6)
				m.under()
			}
		})
		if m.margin > 0 {
			col := ui.Column(c).Width(m.margin).AlignItems(ui.Stretch)
			if m.margin == insightColumn {
				col.Padding(0, 0, 0, 12)
			} else {
				col.Padding(1, 0, 0, 4)
			}
			col.Children(func() {
				if m.note != nil {
					m.note()
				}
			})
		}
	})
}

// note is highlight h in a transcript's margin: its card in the insight
// column, or its glyph where the pane is narrow, a click expanding its card
// under the turn.
func (w *mainWindow) note(c *ui.Context, h store.Highlight, margin float32, t cardTurn, s store.SessionSummary, live bool) {
	toggle := func() {
		if w.insight == h.ID {
			w.insight = ""
		} else {
			w.insight = h.ID
		}
	}
	if margin == insightColumn {
		if insightCard(c, h, t, w.insight == h.ID, w.cardActions(c, h, t, s, live), func() { w.insight = "" }) {
			toggle()
		}
		return
	}
	if noteGlyph(c, h.ShownKind()).Clicked() {
		toggle()
	}
}

// under is highlight h's card under its turn, expanded, where the pane is
// too narrow for the insight column; nil when it is not expanded.
func (w *mainWindow) under(c *ui.Context, h store.Highlight, margin float32, t cardTurn, s store.SessionSummary, live bool) func() {
	if margin != glyphColumn || w.insight != h.ID {
		return nil
	}
	return func() {
		if insightCard(c, h, t, true, w.cardActions(c, h, t, s, live), func() { w.insight = "" }) {
			w.insight = ""
		}
	}
}

// cardActions are what highlight h's card does: Copy its insight, Not
// important, and Ask about this where Ask can answer.
func (w *mainWindow) cardActions(c *ui.Context, h store.Highlight, t cardTurn, s store.SessionSummary, live bool) cardActions {
	acts := cardActions{
		copy: func() {
			c.WriteClipboard(insightText(h, t))
			c.Toast("Copied")
		},
		dismiss: func() { w.dismiss(s.ID, h.ID, live) },
	}
	if w.app.ext.CanAsk() {
		acts.ask = func() { w.app.ext.Ask(addon.Session{ID: s.ID, Title: s.Title, Live: live}, askAbout(t.stamp), false) }
	}
	return acts
}

// speakerChange is who said turns[i] when the session names more than one
// speaker and someone else said the turn before it, else "". A gap names
// none, so the speaker shows again after it: Scribe numbers them anew.
func speakerChange[T any](turns []T, i int, named bool, speaker func(T) string) string {
	who := speaker(turns[i])
	if !named || who == "" || i > 0 && speaker(turns[i-1]) == who {
		return ""
	}
	return who
}

// lineSpeaker is who said l, for speakerChange.
func lineSpeaker(l session.Line) string { return l.Speaker }

// connectionLost divides the turns where the connection dropped.
func connectionLost(c *ui.Context) ui.Element {
	p := kit.Colors(c)
	return ui.Row(c).Gap(10).AlignItems(ui.Center).Margin(2, 0, 2, stampColumn).Children(func() {
		ui.Box(c).Height(1).Grow(1).Background(p.Separator)
		ui.Text(c, "Connection lost").FontSize(11).FontWeight(500).TextColor(p.Secondary)
		ui.Box(c).Height(1).Grow(1).Background(p.Separator)
	})
}

// liveView is the session being recorded: its lines as the caption bar
// shows them, the latest in view until the user scrolls up.
func (w *mainWindow) liveView(c *ui.Context, d *store.SessionDetail) {
	p := kit.Colors(c)
	b := w.app.captions
	snap := b.snapshot()
	page := ui.Column(c).Grow(1).Padding(12, 40, 0).AlignItems(ui.Stretch)
	page.Children(func() {
		centered(c).Children(func() {
			elapsed := ""
			if !snap.StartedAt.IsZero() {
				since := c.Now().Sub(snap.StartedAt)
				c.After(time.Second - since%time.Second)
				elapsed = lengthText(since)
			}
			info := *d
			info.SessionSummary = b.live(d.SessionSummary)
			info.Highlights = snap.Highlights
			w.header(c, "Recording…", "Started "+d.StartedAt.Local().Format("15:04"), elapsed, &info, true)
			if b.canSwitch() {
				ui.Row(c).Wrap().GapX(24).GapY(8).AlignItems(ui.Center).Margin(16, 0, 0, 0).Children(func() {
					ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
						ui.Text(c, "Spoken").TextColor(p.Secondary)
						if code := b.cfg.LangA; langSelect(c, &code, lang.Spoken(), "Spoken language") {
							b.setSpoken(code)
						}
					})
					ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
						on := b.cfg.Translate
						label := "Translate"
						if on {
							label = "Translate to"
						}
						if ui.Checkbox(c, &on, label).Changed() {
							w.app.SetTranslate(on)
						}
						if b.cfg.Translate {
							if code := b.cfg.LangB; langSelect(c, &code, lang.Targets(), "Translation language") {
								w.app.setTarget(code)
							}
						}
					})
				})
			}
			if b.minimized {
				ui.Row(c).Gap(16).AlignItems(ui.Center).Margin(20, 0, 0, 0).Children(func() {
					show := ui.Button(c, "").Label("Show Caption Bar").Height(26).Padding(0, 12).Gap(6)
					if show.Clicked() {
						b.expand()
					}
					show.Children(func() {
						ui.Icon(c, kit.CaptionsIcon).Size(15, 15)
						ui.Text(c, "Show Caption Bar").SingleLine()
					})
					ui.Text(c, "The caption bar is minimized. Lines also appear here as they’re recognized.").
						FontSize(12).TextColor(p.Secondary).Grow(1).Basis(0)
				})
			}
		})
		lines := snap.Lines
		n := len(lines)
		if snap.Draft != "" {
			n++
		}
		named := session.ManySpeakers(lines, lineSpeaker)
		shown := shownHighlights(snap.Highlights)
		margin := w.margin(shown)
		at := func(seq int) string { return stampOf(lines, seq) }
		refs := refsOf(snap.Highlights, at)
		width := float32(contentWidth)
		if margin == insightColumn {
			width += insightColumn
		}
		w.live.FollowEnd = true
		w.live.Key = func(i int) any {
			if i < len(lines) {
				return lines[i].Seq
			}
			return "draft"
		}
		list := ui.List(c, &w.live, n, func(i int) {
			centeredIn(c, width).Padding(0, 0, 18).Children(func() {
				if i == len(lines) {
					ui.Row(c).Gap(8).AlignItems(ui.End).Margin(0, 0, 0, stampColumn).Children(func() {
						ui.Text(c, snap.Draft).FontSize(15).Italic().TextColor(p.Secondary)
						kit.WorkingDots(c, p.Tertiary).Margin(0, 0, 7, 0)
					})
					return
				}
				m := turnMark{margin: margin}
				if h, ok := shown[lines[i].Seq]; ok {
					t := cardTurn{speaker: lines[i].Speaker, stamp: clock(time.Duration(lines[i].StartMs) * time.Millisecond),
						said: lines[i].Text, ref: refs, at: at, jump: func(seq int) { w.jumpTo(c, lines, seq) }}
					m.kind, m.drawn, m.note = h.ShownKind(), drawn(c, h), func() { w.note(c, h, margin, t, d.SessionSummary, true) }
					m.under = w.under(c, h, margin, t, d.SessionSummary, true)
				}
				row := w.liveLine(c, lines[i], speakerChange(lines, i, named, lineSpeaker), m)
				if w.flash.seq == lines[i].Seq && !w.flash.since.IsZero() {
					w.flashRow(c, row)
				}
			})
		}).Grow(1).Margin(28, 0, 0, 0)
		w.liveAttention(c, b, snap, list.Bounds().Y-page.Bounds().Y)
	})
}

// flash is a line of the recording jumped to, highlighted for a moment.
type flash struct {
	seq   int
	since time.Time
}

// flashRow highlights the line jumped to for highlightTime.
func (w *mainWindow) flashRow(c *ui.Context, row ui.Element) {
	left := highlightTime - c.Now().Sub(w.flash.since)
	if left <= 0 {
		w.flash = flash{}
		return
	}
	c.After(left)
	row.Background(kit.Colors(c).Highlight).Radius(6).Padding(4, 8).Margin(-4, -8)
}

// jumpTo scrolls the recording's lines to turn seq and flashes it.
func (w *mainWindow) jumpTo(c *ui.Context, lines []session.Line, seq int) {
	if i := lineOf(lines, seq); i >= 0 {
		w.live.ScrollTo(i, ui.Center)
		w.flash = flash{seq: seq, since: c.Now()}
	}
}

// liveAttention points at what was highlighted out of view: the edge
// pill, top lines down the page, for a highlight for the user whose line
// is above or below, and, while the user is scrolled up, what was
// highlighted since.
func (w *mainWindow) liveAttention(c *ui.Context, b *captionBar, snap session.Snapshot, top float32) {
	b.att.follow(c.Now(), snap, w.app.prefs.HighlightNudge)
	first, last := w.live.Visible()
	if w.live.AtEnd() {
		w.awaySince = time.Time{}
	} else if w.awaySince.IsZero() {
		w.awaySince = c.Now()
	}
	if h, ok := highlightOf(snap.Highlights, b.att.edge); ok && !h.Dismissed {
		i := lineOf(snap.Lines, h.Seq)
		if i < 0 || i >= first && i <= last {
			b.att.edge = "" // its line is in view
		} else {
			ui.Row(c).Absolute().Top(top + 8).Left(0).Right(0).Justify(ui.Center).Children(func() {
				ago := c.Now().Sub(b.att.edgeAt)
				c.After(time.Minute - ago%time.Minute)
				opened, closed := edgePill(c, h, snap.Lines[i].Speaker, ago, i < first)
				if opened {
					w.jumpTo(c, snap.Lines, h.Seq)
				}
				if opened || closed {
					b.att.edge = ""
				}
			})
		}
	}
	if w.awaySince.IsZero() {
		return
	}
	var since []store.Highlight
	for _, h := range snap.Highlights {
		if !h.Dismissed && h.CreatedAt.After(w.awaySince) {
			since = append(since, h)
		}
	}
	if len(since) == 0 {
		return
	}
	ui.Row(c).Absolute().Bottom(12).Left(stampColumn).Right(24).Children(func() {
		h, picked, latest := awayRibbon(c, since)
		switch {
		case picked:
			w.jumpTo(c, snap.Lines, h.Seq)
		case latest:
			w.live.ScrollToEnd()
		}
	})
}

func (w *mainWindow) liveLine(c *ui.Context, l session.Line, speaker string, m turnMark) ui.Element {
	p := kit.Colors(c)
	if l.Gap {
		return connectionLost(c)
	}
	return markedTurn(c, l.Seq, clock(time.Duration(l.StartMs)*time.Millisecond), speaker, l.Text, m, func() {
		switch {
		case l.Translation != "":
			ui.Text(c, l.Translation).FontSize(15).LineHeight(1.45).TextColor(p.Link)
		case l.Translating:
			kit.WorkingDots(c, p.Link).Margin(8, 0, 4, 0)
		}
	})
}

// langPair is "English → 中文 (简体)", "English" for captions without
// translation, or "中文 ⇄ English" for a conversation; short drops what
// is in parentheses.
func langPair(s store.SessionSummary, short bool) string {
	name := func(code string) string {
		n := langName(code)
		if short {
			n, _, _ = strings.Cut(n, " (")
		}
		return n
	}
	switch {
	case s.Mode == store.ModeConversation:
		return name(s.LangA) + " ⇄ " + name(s.LangB)
	case !s.Translated || s.LangA == s.LangB:
		return name(s.LangA)
	}
	return name(s.LangA) + " → " + name(s.LangB)
}
