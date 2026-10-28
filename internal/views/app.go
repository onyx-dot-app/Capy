package views

import (
	"context"
	_ "embed"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo"

	"github.com/onyx-dot-app/Capy/internal/audio"
	"github.com/onyx-dot-app/Capy/internal/features"
	"github.com/onyx-dot-app/Capy/internal/keychain"
	"github.com/onyx-dot-app/Capy/internal/lang"
	"github.com/onyx-dot-app/Capy/internal/profile"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/views/addon"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

//go:embed tray.png
var trayIcon []byte

// startTimeout bounds connecting a session, permission prompts included.
const startTimeout = 60 * time.Second

// Global shortcuts.
const (
	captionsShortcut  = "Alt+Cmd+C"
	windowShortcut    = "Alt+Cmd+L"
	translateShortcut = "Alt+Cmd+T"
)

// App is Capy's interface: the menu bar item, Capy's window, the caption
// bar and settings. Its state is
// read and changed on the main thread; sessions report from other
// goroutines through Window.Update and Window.Invalidate.
type App struct {
	store     store.Store
	prefsPath string
	prefs     Prefs
	// profilePath is profile.json, beside the preferences in Capy's data
	// directory, and profile the user's profile as last saved.
	profilePath string
	profile     profile.Profile
	log         *slog.Logger
	newClient   func(key string) (*zoomai.Client, error)
	purger      *session.Purger
	ext         addon.AddOn
	gate        features.Gate
	// highlightsDenied is a recording whose API key could not use
	// Decisions, as Settings tells.
	highlightsDenied bool

	tray          *mygo.Tray
	recent        []store.SessionSummary // the menu's, newest first
	importing     []store.BatchSummary   // the menu's: the batches under way
	menuSeq       int                    // the latest refreshMenu
	quitConfirmed bool
	captions      *captionBar
	importer      *session.Importer
	window        *mainWindow
	// onMain runs fn on the main thread, and notify shows a notification
	// id: mygo's, or a test's.
	onMain func(fn func())
	notify func(id, title, body string)
}

// NewApp returns the interface over st, whose sessions purger erases.
// newClient makes the Zoom client for an API key; ext fills the views'
// add-on slots, addon.None for none; gate says which features are on.
func NewApp(st store.Store, purger *session.Purger, prefsPath string, log *slog.Logger,
	newClient func(key string) (*zoomai.Client, error), ext addon.AddOn, gate features.Gate) *App {
	profilePath := filepath.Join(filepath.Dir(prefsPath), profile.FileName)
	a := &App{store: st, purger: purger, prefsPath: prefsPath, prefs: LoadPrefs(prefsPath), log: log,
		profilePath: profilePath, profile: profile.Read(profilePath),
		newClient: newClient, ext: ext, gate: gate, onMain: mygo.RunOnMain}
	a.notify = a.showNotification
	a.importer = a.newImporter()
	return a
}

// Start shows the menu bar item and Capy's window, and registers the global
// shortcuts. Call it once the app is ready.
func (a *App) Start() error {
	mygo.App.SetMenu(a.appMenu())
	a.updateDock()
	tray, err := mygo.NewTray(mygo.TrayOptions{
		Icon:           trayIcon,
		IconIsTemplate: true,
		ToolTip:        "Capy",
		Menu:           a.trayMenu(),
	})
	if err != nil {
		return err
	}
	a.tray = tray
	// Sessions cut short by quitting are ended and offered a recap retry at
	// the next launch (session.RecoverInterrupted).
	mygo.App.OnBeforeQuit(a.confirmQuit)
	mygo.App.OnWillQuit(func(*mygo.QuitEvent) {
		a.stopSessions()
		a.importer.Stop() // its files stay waiting, for the next launch
	})
	for shortcut, fn := range map[string]func(){
		captionsShortcut:  a.ToggleCaptions,
		windowShortcut:    func() { a.OpenWindow("") },
		translateShortcut: a.ToggleTranslation,
	} {
		if err := mygo.GlobalShortcut.Register(shortcut, fn); err != nil {
			a.log.Warn("global shortcut unavailable", "shortcut", shortcut, "err", err)
		}
	}
	a.ext.Start(host{a})
	go a.purgeExpired()
	go a.resumeImports()
	a.refreshMenu()
	a.OpenWindow("")
	return nil
}

// resumeImports transcribes the files a quit left waiting.
func (a *App) resumeImports() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := a.importer.Resume(ctx)
	if err != nil {
		a.log.Warn("resume imports", "err", err)
	}
	if n > 0 {
		a.log.Info("resumed imports", "files", n)
	}
}

func (a *App) trayMenu() *mygo.Menu {
	click := func(fn func()) func(*mygo.MenuItem, *mygo.Window) {
		return func(*mygo.MenuItem, *mygo.Window) { fn() }
	}
	record := &mygo.MenuItem{Label: "Record", Accelerator: captionsShortcut, Click: click(a.ToggleCaptions)}
	switch state, _ := a.recordState(); state {
	case recordStarting, recordLive:
		record.Label = "Stop Recording"
	case recordFinishing:
		record.Label, record.Disabled = "Finishing…", true
	}
	var targets []*mygo.MenuItem
	for _, l := range lang.Targets() {
		targets = append(targets, &mygo.MenuItem{Label: l.Name, Type: mygo.MenuItemRadio, Checked: l.Code == a.prefs.CaptionsTo,
			Click: click(func() { a.setTarget(l.Code) })})
	}
	items := []*mygo.MenuItem{
		{Label: "Open Capy", Accelerator: windowShortcut, Click: click(func() { a.OpenWindow("") })},
		record,
		{Label: "Import Audio…", Click: click(a.OpenImport)},
		{Label: "Translate", Type: mygo.MenuItemCheckbox, Checked: a.prefs.CaptionsTranslate, Accelerator: shownShortcut(translateShortcut),
			Click: click(a.ToggleTranslation)},
		{Label: "Translate To", Submenu: targets},
		mygo.Separator(),
	}
	if len(a.importing) > 0 {
		items = append(items, &mygo.MenuItem{Label: "Importing", Disabled: true})
		for _, b := range a.importing {
			id := b.ID
			items = append(items, &mygo.MenuItem{Label: b.Name + " — " + importingText(a.importer, b),
				Click: click(func() { a.OpenWindow(batchNotification + id) })})
		}
		items = append(items, mygo.Separator())
	}
	if len(a.recent) > 0 {
		items = append(items, &mygo.MenuItem{Label: "Recent", Disabled: true})
		for _, s := range a.recent {
			title, id := s.Title, s.ID
			if title == "" {
				title = untitled(s)
			}
			items = append(items, &mygo.MenuItem{Label: title, Click: click(func() { a.OpenWindow(id) })})
		}
		items = append(items, mygo.Separator())
	}
	return mygo.NewMenu(append(items,
		&mygo.MenuItem{Label: "Settings…", Accelerator: "Cmd+,", Click: click(a.OpenSettings)},
		mygo.Separator(),
		&mygo.MenuItem{Role: mygo.RoleQuit, Label: "Quit Capy"},
	))
}

// appMenu is the menu bar Capy shows while it is in the Dock. It leaves
// out mygo's default View menu, whose reload and developer tools are for
// web content.
func (a *App) appMenu() *mygo.Menu {
	return mygo.NewMenu([]*mygo.MenuItem{
		{Label: "Capy", Submenu: []*mygo.MenuItem{
			{Role: mygo.RoleAbout},
			mygo.Separator(),
			{Label: "Settings…", Accelerator: "Cmd+,", Click: func(*mygo.MenuItem, *mygo.Window) { a.OpenSettings() }},
			mygo.Separator(),
			{Role: mygo.RoleServices},
			mygo.Separator(),
			{Role: mygo.RoleHide},
			{Role: mygo.RoleHideOthers},
			{Role: mygo.RoleUnhide},
			mygo.Separator(),
			{Role: mygo.RoleQuit, Label: "Quit Capy"},
		}},
		{Label: "File", Submenu: []*mygo.MenuItem{
			{Label: "Record", Accelerator: captionsShortcut, Click: func(*mygo.MenuItem, *mygo.Window) { a.ToggleCaptions() }},
			{Label: "Import Audio…", Accelerator: "Cmd+O", Click: func(*mygo.MenuItem, *mygo.Window) { a.OpenImport() }},
			mygo.Separator(),
			{Role: mygo.RoleClose, Label: "Close Window", Accelerator: "Cmd+W"},
		}},
		{Role: mygo.RoleEditMenu},
		{Role: mygo.RoleWindowMenu},
	})
}

// dockPolicy puts Capy in the Dock, ⌘-Tab and the menu bar while its
// window is open. Otherwise, the caption bar alone included, it is the
// menu bar item only.
func (a *App) dockPolicy() mygo.ActivationPolicy {
	if a.window != nil {
		return mygo.ActivationPolicyRegular
	}
	return mygo.ActivationPolicyAccessory
}

// updateDock applies dockPolicy after a window opens or closes.
func (a *App) updateDock() { mygo.App.SetActivationPolicy(a.dockPolicy()) }

// recentCount is how many sessions the menu offers to open.
const recentCount = 3

// refreshMenu lists the latest ended sessions in the menu, and the
// batches being imported, off the main thread.
func (a *App) refreshMenu() {
	if a.tray == nil {
		return
	}
	a.menuSeq++
	seq, st, im := a.menuSeq, a.store, a.importer
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		list, err := st.ListSessions(ctx, store.ListQuery{Limit: recentCount + 1})
		if err != nil {
			a.log.Warn("list recent sessions", "err", err)
			return
		}
		list = slices.DeleteFunc(list, func(s store.SessionSummary) bool { return s.EndedAt.IsZero() })
		var importing []store.BatchSummary
		if im.Busy("") {
			batches, err := st.ListBatches(ctx)
			if err != nil {
				a.log.Warn("list batches", "err", err)
			}
			importing = slices.DeleteFunc(batches, func(b store.BatchSummary) bool { return !im.Busy(b.ID) })
		}
		mygo.RunOnMain(func() {
			if seq != a.menuSeq {
				return // a later refresh lists newer sessions
			}
			a.recent, a.importing = list[:min(len(list), recentCount)], importing
			a.tray.SetMenu(a.trayMenu())
		})
	}()
}

// importingText is how far batch b has come, as the menu says it.
func importingText(im *session.Importer, b store.BatchSummary) string {
	bp := progressOfBatch(im, b)
	if bp.pause != session.Running {
		return strings.ToLower(pauseText(bp.pause)[:1]) + pauseText(bp.pause)[1:]
	}
	return strconv.Itoa(bp.ready) + " of " + strconv.Itoa(bp.total) + " ready"
}

// captionsChanged updates the menu and Capy's window after a session
// starts or stops: the window lists the session being recorded.
func (a *App) captionsChanged() {
	a.holdImports()
	if a.tray != nil {
		a.tray.SetMenu(a.trayMenu())
	}
	a.refreshMenu()
	if a.window != nil {
		a.window.reload()
		a.window.win.Invalidate()
	}
}

// holdImports holds the files not sent yet while the recording
// reconnects: it needs the connection more.
func (a *App) holdImports() {
	a.importer.Hold(a.captions != nil && a.captions.snapshot().Status == session.StatusReconnecting)
}

// followRecording shows the session that just started recording in
// Capy's window, once: the user may then look at another.
func (a *App) followRecording() {
	if a.window == nil || a.captions == nil {
		return
	}
	if id := a.captions.snapshot().SessionID; a.sessionLive(id) {
		a.window.showRecording(id)
	}
}

// libraryChanged shows a session saved away from Capy's window, as an
// uploaded file's: the sidebar and the menu list it.
func (a *App) libraryChanged() {
	if a.window != nil {
		a.window.reload()
	}
	a.refreshMenu()
}

// windowChanged redraws Capy's window, which follows the session being
// recorded.
func (a *App) windowChanged() {
	if a.window != nil && a.window.win != nil {
		a.window.win.Invalidate()
	}
}

// confirmQuit asks before quitting while captions record or files are
// imported. The transcripts so far are saved either way; the files not
// transcribed are, at the next launch.
func (a *App) confirmQuit(e *mygo.QuitEvent) {
	state, _ := a.recordState()
	recording := state == recordLive || state == recordStarting
	if a.quitConfirmed || !recording && !a.importer.Busy("") {
		return
	}
	e.PreventDefault()
	opts := mygo.MessageOptions{Type: mygo.MessageQuestion, Message: "Stop recording and quit?",
		Detail: "The transcript so far is saved.", Buttons: []string{"Cancel", "Stop and Quit"}, DefaultButton: 1}
	if !recording {
		opts = a.quitImporting()
	}
	quit := len(opts.Buttons) - 1
	go func() {
		res, err := mygo.Dialog.Message(opts)
		if err != nil || res.Button != quit {
			return
		}
		mygo.RunOnMain(func() {
			a.quitConfirmed = true
			mygo.App.Quit()
		})
	}()
}

// quitImporting is the alert that asks before quitting while files are
// imported: Cancel is the default, as nothing is lost either way.
func (a *App) quitImporting() mygo.MessageOptions {
	message := "Quit while files are importing?"
	if len(a.importing) == 1 {
		message = "Quit while “" + a.importing[0].Name + "” is importing?"
	}
	detail := "Capy picks up where it left off the next time it opens."
	switch n := a.importer.Unsent(""); n {
	case 0:
	case 1:
		detail = "1 file hasn’t been uploaded yet. " + detail
	default:
		detail = strconv.Itoa(n) + " files haven’t been uploaded yet. " + detail
	}
	return mygo.MessageOptions{Type: mygo.MessageQuestion, Message: message, Detail: detail,
		Buttons: []string{"Cancel", "Quit"}, DefaultButton: 0}
}

// client returns a Zoom client for the stored API key.
func (a *App) client() (*zoomai.Client, error) {
	key, err := keychain.APIKey()
	if err != nil {
		return nil, err
	}
	return a.newClient(key)
}

// startSession connects a session in the background: reading the key,
// connecting and the permission prompts take time, and the main thread must
// keep drawing. started runs on win's main thread with the run or the
// error. The returned cancel abandons a start still in progress.
func (a *App) startSession(win *mygo.Window, cfg session.Config, onChange func(), started func(*session.Run, error)) (cancel func()) {
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	go func() {
		defer cancel()
		run, err := a.connect(ctx, cfg, onChange)
		win.Update(func() { started(run, err) })
		if err != nil {
			return
		}
		<-run.Done()
		snap := run.Snapshot()
		win.Update(a.captionsChanged)
		if snap.Status == session.StatusDone {
			a.notifyRecap(snap.SessionID)
		}
	}()
	return cancel
}

func (a *App) connect(ctx context.Context, cfg session.Config, onChange func()) (*session.Run, error) {
	ai, err := a.client()
	if err != nil {
		return nil, err
	}
	var src audio.Source = audio.NewMic()
	if cfg.Source == store.SourceSystem {
		// This Mac is the meeting in the headphones and the user's own voice.
		src = audio.Mix(audio.NewSystem(), audio.NewMic())
	}
	return session.Start(ctx, a.sessionDeps(ai), cfg, src, onChange)
}

// sessionDeps are a session's services, with ai: the add-on hears what
// Highlights saves.
func (a *App) sessionDeps(ai *zoomai.Client) session.Deps {
	return session.Deps{AI: ai, Store: a.store, Log: a.log, Highlighted: a.ext.Highlighted}
}

// notifyRecap tells the user a recap is ready; a click opens it.
func (a *App) notifyRecap(sessionID string) {
	n := mygo.NewNotification(mygo.NotificationOptions{
		ID:    sessionID,
		Title: "Recap ready",
		Body:  "Your Capy session has been summarized.",
	})
	if err := n.Show(); err != nil && !errors.Is(err, mygo.ErrNotificationsDenied) {
		a.log.Warn("notification", "err", err)
	}
}

// stopSessions stops capture of the running session.
func (a *App) stopSessions() {
	if a.captions != nil {
		a.captions.sessionSlot.stop()
	}
}

// ToggleCaptions starts captions in a new caption bar, or stops the running
// ones. Captions being finished are left to open in Capy's window.
func (a *App) ToggleCaptions() {
	if a.captions != nil {
		if a.captions.stopping {
			return
		}
		if a.captions.running() {
			a.captions.stop()
			a.captionsChanged()
			return
		}
		a.captions.close()
	}
	a.captions = newCaptionBar(a)
	a.captionsChanged()
}

// shownShortcut is a global shortcut for a menu to show, or "" when
// another app holds it.
func shownShortcut(acc string) string {
	if !mygo.GlobalShortcut.IsRegistered(acc) {
		return ""
	}
	return acc
}

// ToggleTranslation turns translation on or off, as ⌥⌘T does.
func (a *App) ToggleTranslation() { a.SetTranslate(!a.prefs.CaptionsTranslate) }

// SetTranslate turns translation on or off: the preference, which every
// control shows, and the captions being recorded, from their next turn.
func (a *App) SetTranslate(on bool) {
	a.prefs.CaptionsTranslate = on
	a.savePrefs()
	if a.captions != nil {
		a.captions.setTranslate(on)
	}
}

// setTarget chooses the language translations are in: the preference, and
// the captions being recorded, from their next turn.
func (a *App) setTarget(code string) {
	a.prefs.CaptionsTo = code
	a.savePrefs()
	if a.captions != nil {
		a.captions.setTarget(code)
	}
}

// OpenWindow shows Capy's window, selecting sessionID when it is not empty,
// or the batch a notification told of. The window stays open, captions or
// not, until the user closes it.
func (a *App) OpenWindow(sessionID string) {
	if a.window == nil {
		a.window = newMainWindow(a)
	}
	switch batch, isBatch := strings.CutPrefix(sessionID, batchNotification); {
	case sessionID == "":
	case isBatch:
		a.window.openBatch(batch)
	default:
		a.window.selectSession(sessionID)
	}
	a.window.reload()
	a.updateDock()
	a.window.win.Show()
	a.window.win.Focus()
	mygo.App.Focus()
}

// OpenImport shows Capy's window and asks for files to import.
func (a *App) OpenImport() {
	a.OpenWindow("")
	a.window.chooseFiles()
}

// OpenSettings shows Capy's window at Settings.
func (a *App) OpenSettings() {
	a.OpenWindow("")
	a.window.openSettings()
	a.windowChanged()
}

// savePrefs writes the preferences after a change, which every window and
// the menu show.
func (a *App) savePrefs() {
	if err := SavePrefs(a.prefsPath, a.prefs); err != nil {
		a.log.Warn("save preferences", "err", err)
	}
	a.highlightsChanged()
	if a.tray != nil {
		a.tray.SetMenu(a.trayMenu())
	}
	if a.captions != nil && a.captions.win != nil {
		a.captions.win.Invalidate()
	}
	a.windowChanged() // it shows what Record listens to
}

// saveProfile saves the user's profile after a change: Ask's agents read
// it from the file, and the recording marks the lines for them from its
// next check.
func (a *App) saveProfile(p profile.Profile) {
	saved, err := profile.Save(a.profilePath, p)
	if err != nil {
		a.log.Warn("save profile", "err", err)
		return
	}
	a.profile = saved
	a.highlightsChanged()
}

// highlightsOn reports whether this build's Highlights feature is on.
func (a *App) highlightsOn() bool { return a.gate.Enabled(features.Highlights) }

// highlightConfig is what Highlights marks in a recording, as Settings and
// the profile have it.
func (a *App) highlightConfig() session.HighlightConfig {
	p := a.prefs
	var kinds []store.HighlightKind
	for _, k := range []struct {
		on    bool
		kinds []store.HighlightKind
	}{
		{p.HighlightForYou, []store.HighlightKind{store.KindAskedYou, store.KindMentionedYou}},
		{p.HighlightDecisions, []store.HighlightKind{store.KindDecision}},
		{p.HighlightDebates, []store.HighlightKind{store.KindDebate}},
		{p.HighlightActions, []store.HighlightKind{store.KindAction}},
		{p.HighlightAnnouncements, []store.HighlightKind{store.KindAnnouncement}},
		{p.HighlightHeadsUps, []store.HighlightKind{store.KindHeadsUp}},
	} {
		if k.on {
			kinds = append(kinds, k.kinds...)
		}
	}
	threshold := session.ThresholdFewer
	if p.HighlightMore {
		threshold = session.ThresholdMore
	}
	return session.HighlightConfig{On: a.highlightsOn() && p.Highlights, Every: time.Duration(p.HighlightEvery) * time.Minute,
		Kinds: kinds, Threshold: threshold, Profile: a.profile}
}

// highlightsChanged hands the recording what Highlights marks, from its
// next check.
func (a *App) highlightsChanged() {
	if a.captions != nil && a.captions.run != nil && a.captions.running() {
		a.captions.run.SetHighlights(a.highlightConfig())
	}
}

// purgeExpired erases the sessions Recently Deleted has kept past
// store.TrashRetention, at launch and then every hour while Capy runs:
// Go's timers stop while the Mac sleeps, so a daily one could keep a
// session days past the date Recently Deleted gives.
func (a *App) purgeExpired() {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		n, err := a.purger.PurgeExpired(ctx, time.Now())
		cancel()
		if err != nil {
			a.log.Warn("purge expired sessions", "err", err)
		}
		if n > 0 || err != nil {
			mygo.RunOnMain(func() {
				if a.window != nil {
					a.window.reload()
				}
			})
		}
		time.Sleep(time.Hour)
	}
}

// redraw builds every window of Capy's anew.
func (a *App) redraw() {
	a.windowChanged()
	if a.captions != nil && a.captions.win != nil {
		a.captions.win.Invalidate()
	}
}

// openPane opens a System Settings pane, as Privacy & Security → Microphone.
func openPane(pane string) {
	go func() { _ = mygo.Shell.OpenExternal(pane) }()
}
