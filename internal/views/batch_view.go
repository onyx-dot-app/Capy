package views

import (
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
)

// shownBatch is the batch whose page shows: with its recaps once loaded,
// as the sidebar lists it meanwhile.
func (w *mainWindow) shownBatch() (store.BatchSummary, bool) {
	if d := w.batchDetail; d != nil && d.ID == w.batch {
		// The files' sessions as listed last: a file saved since the
		// batch was loaded shows at once.
		b := *d
		if listed, ok := findBatch(w.batches, w.batch); ok {
			recaps := make(map[string]string, len(b.Files))
			for _, f := range b.Files {
				recaps[f.ID] = f.Recap
			}
			b = listed
			b.Files = append([]store.BatchFile(nil), listed.Files...)
			for i := range b.Files {
				b.Files[i].Recap = recaps[b.Files[i].ID]
			}
		}
		return b, true
	}
	return findBatch(w.batches, w.batch)
}

// batchPage is a batch: its name, when it was imported, how far it has
// come or how it ended, and its files, each opening as a session does.
func (w *mainWindow) batchPage(c *ui.Context) {
	b, ok := w.shownBatch()
	if !ok {
		return
	}
	p := kit.Colors(c)
	bp := progressOfBatch(w.app.importer, b)
	if bp.busy {
		c.After(time.Second) // the share uploaded, read when shown
	}
	ui.Scroll(c).Key("batch-"+b.ID).Grow(1).Padding(12, 40, 40).Children(func() {
		centered(c).Children(func() {
			ui.Text(c, b.Name).FontSize(26).Bold().Selectable()
			w.batchMeta(c, b, bp)
			if bp.busy {
				w.batchProgressView(c, b, bp)
			} else {
				ui.Row(c).Margin(24, 0, 0, 0).Gap(4).Children(func() {
					ui.Text(c, strconv.Itoa(bp.ready)+" of "+strconv.Itoa(bp.total)+" ready").FontSize(15).FontWeight(600).
						FontFeatures(kit.Tabular)
					if note := endNote(bp); note != "" {
						ui.Text(c, "· "+note).FontSize(15).TextColor(p.Secondary)
					}
				})
			}
			margin := float32(16)
			if bp.busy {
				margin = 24
			}
			ui.Column(c).Margin(margin, 0, 0, 0).Radius(10).Background(p.Window).Border(1, p.Separator).AlignItems(ui.Stretch).
				Children(func() {
					for i, f := range b.Files {
						if i > 0 {
							ui.Box(c).Height(1).Margin(0, 14, 0, 42).Background(p.Separator)
						}
						w.batchFileRow(c, b, f, bp.files[i])
					}
				})
		})
	})
}

// endNote is what went wrong of a batch done: "1 couldn’t be
// transcribed", "2 canceled".
func endNote(bp batchProgress) string {
	var notes []string
	if bp.failed > 0 {
		notes = append(notes, strconv.Itoa(bp.failed)+" couldn’t be transcribed")
	}
	if bp.canceled > 0 {
		notes = append(notes, strconv.Itoa(bp.canceled)+" canceled")
	}
	return strings.Join(notes, " · ")
}

// batchMeta is the line under a batch's name: when it was imported, its
// files and their length, then its actions, as a session's header has
// its own.
func (w *mainWindow) batchMeta(c *ui.Context, b store.BatchSummary, bp batchProgress) {
	p := kit.Colors(c)
	parts := []string{"Imported " + dayText(b.CreatedAt, c.Now()), strings.ToLower(filesText(len(b.Files)))}
	if !bp.unknownLength {
		parts = append(parts, totalText(bp.length))
	}
	ui.Row(c).Gap(6).AlignItems(ui.Center).Margin(6, 0, 0, 0).TextColor(p.Secondary).Wrap().Children(func() {
		for i, part := range parts {
			if i > 0 {
				ui.Text(c, "·").TextColor(p.Tertiary)
			}
			ui.Text(c, part).FontFeatures(kit.Tabular).SingleLine()
		}
		ui.Row(c).Gap(2).AlignItems(ui.Center).Margin(0, 0, 0, 2).Children(func() {
			saved := bp.ready > 0
			if metaButton(c, kit.CopyIcon, "Copy All as Markdown").Disabled(!saved).Clicked() {
				w.copyBatch(b, false)
			}
			if metaButton(c, kit.ShareIcon, "Export All…").Disabled(!saved).Clicked() {
				w.copyBatch(b, true)
			}
			if metaButton(c, kit.TrashIcon, "Delete Batch").Clicked() {
				w.deleting = b.ID
			}
		})
	})
}

// batchProgressView is how far a batch under way has come: how many files
// are ready, Cancel Remaining, a bar, and that Capy notifies.
func (w *mainWindow) batchProgressView(c *ui.Context, b store.BatchSummary, bp batchProgress) {
	p := kit.Colors(c)
	ui.Row(c).Margin(24, 0, 0, 0).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Text(c, strconv.Itoa(bp.ready)+" of "+strconv.Itoa(bp.total)+" ready").FontSize(15).FontWeight(600).
			FontFeatures(kit.Tabular).Grow(1)
		if ui.Button(c, "Cancel Remaining").Height(24).Padding(0, 12).Clicked() {
			w.app.importer.CancelBatch(b.ID)
		}
	})
	ui.Box(c).Height(4).Radius(2).Background(p.Fill2).Margin(10, 0, 0, 0).Label("Import progress").Draw(func(pt *ui.Painter, r ui.Rect) {
		if bp.done > 0 {
			pt.Fill(ui.Rect{X: r.X, Y: r.Y, W: r.W * float32(min(bp.done, 1)), H: r.H}, p.Accent, 2)
		}
	})
	note := "Capy will notify you when all " + strconv.Itoa(bp.total) + " are ready. You can close this window; Capy keeps working from the menu bar."
	if bp.total == 1 {
		note = "Capy will notify you when it’s ready. You can close this window; Capy keeps working from the menu bar."
	}
	if bp.pause != session.Running {
		note = pauseText(bp.pause) + ". Capy goes on by itself."
	}
	ui.Row(c).Gap(6).AlignItems(ui.Center).Margin(10, 0, 0, 0).Children(func() {
		ui.Icon(c, kit.BellIcon).Size(13, 13).TextColor(p.Secondary)
		ui.Text(c, note).FontSize(12).LineHeight(16.0 / 12).TextColor(p.Secondary).Grow(1).Basis(0)
	})
}

// batchFileRow is a file of a batch's page: where it is, what its recap
// says once ready, and Retry once failed. It opens the file.
func (w *mainWindow) batchFileRow(c *ui.Context, b store.BatchSummary, f store.BatchFile, fp fileProgress) {
	p := kit.Colors(c)
	row := ui.ButtonBase(c).Key("batch-file:"+f.ID).Label(fileTitle(f)).MinHeight(52).Padding(8, 14).Gap(12).Row().AlignItems(ui.Center)
	if row.Hovered() {
		row.Background(p.Fill)
	}
	if row.Clicked() {
		w.openFile(f)
	}
	row.Children(func() {
		ui.Box(c).Width(16).Center().Children(func() { fileGlyph(c, fp.state, false, p.Success) })
		ui.Column(c).Grow(1).Basis(0).Children(func() {
			ui.Text(c, fileTitle(f)).FontWeight(500).SingleLine()
			ui.Text(c, fileLine(f, fp)).FontSize(12).TextColor(p.Secondary).SingleLine().FontFeatures(kit.Tabular).Margin(2, 0, 0, 0)
		})
		switch fp.state {
		case fileFailed, fileCanceled:
			if retryButton(c, "Retry") {
				w.retryFile(b.ID, f.ID)
			}
		case fileReady, fileSummarizing:
			ui.Icon(c, kit.ChevronRightIcon).Size(11, 11).TextColor(p.Tertiary)
		}
	})
}

// fileLine is what a batch's page says of file f: its length, then where
// it is or what its recap says.
func fileLine(f store.BatchFile, fp fileProgress) string {
	var what string
	switch fp.state {
	case fileWaiting, filePaused:
		what = "Waiting"
	case fileUploading:
		what = "Uploading… " + strconv.Itoa(int(fp.sent*100)) + "%"
	case fileTranscribing:
		what = "Transcribing…"
	case fileSummarizing:
		what = "Writing the recap… The transcript is ready."
	case fileFailed:
		what = problemText(f.Problem)
	case fileCanceled:
		what = "Canceled"
	case fileReady:
		switch {
		case f.Session.RecapStatus == store.RecapFailed:
			what = "The recap couldn’t be written. The transcript is saved."
		case f.Session.SegmentCount == 0:
			what = "Nothing was said."
		default:
			what = gist(f.Recap)
		}
	}
	if d := fileLength(f); d > 0 {
		if what == "" {
			return minutesText(d)
		}
		return minutesText(d) + " · " + what
	}
	return what
}

// fileGlyph shows where a file is: a dashed ring waiting, a spinner under
// way, a check when ready on a batch's page (ok), a warning when it failed.
func fileGlyph(c *ui.Context, s fileState, selected bool, ok ui.Color) {
	p := kit.Colors(c)
	white := ui.RGBA(255, 255, 255, 0.88)
	switch s {
	case fileWaiting, filePaused:
		color := p.Secondary
		if selected {
			color = white
		}
		kit.DashedCircle(c, color)
	case fileUploading, fileTranscribing, fileSummarizing:
		ui.Spinner(c).Size(12, 12)
	case fileFailed:
		color := p.Warning
		if selected {
			color = ui.RGB(255, 255, 255)
		}
		ui.Icon(c, kit.WarnIcon).Size(15, 15).TextColor(color)
	case fileReady:
		if ok != (ui.Color{}) {
			kit.CheckCircle(c, ok)
		}
	case fileCanceled:
		if ok != (ui.Color{}) {
			ui.Icon(c, kit.CloseIcon).Size(12, 12).TextColor(p.Tertiary)
		}
	}
}

// retryButton is a small button with the retry arrow. It reports a click.
func retryButton(c *ui.Context, label string) bool {
	b := ui.Button(c, "").Label(label).Height(24).Padding(0, 12).Gap(6)
	b.Children(func() {
		ui.Icon(c, kit.RetryIcon).Size(12, 12)
		ui.Text(c, label).SingleLine()
	})
	return b.Clicked()
}

// crumb is the batch a file is of, above its title: it opens the batch.
func (w *mainWindow) crumb(c *ui.Context, b store.Batch) {
	p := kit.Colors(c)
	link := ui.ButtonBase(c).Label("Show "+b.Name).Row().Gap(2).AlignItems(ui.Center).AlignSelf(ui.Start).
		Margin(0, 0, 4, 0).TextColor(p.Secondary)
	if link.Hovered() {
		link.TextColor(p.Label)
	}
	if link.Clicked() {
		w.openBatch(b.ID)
	}
	link.Children(func() {
		ui.Text(c, b.Name).FontSize(12).FontWeight(500).SingleLine()
		ui.Icon(c, kit.ChevronRightIcon).Size(9, 9)
	})
}

// filePage is a file of a batch not saved yet: its steps under way, or
// why it failed with Retry. Once its transcript is saved, it shows as its
// session.
func (w *mainWindow) filePage(c *ui.Context) {
	b, f, ok := findFile(w.batches, w.file)
	if !ok {
		return
	}
	p := kit.Colors(c)
	fp := progressOf(w.app.importer, f)
	if fp.state == fileUploading {
		c.After(time.Second)
	}
	ui.Scroll(c).Key("file-"+f.ID).Grow(1).Padding(12, 40, 40).Children(func() {
		centered(c).Children(func() {
			w.crumb(c, b.Batch)
			ui.Text(c, fileTitle(f)).FontSize(26).Bold().Selectable()
			ui.Row(c).Gap(6).AlignItems(ui.Center).Margin(6, 0, 0, 0).TextColor(p.Secondary).Wrap().Children(func() {
				ui.Text(c, fileStartedText(b.Batch, f, c.Now())).SingleLine()
				if f.Duration > 0 {
					ui.Text(c, "·").TextColor(p.Tertiary)
					ui.Text(c, lengthText(f.Duration)).FontFeatures(kit.Tabular).SingleLine()
				}
				w.fileInfoButton(c, b.Batch, f)
			})
			ui.Row(c).Margin(20, 0, 0, 0).Children(func() {
				tab := tabSummary
				segmented(c, &tab, "View", false, segment{label: "Summary"}, segment{label: "Transcript", disabled: true})
			})
			if fp.state == fileFailed || fp.state == fileCanceled {
				w.fileProblem(c, b, f, fp)
				return
			}
			fileSteps(c, f, fp, w.app.importer.Paused())
			ui.Row(c).Gap(6).AlignItems(ui.Center).Margin(28, 0, 0, 0).Children(func() {
				ui.Icon(c, kit.BellIcon).Size(13, 13).TextColor(p.Secondary)
				ui.RichText(c).FontSize(12).TextColor(p.Secondary).Children(func() {
					ui.Text(c, "You’ll get a notification when ")
					if ui.Link(c, b.Name, "").FontWeight(500).TextColor(p.Link).Clicked() {
						w.openBatch(b.ID)
					}
					ui.Text(c, " is ready.")
				})
			})
		})
	})
}

// fileSteps are a file's steps: uploading it, transcribing it, writing
// its recap; done, under way, or to come. pause is why a file waits.
func fileSteps(c *ui.Context, f store.BatchFile, fp fileProgress, pause session.Pause) {
	audio := "the audio"
	if f.Duration > 0 {
		audio = minutesText(f.Duration) + " of audio"
	}
	type step struct {
		text string
		at   int // -1 done, 0 under way, 1 to come
		wait bool
	}
	steps := []step{{"Uploaded", -1, false}, {"Transcribe " + audio, 1, false}, {"Write the recap", 1, false}}
	switch fp.state {
	case fileWaiting:
		steps[0] = step{"Waiting to upload", 0, true}
	case filePaused:
		steps[0] = step{"Waiting to upload · " + strings.ToLower(pauseText(pause)), 0, true}
	case fileUploading:
		steps[0] = step{"Uploading… " + strconv.Itoa(int(fp.sent*100)) + "%", 0, false}
	case fileTranscribing:
		steps[1] = step{"Transcribing " + audio + "…", 0, false}
	}
	p := kit.Colors(c)
	ui.Column(c).Margin(28, 0, 0, 0).Gap(14).AlignItems(ui.Stretch).Children(func() {
		for _, s := range steps {
			ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
				color := p.Label
				ui.Box(c).Width(16).Center().Children(func() {
					switch {
					case s.at < 0:
						color = p.Secondary
						ui.Icon(c, kit.CheckIcon).Size(14, 14).TextColor(color)
					case s.at > 0:
						color = p.Tertiary
						ui.Icon(c, kit.CircleIcon).Size(14, 14).TextColor(color)
					case s.wait:
						kit.DashedCircle(c, p.Secondary)
					default:
						ui.Spinner(c).Size(14, 14)
					}
				})
				ui.Text(c, s.text).TextColor(color).Grow(1).Basis(0)
			})
		}
	})
}

// fileProblem is why a file was not transcribed, with Retry.
func (w *mainWindow) fileProblem(c *ui.Context, b store.BatchSummary, f store.BatchFile, fp fileProgress) {
	p := kit.Colors(c)
	title, detail := "Couldn’t transcribe this file", problemText(f.Problem)
	if fp.state == fileCanceled {
		title, detail = "Canceled", "This file wasn’t transcribed."
	}
	ui.Row(c).Gap(10).AlignItems(ui.Start).Margin(28, 0, 0, 0).Children(func() {
		icon, color := kit.WarnIcon, p.Warning
		if fp.state == fileCanceled {
			icon, color = kit.CloseIcon, p.Tertiary
		}
		ui.Icon(c, icon).Size(16, 16).TextColor(color).Margin(1, 0, 0, 0)
		ui.Column(c).Grow(1).Basis(0).Gap(2).Children(func() {
			ui.Text(c, title)
			ui.Text(c, detail).FontSize(12).TextColor(p.Secondary)
		})
		if retryButton(c, "Retry") {
			w.retryFile(b.ID, f.ID)
		}
	})
}

// fileInfoButton opens a file's info, as Session Info does a session's.
func (w *mainWindow) fileInfoButton(c *ui.Context, b store.Batch, f store.BatchFile) {
	rows := [][2]string{{"Imported", b.CreatedAt.Local().Format("Monday, Jan 2, 2006 at 15:04")}}
	if !f.RecordedAt.IsZero() {
		rows = [][2]string{{"Recorded", f.RecordedAt.Local().Format("Monday, Jan 2, 2006 at 15:04")}}
	}
	if f.Duration > 0 {
		rows = append(rows, [2]string{"Length", lengthText(f.Duration)})
	}
	rows = append(rows, [2]string{"Audio from", f.Name}, [2]string{"Size", sizeText(f.Size)}, [2]string{"Spoken", langName(b.Lang)})
	if b.Target != "" && b.Target != b.Lang {
		rows = append(rows, [2]string{"Translated to", langName(b.Target)})
	}
	w.infoPopover(c, "File Info", func() { infoRows(c, rows) })
}
