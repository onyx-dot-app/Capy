package views

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/views/addon"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
)

// batchRow is a batch in the sidebar: a waveform, like an imported
// session's, that turns into a disclosure button under the pointer and
// lists its files under it, its name over how far it has come, and a ring
// while it is imported, or a warning once a file failed. It opens the batch.
func (w *mainWindow) batchRow(c *ui.Context, b store.BatchSummary, older bool) {
	p := kit.Colors(c)
	bp := progressOfBatch(w.app.importer, b)
	if bp.busy {
		c.After(time.Second) // the share uploaded
	}
	selected := w.batch == b.ID
	text, sub := p.Label, p.Secondary
	if selected {
		text, sub = ui.RGB(255, 255, 255), ui.RGBA(255, 255, 255, 0.88)
	}
	open := w.expanded[b.ID]
	row := ui.ButtonBase(c).Key("batch:"+b.ID).Label(b.Name).Height(46).Margin(0, 10).Padding(0, 10).Radius(6).Gap(10).
		Row().AlignItems(ui.Center).TextColor(text)
	switch {
	case selected:
		row.Background(p.Accent)
	case row.Hovered():
		row.Background(p.Fill)
	}
	if row.Clicked() {
		w.openBatch(b.ID)
	}
	row.ContextMenu(func(m *ui.Menu) { w.batchMenu(m, b, bp) })
	row.Children(func() {
		icon, name := kit.ChevronRightIcon, "Show Files"
		if open {
			icon, name = kit.ChevronDownIcon, "Hide Files"
		}
		disclose := ui.ButtonBase(c).Label(name+" of "+b.Name).Size(16, 16).Center().TextColor(sub)
		if disclose.Clicked() {
			w.expand(b.ID, !open)
		}
		disclose.Children(func() {
			if row.Hovered() {
				ui.Icon(c, icon).Size(10, 10)
			} else {
				ui.Icon(c, kit.WaveformIcon).Size(16, 16)
			}
		})
		ui.Column(c).Grow(1).Basis(0).Gap(1).Children(func() {
			ui.Text(c, b.Name).FontWeight(600).SingleLine()
			ui.Text(c, batchSubtitle(b, bp, older)).FontSize(11).TextColor(sub).SingleLine().FontFeatures(kit.Tabular)
		})
		ui.Box(c).Width(16).Center().Children(func() {
			switch {
			case bp.busy:
				color := p.Accent
				switch {
				case selected:
					color = ui.RGB(255, 255, 255)
				case bp.pause != session.Running:
					color = p.Secondary
				}
				kit.Ring(c, color, float32(bp.finished), strconv.Itoa(bp.ready)+" of "+strconv.Itoa(bp.total)+" ready")
			case bp.failed > 0:
				color := p.Warning
				if selected {
					color = ui.RGB(255, 255, 255)
				}
				ui.Icon(c, kit.WarnIcon).Size(15, 15).TextColor(color)
			}
		})
	})
}

// batchSubtitle is what a batch's row says under its name: how far it
// has come, or why it waits, while imported; how many files failed once
// done; else when it was imported and how many sessions it has.
func batchSubtitle(b store.BatchSummary, bp batchProgress, older bool) string {
	ready := strconv.Itoa(bp.ready)
	switch {
	case bp.busy && bp.pause != session.Running:
		return pauseText(bp.pause)
	case bp.busy:
		return ready + " of " + strconv.Itoa(bp.total) + " ready"
	case bp.failed > 0:
		return ready + " ready · " + strconv.Itoa(bp.failed) + " failed"
	case bp.canceled > 0:
		return ready + " ready · " + strconv.Itoa(bp.canceled) + " canceled"
	}
	when := b.CreatedAt.Local().Format("15:04")
	if older {
		when = b.CreatedAt.Local().Format("Jan 2")
	}
	sessions := strconv.Itoa(bp.ready) + " sessions"
	if bp.ready == 1 {
		sessions = "1 session"
	}
	return when + " · " + sessions
}

// fileRow is a file of a batch, listed under it: its title over where it
// is, or when it was recorded and how long it is once ready, indented
// right of a guide line. It opens the file.
func (w *mainWindow) fileRow(c *ui.Context, f store.BatchFile, last bool) {
	p := kit.Colors(c)
	b, _ := findBatch(w.batches, f.BatchID)
	fp := progressOf(w.app.importer, f)
	if fp.state == fileUploading {
		c.After(time.Second)
	}
	selected := f.SessionID != "" && f.SessionID == w.selected || f.ID == w.file
	text, sub := p.Label, p.Secondary
	if selected {
		text, sub = ui.RGB(255, 255, 255), ui.RGBA(255, 255, 255, 0.88)
	}
	ui.Box(c).Key("file:" + f.ID).Height(40).Children(func() {
		guide := float32(0)
		if last {
			guide = 4
		}
		ui.Box(c).Absolute().Left(28).Top(0).Bottom(guide).Width(1).Background(p.Separator)
		row := ui.ButtonBase(c).Label(fileTitle(f)).Height(40).Margin(0, 10, 0, 50).Padding(0, 10, 0, 6).Radius(6).Gap(8).
			Row().AlignItems(ui.Center).TextColor(text)
		switch {
		case selected:
			row.Background(p.Accent)
		case row.Hovered():
			row.Background(p.Fill)
		}
		if row.Clicked() {
			w.openFile(f)
		}
		row.ContextMenu(func(m *ui.Menu) { w.fileMenu(m, b, f, fp) })
		row.Children(func() {
			ui.Column(c).Grow(1).Basis(0).Gap(1).Children(func() {
				ui.Text(c, fileTitle(f)).SingleLine()
				ui.Text(c, fileSubtitle(b, f, fp)).FontSize(11).TextColor(sub).SingleLine().FontFeatures(kit.Tabular)
			})
			ui.Box(c).Width(16).Center().Children(func() { fileGlyph(c, fp.state, selected, ui.Color{}) })
		})
	})
}

// fileSubtitle is what a file's row says under its title: where it is,
// why it failed, or, once ready, the day it was recorded, else imported,
// and its length.
func fileSubtitle(b store.BatchSummary, f store.BatchFile, fp fileProgress) string {
	switch fp.state {
	case fileWaiting, filePaused:
		return "Waiting"
	case fileUploading:
		return "Uploading · " + strconv.Itoa(int(fp.sent*100)) + "%"
	case fileTranscribing:
		return "Transcribing…"
	case fileSummarizing:
		return "Writing recap…"
	case fileFailed:
		return problemShort(f.Problem)
	case fileCanceled:
		return "Canceled"
	}
	day := f.RecordedAt
	if day.IsZero() {
		day = b.CreatedAt
	}
	text := day.Local().Format("Jan 2")
	if d := fileLength(f); d > 0 {
		text += " · " + minutesText(d)
	}
	return text
}

// batchMenu is a batch row's context menu.
func (w *mainWindow) batchMenu(m *ui.Menu, b store.BatchSummary, bp batchProgress) {
	if m.Item("Rename").Chosen() {
		w.renaming = &renameSheet{batch: b.ID, title: "Rename Batch", name: b.Name}
	}
	w.app.ext.BatchMenu(m, batchOf(b))
	m.Separator()
	if m.Item("Copy All as Markdown").Disabled(bp.ready == 0).Chosen() {
		w.copyBatch(b, false)
	}
	if m.Item("Export All…").Disabled(bp.ready == 0).Chosen() {
		w.copyBatch(b, true)
	}
	m.Separator()
	if m.Item("Retry Failed").Disabled(bp.failed == 0).Chosen() {
		w.retryFailed(b.ID)
	}
	if m.Item("Cancel Remaining").Disabled(!bp.busy).Chosen() {
		w.app.importer.CancelBatch(b.ID)
	}
	m.Separator()
	if m.Item("Ungroup").Chosen() {
		w.ungroup(b.ID)
	}
	if m.Item("Delete Batch").Chosen() {
		w.deleting = b.ID
	}
}

// fileMenu is a file row's context menu.
func (w *mainWindow) fileMenu(m *ui.Menu, b store.BatchSummary, f store.BatchFile, fp fileProgress) {
	saved := f.SessionID != ""
	if m.Item("Rename").Chosen() {
		w.renaming = &renameSheet{file: f.ID, session: f.SessionID, title: "Rename", name: fileTitle(f)}
	}
	if m.Item("Session Info").Chosen() {
		w.openFile(f)
		w.info = true
	}
	m.Separator()
	if m.Item("Copy as Markdown").Disabled(!saved || fp.state == fileSummarizing).Chosen() {
		w.withSession(f.SessionID, func(d store.SessionDetail) { w.copying = w.markdown(d) })
	}
	if m.Item("Export…").Disabled(!saved || fp.state == fileSummarizing).Chosen() {
		w.withSession(f.SessionID, w.export)
	}
	m.Separator()
	if m.Item("Cancel Transcription").Disabled(!fp.state.busy()).Chosen() {
		w.app.importer.Cancel(f.ID)
	}
	if m.Item("Retry").Disabled(fp.state != fileFailed && fp.state != fileCanceled).Chosen() {
		w.retryFile(b.ID, f.ID)
	}
	m.Separator()
	if m.Item("Move Out of Batch").Chosen() {
		w.libraryWrite("Could not move the file.", func(ctx context.Context, st store.Store) error { return st.RemoveFile(ctx, f.ID) })
	}
	if m.Item("Delete").Chosen() {
		w.deleteFile(f)
	}
}

// deleteFile moves a file saved to Recently Deleted, as a session, and
// cancels one not saved, forgetting it.
func (w *mainWindow) deleteFile(f store.BatchFile) {
	if f.SessionID != "" {
		w.delete(f.SessionID)
		return
	}
	w.app.importer.Cancel(f.ID)
	w.libraryWrite("Could not delete the file.", func(ctx context.Context, st store.Store) error { return st.RemoveFile(ctx, f.ID) })
}

// retryFile transcribes file fileID of batch batchID again.
func (w *mainWindow) retryFile(batchID, fileID string) {
	im := w.app.importer
	w.libraryWrite("Could not retry the file.", func(ctx context.Context, _ store.Store) error {
		return im.Retry(ctx, batchID, fileID)
	})
}

// retryFailed transcribes the files of batch id that failed again.
func (w *mainWindow) retryFailed(id string) {
	im := w.app.importer
	w.libraryWrite("Could not retry the files.", func(ctx context.Context, _ store.Store) error {
		return im.RetryFailed(ctx, id)
	})
}

// ungroup ends batch id: its sessions stay, on their own; its files not
// transcribed are canceled and forgotten.
func (w *mainWindow) ungroup(id string) {
	w.app.importer.CancelBatch(id)
	w.libraryWrite("Could not ungroup the batch.", func(ctx context.Context, st store.Store) error { return st.UngroupBatch(ctx, id) })
}

// deleteBatch moves batch id's sessions to Recently Deleted and forgets
// it, its files not transcribed canceled.
func (w *mainWindow) deleteBatch(id string) {
	w.app.importer.CancelBatch(id)
	st := w.app.store
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		edits, err := st.DeleteBatch(ctx, id, "user", "")
		w.post(func() {
			if err != nil {
				w.notice = "Could not delete the batch."
				w.app.log.Error("delete batch", "err", err)
				return
			}
			if w.batch == id {
				w.batch, w.batchDetail = "", nil
			}
			w.app.libraryChanged()
			title := "Deleted the batch"
			if len(edits) > 0 {
				title = "Moved " + sessionsText(len(edits)) + " to Recently Deleted"
			}
			w.toasts = append(w.toasts, ui.Toast{Title: title, Timeout: undoTime})
		})
	}()
}

// sessionsText is "1 session" or "3 sessions".
func sessionsText(n int) string {
	if n == 1 {
		return "1 session"
	}
	return strconv.Itoa(n) + " sessions"
}

// libraryWrite runs write off the main thread, then shows the library as
// it changed, or notice when it failed.
func (w *mainWindow) libraryWrite(notice string, write func(ctx context.Context, st store.Store) error) {
	st := w.app.store
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := write(ctx, st)
		w.post(func() {
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				w.notice = notice
				w.app.log.Error("library write", "err", err)
			}
			w.app.libraryChanged()
		})
	}()
}

// withSession reads session id, then gives it to then on the main thread.
func (w *mainWindow) withSession(id string, then func(store.SessionDetail)) {
	st := w.app.store
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		d, err := st.GetSession(ctx, id)
		w.post(func() {
			if err != nil {
				w.app.log.Warn("read session", "err", err)
				return
			}
			then(d)
		})
	}()
}

// copyBatch copies batch b's sessions as one Markdown document, or exports
// it to a file.
func (w *mainWindow) copyBatch(b store.BatchSummary, export bool) {
	st := w.app.store
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var details []store.SessionDetail
		for _, f := range b.Files {
			if f.SessionID == "" {
				continue
			}
			d, err := st.GetSession(ctx, f.SessionID)
			if err != nil {
				w.app.log.Warn("read session", "err", err)
				continue
			}
			details = append(details, d)
		}
		w.post(func() {
			parts := []string{"# " + b.Name}
			for _, d := range details {
				// Each session's headings, a level down under the batch's.
				parts = append(parts, demote(w.markdown(d)))
			}
			md := strings.Join(parts, "\n\n") + "\n"
			if export {
				w.exportText(b.Name, md)
				return
			}
			w.copying = md
		})
	}()
}

// demote moves Markdown's headings a level down.
func demote(md string) string {
	var b strings.Builder
	for line := range strings.Lines(md) {
		if strings.HasPrefix(line, "#") {
			b.WriteString("#")
		}
		b.WriteString(line)
	}
	return strings.TrimRight(b.String(), "\n")
}

// batchOf is b as the add-on knows it.
func batchOf(b store.BatchSummary) addon.Batch {
	ab := addon.Batch{ID: b.ID, Name: b.Name}
	for _, f := range b.Files {
		if f.SessionID != "" {
			ab.SessionIDs = append(ab.SessionIDs, f.SessionID)
		}
	}
	return ab
}

// exportText asks where to save md, a document named name, and saves it.
func (w *mainWindow) exportText(name, md string) {
	go func() {
		name = strings.Map(func(r rune) rune {
			if strings.ContainsRune(`/\:*?"<>|`, r) {
				return '-'
			}
			return r
		}, name)
		if name == "" {
			name = "Capy session"
		}
		path, err := mygo.Dialog.Save(mygo.SaveDialogOptions{Parent: w.win, DefaultPath: name + ".md"})
		if err != nil || path == "" {
			return
		}
		if err := os.WriteFile(path, []byte(md), 0o600); err != nil {
			w.post(func() {
				w.notice = "Could not save the file."
				// The path holds the name: log only the cause.
				if pe, ok := errors.AsType[*fs.PathError](err); ok {
					err = pe.Err
				}
				w.app.log.Error("export", "err", err)
			})
		}
	}()
}

// renameSheet renames a batch, a file not saved, or a file's session.
type renameSheet struct {
	batch, file, session string
	title, name          string
	open                 bool
}

// batchSheets are what a batch's actions ask: Rename, the Delete Batch
// alert, and the Markdown to copy.
func (w *mainWindow) batchSheets(c *ui.Context) {
	if w.copying != "" {
		c.WriteClipboard(w.copying)
		c.Toast("Copied")
		w.copying = ""
	}
	w.importSheetView(c)
	if r := w.renaming; r != nil {
		r.open = true
		w.renameView(c, r)
		if !r.open {
			w.renaming = nil
		}
	}
	if w.deleting != "" {
		b, ok := findBatch(w.batches, w.deleting)
		open := ok
		message := "Its sessions move to Recently Deleted. Files not transcribed yet are canceled."
		if ok {
			if n := progressOfBatch(w.app.importer, b).ready; n > 0 {
				message = "Its " + sessionsText(n) + " move to Recently Deleted. Files not transcribed yet are canceled."
			}
		}
		if ui.AlertDialog(c, &open, "Delete “"+b.Name+"”?", message, "Cancel", "Delete") == 1 {
			w.deleteBatch(b.ID)
		}
		if !open {
			w.deleting = ""
		}
	}
}

// renameView is the Rename sheet: a name, Cancel and Rename.
func (w *mainWindow) renameView(c *ui.Context, r *renameSheet) {
	p := kit.Colors(c)
	ui.DialogBase(c, &r.open, func(backdrop, panel ui.Element) {
		backdrop.Background(ui.RGBA(0, 0, 0, 0.08))
		panel.Width(360).Radius(12).Background(p.Sheet).Border(0.5, ui.RGBA(0, 0, 0, 0.28)).
			Shadow(0, 22, 60, 0, ui.RGBA(0, 0, 0, 0.25)).Padding(20, 20, 16).AlignItems(ui.Stretch)
		ui.Text(c, r.title).FontSize(15).FontWeight(600).Margin(0, 2, 12, 2)
		submitted := ui.TextInput(c, &r.name).Label("Name").AutoFocus().Height(24).Submitted()
		ui.Row(c).Gap(8).Justify(ui.End).Margin(16, 0, 0, 0).Children(func() {
			if ui.Button(c, "Cancel").Height(28).Padding(0, 14).Clicked() {
				r.open = false
			}
			name := strings.TrimSpace(r.name)
			if ui.PrimaryButton(c, "Rename").Height(28).Padding(0, 14).Disabled(name == "").Clicked() || submitted && name != "" {
				r.open = false
				w.rename(*r, name)
			}
		})
	})
}

// rename gives what r renames name.
func (w *mainWindow) rename(r renameSheet, name string) {
	w.libraryWrite("Could not rename it.", func(ctx context.Context, st store.Store) error {
		switch {
		case r.batch != "":
			return st.RenameBatch(ctx, r.batch, name)
		case r.session != "":
			_, err := st.RenameSession(ctx, r.session, name, "user", "")
			return err
		}
		return st.RenameFile(ctx, r.file, name)
	})
}
