package views

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"

	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/views/addon"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

// The tabs of a session in Capy's window.
const (
	tabSummary = iota
	tabTranscript
)

const (
	titleBarHeight = 52
	sidebarWidth   = 260 // resizable between the two below
	sidebarMin     = 220
	sidebarMax     = 360
)

// mainWindow is Capy's window: it records, lists past sessions and shows
// one's recap and transcript, or the lines of the one being recorded.
type mainWindow struct {
	app       *App
	win       *mygo.Window
	split     float32
	query     string
	searching bool // the search field has the focus
	sessions  []store.SessionSummary
	loaded    bool // the list has been read once
	reloads   int  // the reloads begun…
	reloaded  int  // …and the latest applied: an older one landing after is dropped
	keyState  keyState
	list      ui.ListState
	live      ui.ListState // the lines of the session being recorded
	selected  string
	detail    *store.SessionDetail
	tab       int    // tabSummary or tabTranscript
	busy      string // the action in progress, if any
	notice    string
	info      bool // the Session Info popover is open
	trashed   []store.TrashedSession
	trashOpen bool          // Recently Deleted shows in place of a session
	settings  *settingsPage // Settings shows in place of a session, when not nil
	back      place         // what showed before Settings or Recently Deleted opened
	batches   []store.BatchSummary
	// batch is the batch whose page shows in place of a session, with
	// its recaps once loaded; file a file of a batch not saved, whose
	// page shows.
	batch       string
	batchDetail *store.BatchSummary
	file        string
	expanded    map[string]bool // the batches whose files the sidebar lists
	sheet       *importSheet    // files dropped or chosen, to confirm
	renaming    *renameSheet
	deleting    string // the Delete Batch alert is open for this batch
	copying     string // Markdown to copy at the next frame
	confirm     bool   // the Delete Now alert is open…
	purging     string // …for this session
	trashing    string // the session being moved to Recently Deleted
	jump        jump
	recap       markdown.Options // how the Summary shows a recap
	insight     string           // the highlight whose card is expanded, "" for none
	// toHighlights scrolls the Summary to its Highlights, as the meta
	// line's count asks.
	toHighlights bool
	paneWidth    float32 // of the right side, as the last frame laid it out
	flash        flash   // a line of the recording jumped to
	// awaySince is when the user scrolled up from the recording's latest
	// line; zero while it is in view.
	awaySince time.Time
	inspector docking
	toasts    []ui.Toast // the add-on's, for the next frame
	// post runs fn on the main thread, then builds the window anew:
	// Window.Update, or the test's queue.
	post func(fn func())
}

// jump is a turn of the Transcript to bring into view and highlight, as a
// citation asks.
type jump struct {
	session string
	atMs    int64
	since   time.Time // when it came into view; zero until then
}

// highlightTime is how long a turn jumped to stays highlighted.
const highlightTime = 2 * time.Second

func newMainWindow(a *App) *mainWindow {
	w := &mainWindow{app: a, split: sidebarWidth}
	w.win = mygo.NewWindow(mygo.WindowOptions{
		Title:          "Capy",
		Width:          1040,
		Height:         700,
		MinWidth:       720,
		MinHeight:      460,
		StateKey:       "main",
		TitleBarStyle:  mygo.TitleBarHiddenInset,
		TitleBarHeight: titleBarHeight,
		Vibrancy:       mygo.VibrancySidebar,
		Content:        ui.View(w.view),
	})
	w.post, w.inspector.frame, w.inspector.post = w.win.Update, windowFrame{w.win}, w.win.Update
	w.inspector.min = mygo.Rectangle{Width: 720, Height: 460}
	w.win.OnClose(func(*mygo.CloseEvent) { w.inspector.restore() })
	ctx, stopWatching := context.WithCancel(context.Background())
	go w.watch(ctx)
	w.win.OnClosed(func() {
		stopWatching()
		if a.window == w {
			a.window = nil
			a.updateDock()
		}
	})
	return w
}

// reload lists the sessions matching the query, the batches and the
// sessions in Recently Deleted, and reads the selected session or batch
// again. With nothing selected, nor Recently Deleted or Settings open, it
// selects the newest session. With no session at all, it reads whether
// there is an API key, to welcome a first launch.
func (w *mainWindow) reload() {
	w.reloads++
	query, st, seq := w.query, w.app.store, w.reloads
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var list []store.SessionSummary
		var batches []store.BatchSummary
		var err error
		if strings.TrimSpace(query) == "" { // see searchQuery
			list, err = st.ListSessions(ctx, store.ListQuery{Limit: 200, Unbatched: true})
			if err == nil {
				batches, err = st.ListBatches(ctx)
			}
		} else {
			list, err = st.Search(ctx, query, 100)
		}
		var trashed []store.TrashedSession
		if err == nil {
			trashed, err = st.ListTrashed(ctx)
		}
		key := keyStored
		if err == nil && len(list) == 0 && len(batches) == 0 && strings.TrimSpace(query) == "" {
			key = readKeyState() // runs /usr/bin/security: off the main thread
		}
		w.post(func() {
			if err != nil {
				w.notice = "Could not load sessions."
				w.app.log.Error("list sessions", "err", err)
				return
			}
			if query != w.query || seq < w.reloaded {
				return
			}
			w.reloaded = seq
			w.sessions, w.trashed, w.loaded, w.keyState = list, trashed, true, key
			if strings.TrimSpace(query) == "" {
				w.batches = batches
			}
			w.follow()
			if w.empty() && strings.TrimSpace(query) == "" && len(list) > 0 {
				w.selectSession(list[0].ID)
			}
		})
	}()
	if w.selected != "" {
		w.load(w.selected)
	}
	if w.batch != "" {
		w.loadBatch(w.batch)
	}
}

// empty reports nothing shown in place of a session.
func (w *mainWindow) empty() bool {
	return w.selected == "" && w.batch == "" && w.file == "" && !w.trashOpen && w.settings == nil
}

// follow keeps the file shown as the library changes: a file saved shows
// as its session, one gone shows nothing.
func (w *mainWindow) follow() {
	if w.file == "" {
		return
	}
	switch _, f, ok := findFile(w.batches, w.file); {
	case !ok:
		w.file = ""
	case f.SessionID != "":
		w.selectSession(f.SessionID)
	}
}

// selectSession shows session id, at its summary. Session Info was for
// the session shown before, as Recently Deleted's alert was for one of
// its sessions: they close.
func (w *mainWindow) selectSession(id string) {
	if b, _, ok := fileOfSession(w.batches, id); ok {
		w.expand(b.ID, true) // a file shows under its batch
	}
	w.selected, w.notice, w.tab, w.info = id, "", tabSummary, false
	w.trashOpen, w.confirm, w.purging, w.jump, w.settings = false, false, "", jump{}, nil
	w.batch, w.batchDetail, w.file = "", nil, ""
	w.load(id)
}

// openBatch shows batch id's page, its files listed under it in the
// sidebar.
func (w *mainWindow) openBatch(id string) {
	w.expand(id, true)
	w.selected, w.detail, w.notice, w.info, w.jump = "", nil, "", false, jump{}
	w.trashOpen, w.confirm, w.purging, w.settings, w.file = false, false, "", nil, ""
	if w.batch != id {
		w.batchDetail = nil
	}
	w.batch = id
	w.loadBatch(id)
}

// openFile shows file f of a batch: its session once saved, else its
// page.
func (w *mainWindow) openFile(f store.BatchFile) {
	if f.SessionID != "" {
		w.selectSession(f.SessionID)
		return
	}
	w.expand(f.BatchID, true)
	w.selected, w.detail, w.notice, w.info, w.jump = "", nil, "", false, jump{}
	w.trashOpen, w.confirm, w.purging, w.settings, w.batch, w.batchDetail = false, false, "", nil, "", nil
	w.file = f.ID
}

// loadBatch reads batch id with its recaps, to show its page.
func (w *mainWindow) loadBatch(id string) {
	st := w.app.store
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		b, err := st.GetBatch(ctx, id)
		w.post(func() {
			if w.batch != id {
				return
			}
			if err != nil {
				// Gone, as a batch ungrouped: the pane asks for another.
				if !errors.Is(err, store.ErrNotFound) {
					w.app.log.Warn("load batch", "err", err)
				}
				w.batch, w.batchDetail = "", nil
				return
			}
			w.batchDetail = &b
		})
	}()
}

// expand lists, or not, batch id's files in the sidebar.
func (w *mainWindow) expand(id string, on bool) {
	if w.expanded == nil {
		w.expanded = make(map[string]bool)
	}
	w.expanded[id] = on
}

// showRecording shows session id as it starts or finishes recording,
// unless Settings is open, which the caption bar's settings are changed
// in while it records: Settings stays, and the session is in the sidebar.
func (w *mainWindow) showRecording(id string) {
	if w.settings == nil {
		w.selectSession(id)
	}
}

// openSettings shows Settings in place of a session.
func (w *mainWindow) openSettings() {
	if w.settings != nil {
		return
	}
	w.keepBack()
	w.selected, w.detail, w.notice, w.info, w.jump = "", nil, "", false, jump{}
	w.trashOpen, w.confirm, w.purging, w.batch, w.batchDetail, w.file = false, false, "", "", nil, ""
	w.settings = newSettingsPage(w)
}

// place is what shows in place of a session, to go back to: a session, a
// batch or a file of one, or none of them.
type place struct{ session, batch, file string }

// keepBack remembers what shows, as Settings or Recently Deleted opens in
// its place: going from one to the other keeps what showed before both.
func (w *mainWindow) keepBack() {
	if w.settings == nil && !w.trashOpen {
		w.back = place{session: w.selected, batch: w.batch, file: w.file}
	}
}

// leave closes Settings or Recently Deleted for what showed before:
// with an empty library, the window before the first session.
func (w *mainWindow) leave() {
	w.settings, w.trashOpen, w.confirm, w.purging, w.notice = nil, false, false, "", ""
	switch b := w.back; {
	case b.session != "":
		w.selectSession(b.session)
	case b.batch != "":
		w.openBatch(b.batch)
	case b.file != "":
		if _, f, ok := findFile(w.batches, b.file); ok {
			w.openFile(f)
		}
	}
	w.back = place{}
}

// showTranscript shows session id's Transcript at the turn said at atMs
// into it, which comes into view highlighted.
func (w *mainWindow) showTranscript(id string, atMs int64) {
	if w.selected != id {
		w.selectSession(id)
	}
	w.tab, w.jump = tabTranscript, jump{session: id, atMs: atMs}
}

func (w *mainWindow) load(id string) {
	st := w.app.store
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		d, err := st.GetSession(ctx, id)
		w.post(func() {
			if w.selected != id {
				return
			}
			if err != nil {
				// Gone, as a session deleted since the menu listed it: the
				// pane asks for another rather than staying blank.
				w.app.log.Warn("load session", "err", err)
				w.selected, w.detail = "", nil
				return
			}
			w.detail = &d
		})
	}()
}

func (w *mainWindow) view(c *ui.Context) {
	kit.Theme(c)
	if c.Vibrancy() {
		c.Root().Background(ui.Transparent) // the sidebar's material shows
	}
	for _, t := range w.toasts {
		c.AddToast(t)
	}
	w.toasts = nil
	w.split = min(max(w.split, sidebarMin), sidebarMax)
	pane := w.inspector.follow(w.app.ext.Inspector(c, w.inspected()), widened)
	// Files dropped anywhere in the window are imported.
	root := ui.Column(c).Fill().AlignItems(ui.Stretch)
	over := root.FileDragOver()
	if paths := root.DroppedFiles(); paths != nil {
		w.addFiles(paths)
	}
	root.Children(func() {
		ui.Split(c, &w.split, func() { w.sidebar(c) }, func() {
			if pane == nil {
				w.content(c)
				return
			}
			w.withInspector(c, pane, func() { w.content(c) })
		}).Fill()
		if over {
			dropOverlay(c)
		}
	})
}

// addonSession is session d, in view, for the add-on's slots: nil for
// none.
func (w *mainWindow) addonSession(d *store.SessionDetail) *addon.Session {
	if d == nil {
		return nil
	}
	return &addon.Session{ID: d.ID, Title: d.Title, Live: w.app.sessionLive(d.ID)}
}

// inspected is the session the inspector is for: the one selected, from
// its row while its detail loads, so that the pane does not flip to no
// session at every click in the sidebar.
func (w *mainWindow) inspected() *addon.Session {
	if d := w.shown(); d != nil {
		return w.addonSession(d)
	}
	for _, s := range w.sessions {
		if s.ID == w.selected {
			return &addon.Session{ID: s.ID, Title: s.Title, Live: w.app.sessionLive(s.ID)}
		}
	}
	return nil
}

// searchQuery is what is searched for; blank lists every session.
func (w *mainWindow) searchQuery() string { return strings.TrimSpace(w.query) }

// shown is the selected session once it is loaded, else nil.
func (w *mainWindow) shown() *store.SessionDetail {
	if w.detail == nil || w.detail.ID != w.selected {
		return nil
	}
	return w.detail
}

// record starts or stops captions; without an API key, it opens Settings
// to add one instead.
func (w *mainWindow) record() {
	if w.keyState == keyNone {
		w.app.OpenSettings()
		return
	}
	w.app.ToggleCaptions()
}

func (w *mainWindow) sidebar(c *ui.Context) {
	p := kit.Colors(c)
	side := ui.Column(c).Fill().AlignItems(ui.Stretch)
	if !c.Vibrancy() {
		side.Background(p.Sidebar)
	}
	side.Children(func() {
		ui.Box(c).Height(max(c.TitleBar().Height, titleBarHeight)).DragWindow() // under the window controls
		ui.Column(c).Padding(0, 10, 10).AlignItems(ui.Stretch).Children(func() {
			search := ui.SearchField(c, &w.query).Label("Search")
			w.searching = search.Focused()
			if search.Changed() {
				w.reload()
			}
		})
		batches := w.batches
		if w.searchQuery() != "" {
			batches = nil // results are sessions
		}
		entries := sidebarEntries(w.sessions, batches, w.searchQuery(), c.Now(), w.expanded)
		if len(entries) == 0 {
			w.sidebarEmpty(c)
			ui.Spacer(c)
			w.sidebarFooter(c)
			return
		}
		w.list.Key = func(i int) any {
			switch e := entries[i]; {
			case e.header != "":
				return "header:" + e.header
			case e.batch != nil:
				return "batch:" + e.batch.ID
			case e.file != nil:
				return "file:" + e.file.ID
			}
			return entries[i].session.ID
		}
		ui.List(c, &w.list, len(entries), func(i int) {
			switch e := entries[i]; {
			case e.header != "":
				ui.Text(c, e.header).FontSize(11).FontWeight(600).TextColor(p.Secondary).Padding(14, 20, 4)
			case e.batch != nil:
				w.batchRow(c, *e.batch, e.older)
			case e.file != nil:
				w.fileRow(c, *e.file, e.last)
			default:
				w.row(c, e)
			}
		}).Grow(1)
		w.sidebarFooter(c)
	})
}

// sidebarFooter is the bar along the sidebar's bottom: Settings on the
// left, Recently Deleted on the right. Each, clicked again, leaves its
// page.
func (w *mainWindow) sidebarFooter(c *ui.Context) {
	p := kit.Colors(c)
	ui.Box(c).Height(1).Background(p.Separator)
	ui.Row(c).Height(40).Padding(0, 10).AlignItems(ui.Center).Children(func() {
		b := kit.ToolbarButton(c, kit.SettingsIcon, "Settings")
		if w.settings != nil {
			b.Background(p.Fill2).TextColor(p.Label)
		}
		if b.Clicked() {
			if w.settings != nil {
				w.leave()
			} else {
				w.openSettings()
			}
		}
		ui.Spacer(c)
		w.trashButton(c)
	})
}

func (w *mainWindow) sidebarEmpty(c *ui.Context) {
	if !w.loaded {
		return
	}
	p := kit.Colors(c)
	title, detail := "No Sessions", "Sessions you record or import appear here."
	if q := w.searchQuery(); q != "" {
		title, detail = "No matches for “"+q+"”", "Search looks in transcripts."
	}
	ui.Column(c).Padding(36, 24).Gap(4).AlignItems(ui.Center).Children(func() {
		ui.Text(c, title).TextColor(p.Secondary).TextAlign(ui.Center)
		ui.Text(c, detail).FontSize(11).TextColor(p.Tertiary).TextAlign(ui.Center)
	})
}

// sidebarEntry is a day's header, a session, a batch, or a file of a
// batch listed under it.
type sidebarEntry struct {
	header  string
	session store.SessionSummary
	older   bool // before yesterday: its row shows the date, not the time
	batch   *store.BatchSummary
	file    *store.BatchFile
	last    bool // the last file of its batch
}

// sidebarEntries groups sessions and batches, newest first, under Today,
// Yesterday, Previous 7 Days and Earlier, a batch where it was imported,
// followed by its files when expanded; search results flat, under their
// count.
func sidebarEntries(sessions []store.SessionSummary, batches []store.BatchSummary, query string, now time.Time,
	expanded map[string]bool) []sidebarEntry {
	var out []sidebarEntry
	if query != "" {
		if len(sessions) == 0 {
			return nil
		}
		header := strconv.Itoa(len(sessions)) + " Results"
		if len(sessions) == 1 {
			header = "1 Result"
		}
		out = append(out, sidebarEntry{header: header})
		for _, s := range sessions {
			_, older := dayGroup(s.StartedAt, now)
			out = append(out, sidebarEntry{session: s, older: older})
		}
		return out
	}
	last := ""
	add := func(at time.Time, e sidebarEntry) bool {
		day, older := dayGroup(at, now)
		if day != last {
			out = append(out, sidebarEntry{header: day})
			last = day
		}
		e.older = older
		out = append(out, e)
		return older
	}
	i, j := 0, 0
	for i < len(sessions) || j < len(batches) {
		if j < len(batches) && (i == len(sessions) || !batches[j].CreatedAt.Before(sessions[i].StartedAt)) {
			b := &batches[j]
			j++
			older := add(b.CreatedAt, sidebarEntry{batch: b})
			if expanded[b.ID] {
				for k := range b.Files {
					out = append(out, sidebarEntry{file: &b.Files[k], older: older, last: k == len(b.Files)-1})
				}
			}
			continue
		}
		add(sessions[i].StartedAt, sidebarEntry{session: sessions[i]})
		i++
	}
	return out
}

// dayGroup names the group of sessions started at t, and reports whether
// t is before yesterday.
func dayGroup(t, now time.Time) (string, bool) {
	t, now = t.Local(), now.Local()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	switch {
	case !t.Before(today):
		return "Today", false
	case !t.Before(today.AddDate(0, 0, -1)):
		return "Yesterday", false
	case !t.Before(today.AddDate(0, 0, -7)):
		return "Previous 7 Days", true
	}
	return "Earlier", true
}

func (w *mainWindow) row(c *ui.Context, e sidebarEntry) {
	p := kit.Colors(c)
	s := e.session
	live := w.app.sessionLive(s.ID)
	sub := p.Secondary
	row := ui.ButtonBase(c).Key(s.ID).Height(46).Margin(0, 10).Padding(0, 10).Radius(6).Gap(10).Row().AlignItems(ui.Center)
	switch {
	case s.ID == w.selected:
		row.Background(p.Accent).TextColor(ui.RGB(255, 255, 255))
		sub = ui.RGBA(255, 255, 255, 0.82)
	case row.Hovered():
		row.Background(p.Fill)
	}
	if row.Clicked() {
		w.selectSession(s.ID)
	}
	row.Children(func() {
		ui.Box(c).Width(16).Center().Children(func() {
			switch {
			case live && s.ID == w.selected:
				kit.LiveDot(c, ui.RGB(255, 255, 255))
			case live:
				kit.LiveDot(c, kit.LiveRed)
			default:
				ui.Icon(c, sourceIcon(s.Source)).Size(16, 16).TextColor(sub)
			}
		})
		ui.Column(c).Grow(1).Basis(0).Gap(1).Children(func() {
			title := s.Title
			if title == "" {
				title = untitled(s)
			}
			ui.Text(c, title).FontWeight(600).SingleLine()
			meta := s
			if live {
				meta = w.app.captions.live(s)
			}
			ui.Text(c, rowMeta(c, meta, e.older)).FontSize(11).TextColor(sub).SingleLine().FontFeatures(kit.Tabular)
		})
	})
}

// rowMeta is "09:30 · 01:35 · English → 中文", with the date in place of
// the time for older sessions; a session being recorded ticks.
func rowMeta(c *ui.Context, s store.SessionSummary, older bool) string {
	when := s.StartedAt.Local().Format("15:04")
	if older {
		when = s.StartedAt.Local().Format("Jan 2")
	}
	end := s.EndedAt
	if end.IsZero() {
		end = c.Now()
		c.After(time.Second)
	}
	return when + " · " + clock(end.Sub(s.StartedAt)) + " · " + langPair(s, true)
}

func untitled(s store.SessionSummary) string {
	switch {
	case s.Source == store.SourceFile:
		return session.FileTitle(s.FileName)
	case s.EndedAt.IsZero():
		return "Recording…"
	case s.RecapStatus == store.RecapPending:
		return "Writing recap…"
	}
	return "Untitled session"
}

// toolbar is the row along the top of the content: the add-on's buttons,
// Import… and Record with its setup menu, the one filled control of the
// window. The session's actions are in its header.
func (w *mainWindow) toolbar(c *ui.Context, d *store.SessionDetail) {
	ui.Row(c).Height(max(c.TitleBar().Height, titleBarHeight)).Padding(0, 14).Gap(2).AlignItems(ui.Center).Justify(ui.End).
		DragWindow().Children(func() {
		if w.busy != "" {
			ui.Spinner(c).Label(w.busy).Margin(0, 8, 0, 0)
		}
		w.app.ext.Toolbar(c, w.addonSession(d))
		ui.Box(c).Width(8)
		if importButton(c) {
			w.chooseFiles()
		}
		ui.Box(c).Width(8)
		if recordSplitButton(c, w.app) {
			w.record()
		}
	})
}

// shortcuts are the window's keys: ⌘O imports audio files, Escape leaves
// Settings or Recently Deleted; and the session's: ⌘I shows Session Info,
// ⇧⌘C copies it as Markdown, ⇧⌘E exports it, ⌘⌫ deletes it, ⌘1 and ⌘2
// switch the tabs. A session being recorded has Session Info only. The
// add-on's come first.
func (w *mainWindow) shortcuts(c *ui.Context, d *store.SessionDetail) {
	w.app.ext.Shortcuts(c, w.addonSession(d))
	if c.Shortcut(ui.Cmd, ui.KeyO) {
		w.chooseFiles()
		return
	}
	if (w.settings != nil || w.trashOpen) && c.Shortcut(0, ui.KeyEscape) {
		w.leave()
		return
	}
	if d == nil {
		return
	}
	if c.Shortcut(ui.Cmd, ui.KeyI) {
		w.info = !w.info
		return
	}
	if w.app.sessionLive(d.ID) {
		return
	}
	switch {
	case c.Shortcut(ui.Cmd|ui.Shift, ui.KeyC):
		w.copyMarkdown(c, d)
	case c.Shortcut(ui.Cmd|ui.Shift, ui.KeyE):
		w.export(*d)
	case !w.searching && !d.EndedAt.IsZero() && c.Shortcut(ui.Cmd, ui.KeyBackspace):
		w.delete(d.ID)
	case c.Shortcut(ui.Cmd, ui.Key1):
		w.tab = tabSummary
	case c.Shortcut(ui.Cmd, ui.Key2):
		w.tab = tabTranscript
	}
}

// dismiss marks highlight id of session sessionID not important: through
// the recording while live, else in the library, read again after.
func (w *mainWindow) dismiss(sessionID, id string, live bool) {
	st, log := w.app.store, w.app.log
	var run liveSession
	if b := w.app.captions; live && b != nil && b.run != nil && b.run.Snapshot().SessionID == sessionID {
		run = b.run
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var err error
		if run != nil {
			err = run.Dismiss(ctx, id)
		} else {
			err = st.DismissHighlight(ctx, id)
		}
		if err != nil {
			log.Warn("dismiss highlight", "err", err)
			return
		}
		w.post(w.reload)
	}()
}

func (w *mainWindow) copyMarkdown(c *ui.Context, d *store.SessionDetail) {
	c.WriteClipboard(w.markdown(*d))
	c.Toast("Copied")
}

// markdown is session d as a document, with the add-on's sections.
func (w *mainWindow) markdown(d store.SessionDetail) string {
	return session.Markdown(d, w.app.ext.Export(*w.addonSession(&d))...)
}

// recapFailed reports a recap to retry: one that failed, or one saved
// empty for a session where something was said.
func recapFailed(d *store.SessionDetail) bool {
	return d.RecapStatus == store.RecapFailed ||
		d.RecapStatus == store.RecapReady && d.Recap == "" && d.SegmentCount > 0
}

func (w *mainWindow) retry(what, id string) {
	w.busy = "Retrying " + what + "…"
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		ai, err := w.app.client() // reads the Keychain: off the main thread
		if err != nil {
			w.post(func() { w.busy, w.notice = "", explain(err).message })
			return
		}
		deps := session.Deps{AI: ai, Store: w.app.store, Log: w.app.log}
		var notice string
		if what == "recap" {
			if err := session.Recap(ctx, deps, id); err != nil {
				notice = explain(err).message
			}
		} else {
			failed, err := session.RetryTranslations(ctx, deps, id)
			switch {
			case err != nil:
				notice = explain(err).message
			case failed > 0:
				notice = "Some lines still could not be translated."
			}
		}
		w.post(func() {
			w.busy, w.notice = "", notice
			w.reload()
			w.app.refreshMenu()
		})
	}()
}

// findHighlights finds the highlights of saved session id, as Settings ›
// Highlights has them, and says how many it found.
func (w *mainWindow) findHighlights(id string) {
	w.busy, w.notice = "Finding highlights…", ""
	cfg := w.app.highlightConfig()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		ai, err := w.app.client() // reads the Keychain: off the main thread
		n := 0
		if err == nil {
			n, err = session.FindHighlights(ctx, w.app.sessionDeps(ai), id, cfg)
		}
		var notice string
		switch {
		case errors.Is(err, zoomai.ErrUnauthorized) && ai != nil:
			notice = "Your API key can’t use Zoom Decisions."
		case err != nil:
			notice = explain(err).message
		case n == 0:
			notice = "No new highlights found."
		case n == 1:
			notice = "Found 1 highlight."
		default:
			notice = "Found " + strconv.Itoa(n) + " highlights."
		}
		w.post(func() {
			w.busy, w.notice = "", notice
			w.reload()
		})
	}()
}

func (w *mainWindow) export(d store.SessionDetail) { w.exportText(d.Title, w.markdown(d)) }

// nextAfterDelete is the session to show once id is deleted: the next
// older, else the next newer, else none.
func nextAfterDelete(sessions []store.SessionSummary, id string) string {
	for i, s := range sessions {
		switch {
		case s.ID != id:
		case i+1 < len(sessions):
			return sessions[i+1].ID
		case i > 0:
			return sessions[i-1].ID
		}
	}
	return ""
}

// undoTime is how long the toast offering to undo a delete shows.
const undoTime = 4 * time.Second

// delete moves session id to Recently Deleted, shows nextAfterDelete's,
// and offers to undo it.
func (w *mainWindow) delete(id string) {
	if w.trashing == id {
		return // a repeated ⌘⌫ while the first is under way
	}
	w.trashing = id
	next := nextAfterDelete(w.sessions, id)
	name := untitledOr(w.sessions, id)
	st := w.app.store
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		edit, err := st.TrashSession(ctx, id, "user", "")
		w.post(func() {
			w.trashing = ""
			if err != nil {
				w.notice = "Could not delete the session."
				w.app.log.Error("trash session", "err", err)
				return
			}
			w.selected, w.detail = "", nil
			if next != "" {
				w.selectSession(next)
			}
			w.reload()
			w.app.refreshMenu()
			w.toasts = append(w.toasts, ui.Toast{Title: "Moved “" + name + "” to Recently Deleted", Timeout: undoTime,
				Action: "Undo", OnAction: func() { w.undo(edit.ID) }})
		})
	}()
}

// undo reverts edit editID, and shows its session.
func (w *mainWindow) undo(editID string) {
	st := w.app.store
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		edit, err := st.UndoEdit(ctx, editID)
		w.post(func() {
			if err != nil {
				w.notice = "Could not undo it."
				w.app.log.Error("undo edit", "err", err)
				return
			}
			w.selectSession(edit.SessionID)
			w.reload()
			w.app.refreshMenu()
		})
	}()
}

// untitledOr is the title of session id among sessions, as the sidebar
// shows it.
func untitledOr(sessions []store.SessionSummary, id string) string {
	for _, s := range sessions {
		if s.ID == id {
			if s.Title == "" {
				return untitled(s)
			}
			return s.Title
		}
	}
	return "Untitled session"
}

// watch reloads the window when another process, such as Capy mcp, has
// changed the library: it reads the library's version every second until
// ctx ends.
func (w *mainWindow) watch(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var last int64
	known := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		v, err := w.app.store.DataVersion(ctx)
		if err != nil {
			w.app.log.Debug("library version", "err", err)
			continue
		}
		if known && v != last {
			w.post(w.reload)
		}
		last, known = v, true
	}
}
