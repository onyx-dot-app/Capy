package views

import (
	"context"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
)

// openTrash shows Recently Deleted in place of a session.
func (w *mainWindow) openTrash() {
	w.keepBack()
	w.selected, w.detail, w.notice, w.info, w.jump = "", nil, "", false, jump{}
	w.trashOpen, w.settings, w.batch, w.batchDetail, w.file = true, nil, "", nil, ""
}

// trashButton opens Recently Deleted from the sidebar's footer, while it
// is not empty, and leaves it, clicked again.
func (w *mainWindow) trashButton(c *ui.Context) {
	if len(w.trashed) == 0 {
		return
	}
	p := kit.Colors(c)
	b := kit.ToolbarButton(c, kit.TrashIcon, "Recently Deleted")
	if w.trashOpen {
		b.Background(p.Fill2).TextColor(p.Label)
	}
	if b.Clicked() {
		if w.trashOpen {
			w.leave()
		} else {
			w.openTrash()
		}
	}
}

// trashText says when a session was deleted and when it will be erased:
// "Deleted today · erased on Nov 11".
func trashText(trashedAt, now time.Time) string {
	t := trashedAt.Local()
	when := "Deleted " + t.Format("Jan 2")
	switch day, _ := dayGroup(t, now); day {
	case "Today":
		when = "Deleted today"
	case "Yesterday":
		when = "Deleted yesterday"
	}
	return when + " · erased on " + t.Add(store.TrashRetention).Format("Jan 2")
}

// trashPage is Recently Deleted: each session with Restore and Delete Now.
func (w *mainWindow) trashPage(c *ui.Context) {
	p := kit.Colors(c)
	ui.Scroll(c).Key("trash").Grow(1).Padding(12, 40, 40).Children(func() {
		centered(c).Children(func() {
			ui.Text(c, "Recently Deleted").FontSize(26).Bold()
			ui.Text(c, "Sessions are erased for good 30 days after they’re deleted.").TextColor(p.Secondary).Margin(6, 0, 0, 0)
			if w.notice != "" {
				ui.Text(c, w.notice).TextColor(p.Secondary).Margin(10, 0, 0, 0)
			}
			if len(w.trashed) == 0 {
				ui.Text(c, "No deleted sessions.").TextColor(p.Tertiary).Margin(24, 0, 0, 0)
				return
			}
			rows := make([]func(), len(w.trashed))
			for i, s := range w.trashed {
				rows[i] = func() { w.trashedRow(c, s) }
			}
			ui.Column(c).Margin(24, 0, 0, 0).AlignItems(ui.Stretch).Children(func() { kit.Group(c, rows...) })
		})
	})
}

// trashedRow is a session of Recently Deleted.
func (w *mainWindow) trashedRow(c *ui.Context, s store.TrashedSession) {
	p := kit.Colors(c)
	title := s.Title
	if title == "" {
		title = untitled(s.SessionSummary)
	}
	ui.Row(c).Key(s.ID).MinHeight(52).Padding(8, 12).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Column(c).Grow(1).Basis(0).Gap(2).Children(func() {
			ui.Text(c, title).FontWeight(600).SingleLine()
			status := trashText(s.TrashedAt, c.Now())
			if s.PurgePending {
				status = "Erasure pending · retry Delete Now"
			}
			ui.Text(c, status).FontSize(12).TextColor(p.Secondary).SingleLine()
		})
		if ui.Button(c, "Restore").Label("Restore "+title).Height(24).Padding(0, 12).Disabled(s.PurgePending).Clicked() {
			w.restore(s.ID)
		}
		if ui.Button(c, "Delete Now…").Label("Delete "+title+" Now").Height(24).Padding(0, 12).Clicked() {
			w.confirm, w.purging = true, s.ID
		}
	})
}

// purgeAlert asks before Delete Now erases a session.
func (w *mainWindow) purgeAlert(c *ui.Context) {
	title := "this session"
	for _, s := range w.trashed {
		if s.ID == w.purging {
			title = "“" + untitledOr([]store.SessionSummary{s.SessionSummary}, s.ID) + "”"
		}
	}
	if ui.AlertDialog(c, &w.confirm, "Delete "+title+" now?",
		"Its transcript, recap and Ask threads are erased from this Mac. You can’t undo this.", "Cancel", "Delete Now") == 1 &&
		w.purging != "" {
		w.purge(w.purging)
	}
}

// restore takes session id out of Recently Deleted, back to its day
// group, and shows it.
func (w *mainWindow) restore(id string) {
	st := w.app.store
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := st.RestoreSession(ctx, id, "user", "")
		w.post(func() {
			if err != nil {
				w.notice = "Could not restore the session."
				w.app.log.Error("restore session", "err", err)
				return
			}
			w.selectSession(id)
			w.reload()
			w.app.refreshMenu()
		})
	}()
}

// purge erases session id for good, with whatever else Capy keeps of it.
// When that fails, the session stays in Recently Deleted to try again.
func (w *mainWindow) purge(id string) {
	purger := w.app.purger
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := purger.Purge(ctx, id)
		w.post(func() {
			w.purging = ""
			if err != nil {
				w.notice = "Couldn’t delete the session."
				w.app.log.Error("purge session", "err", err)
			}
			w.reload()
		})
	}()
}
