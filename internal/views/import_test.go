package views

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/fakezoom"
	"github.com/onyx-dot-app/Capy/internal/keychain"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

// wavFile writes seconds of 8 kHz mono PCM16 silence to dir/name, a file
// whose length its metadata gives, and returns its path.
func wavFile(t *testing.T, dir, name string, seconds int) string {
	t.Helper()
	data := make([]byte, seconds*8000*2)
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(data)))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(8000), uint32(16000), uint16(2), uint16(16)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// importWindow is Capy's window over a real library and a fake Zoom
// following script, the app's work reported through q, and the
// notifications it showed, "title: body".
func importWindow(t *testing.T, script fakezoom.Script) (*App, *mainWindow, posted, *fakezoom.Server, *[]string) {
	t.Helper()
	script.APIKey = "key"
	fake := fakezoom.New(script)
	t.Cleanup(fake.Close)
	t.Setenv(keychain.EnvKey, "key")
	a, w, q := storeApp(t, library(t))
	a.newClient = func(key string) (*zoomai.Client, error) { return zoomai.NewClient(key, zoomai.WithBaseURL(fake.URL)) }
	var notes []string
	a.onMain, a.notify = q.post, func(_, title, body string) { notes = append(notes, title+": "+body) }
	a.importer = a.newImporter()
	t.Cleanup(a.importer.Stop)
	w.reload()
	q.settle(t)
	return a, w, q, fake, &notes
}

// waitImport runs what the importer posts until it is idle, and what that
// posted.
func waitImport(t *testing.T, a *App, q posted) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.importer.Wait(ctx) }()
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("the import never finished: %v", err)
			}
			q.settle(t)
			return
		case fn := <-q:
			fn()
		}
	}
}

// until runs what is posted until cond holds.
func until(t *testing.T, q posted, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); {
		select {
		case fn := <-q:
			fn()
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
	}
}

// interviews are six files named as the design's, of known lengths.
func interviews(t *testing.T, dir string) []string {
	t.Helper()
	var paths []string
	for i, name := range []string{"interview-acme-0921.wav", "interview-birch-0922.wav", "interview-cobalt-0923.wav",
		"interview-delta-0926.wav", "interview-ember-0929.wav", "interview-fjord-1002.wav"} {
		paths = append(paths, wavFile(t, dir, name, 61+i*30))
	}
	return paths
}

// confirm adds paths as dropped, then confirms the sheet named name.
func confirm(t *testing.T, tt *ui.Tester, w *mainWindow, q posted, paths []string, name string) {
	t.Helper()
	w.addFiles(paths)
	until(t, q, "the sheet", func() bool { return w.sheet != nil && len(w.sheet.files) > 0 })
	tt.Frame()
	if name != "" {
		w.sheet.name = name
	}
	if err := tt.Click("Transcribe " + filesText(len(w.sheet.files))); err != nil {
		t.Fatalf("%v; texts %q", err, tt.Texts())
	}
	until(t, q, "the import started", func() bool { return w.sheet == nil })
	tt.Frame()
}

func TestImportConfirmsTheFilesInASheet(t *testing.T) {
	a, w, q, fake, notes := importWindow(t, fakezoom.Script{})
	dir := t.TempDir()
	paths := interviews(t, dir)[:3]
	tt := ui.NewTester(w.view, 1040, 700)
	if err := os.WriteFile(filepath.Join(dir, "notes.pdf"), []byte("%PDF"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.addFiles(append(slices.Clone(paths), filepath.Join(dir, "notes.pdf")))
	until(t, q, "the sheet", func() bool { return w.sheet != nil && len(w.sheet.files) == 3 && len(w.sheet.skipped) == 1 })
	tt.Frame()
	snapshot(t, tt, "import-sheet")
	for _, want := range []string{"Transcribe 3 audio files", "5 min in total", "Name", "interview-acme-0921.wav", "01:01",
		"interview-cobalt-0923.wav", "02:01", "notes.pdf isn’t audio and was left out.", "Spoken language", "English",
		"Translate to", "None", "This takes a while. You’ll get a notification when all 3 are ready.", "Cancel", "Transcribe 3 Files"} {
		if !tt.HasText(want) {
			t.Errorf("sheet lacks %q; texts %q", want, tt.Texts())
		}
	}
	if w.sheet.name != "interview" {
		t.Errorf("name %q, want the start the files share", w.sheet.name)
	}
	if err := tt.Click("Language to translate the files to"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("Français"); err != nil {
		t.Fatalf("%v; menu %q", err, tt.Menu())
	}
	w.sheet.name = "Customer interviews"
	tt.Frame()
	if err := tt.Click("Transcribe 3 Files"); err != nil {
		t.Fatal(err)
	}
	waitImport(t, a, q)
	tt.Frame()
	snapshot(t, tt, "batch-done")

	// The languages are remembered for the next import.
	if p := LoadPrefs(a.prefsPath); p.ImportLanguage != "en-US" || p.ImportTarget != "fr-FR" {
		t.Errorf("prefs %+v", p)
	}
	if calls := fake.SpeechCalls(); len(calls) != 3 {
		t.Errorf("speech calls %q", calls)
	}
	// The batch's page shows, as the sidebar lists it.
	if w.batch == "" || w.sheet != nil {
		t.Fatalf("batch %q, sheet %+v", w.batch, w.sheet)
	}
	for _, want := range []string{"Customer interviews", "3 files", "5 min", "3 of 3 ready", "1 min · interview-acme-0921.wav was discussed",
		"Copy All as Markdown", "Export All…"} {
		if !tt.HasText(want) {
			t.Errorf("batch page lacks %q; texts %q", want, tt.Texts())
		}
	}
	if !slices.ContainsFunc(tt.Texts(), func(s string) bool { return strings.HasSuffix(s, " · 3 sessions") }) {
		t.Errorf("sidebar lacks the batch's when · 3 sessions; texts %q", tt.Texts())
	}
	if !slices.Equal(*notes, []string{"Customer interviews is ready: 3 sessions transcribed and summarized."}) {
		t.Errorf("notifications %q", *notes)
	}
	// Each file is a session, translated.
	b, err := a.store.GetBatch(context.Background(), w.batch)
	if err != nil {
		t.Fatal(err)
	}
	d, err := a.store.GetSession(context.Background(), b.Files[0].SessionID)
	if err != nil || d.LangB != "fr-FR" || d.Segments[0].Translation == "" {
		t.Fatalf("session %+v, %v", d.SessionSummary, err)
	}
	// A file opens as its session, under its batch's crumb.
	if err := tt.Click("interview-birch-0922"); err != nil {
		t.Fatal(err)
	}
	q.settle(t)
	tt.Frame()
	snapshot(t, tt, "batch-file-session")
	if w.selected != b.Files[1].SessionID || !tt.HasText("Customer interviews") || !tt.HasText("Imported today at "+
		b.CreatedAt.Local().Format("15:04")) {
		t.Fatalf("file session: selected %q; texts %q", w.selected, tt.Texts())
	}
	// The crumb goes back to the batch.
	if err := tt.Click("Show Customer interviews"); err != nil {
		t.Fatal(err)
	}
	q.settle(t)
	if w.batch != b.ID {
		t.Fatalf("crumb opened %q", w.batch)
	}
}

func TestBatchShowsEachFileWhereItIs(t *testing.T) {
	a, w, q, fake, notes := importWindow(t, fakezoom.Script{HoldSummary: "cobalt", HoldTranscript: "delta"})
	tt := ui.NewTester(w.view, 1040, 700)
	confirm(t, tt, w, q, interviews(t, t.TempDir()), "Customer interviews")
	until(t, q, "two files ready", func() bool {
		b, _ := findBatch(w.batches, w.batch)
		return progressOfBatch(a.importer, b).ready == 2 && len(fake.SpeechCalls()) == 4
	})
	until(t, q, "cobalt's transcript saved", func() bool {
		b, _ := findBatch(w.batches, w.batch)
		return len(b.Files) == 6 && b.Files[2].SessionID != ""
	})
	tt.Frame()
	snapshot(t, tt, "batch-running")
	for _, want := range []string{"Customer interviews", "2 of 6 ready", "Cancel Remaining",
		"Capy will notify you when all 6 are ready. You can close this window; Capy keeps working from the menu bar.",
		"2 min · Writing the recap… The transcript is ready.", "3 min · Transcribing…", "Waiting", "Writing recap…", "Transcribing…"} {
		if !tt.HasText(want) {
			t.Errorf("batch under way lacks %q; texts %q", want, tt.Texts())
		}
	}
	if len(*notes) != 0 {
		t.Errorf("notified %q while under way", *notes)
	}
	// A file not saved opens to its steps.
	if err := tt.Click("interview-delta-0926"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	snapshot(t, tt, "file-transcribing")
	for _, want := range []string{"Customer interviews", "interview-delta-0926", "Imported today at", "2 min 31 sec", "Summary",
		"Transcript", "Uploaded", "Transcribing 3 min of audio…", "Write the recap", "You’ll get a notification when "} {
		if !slices.ContainsFunc(tt.Texts(), func(s string) bool { return strings.HasPrefix(s, want) }) {
			t.Errorf("file page lacks %q; texts %q", want, tt.Texts())
		}
	}
	fake.Release()
	waitImport(t, a, q)
	tt.Frame()
	// Once saved, the file shows as its session.
	if w.file != "" || w.selected == "" {
		t.Fatalf("file %q, selected %q", w.file, w.selected)
	}
	if !slices.Equal(*notes, []string{"Customer interviews is ready: 6 sessions transcribed and summarized."}) {
		t.Errorf("notifications %q", *notes)
	}
}

func TestBatchFinishedWithAProblemOffersRetry(t *testing.T) {
	a, w, q, fake, notes := importWindow(t, fakezoom.Script{FailSpeech: "delta"})
	tt := ui.NewTester(w.view, 1040, 700)
	confirm(t, tt, w, q, interviews(t, t.TempDir()), "Customer interviews")
	waitImport(t, a, q)
	tt.Frame()
	snapshot(t, tt, "batch-failed")
	for _, want := range []string{"5 of 6 ready", "· 1 couldn’t be transcribed", "3 min · Zoom AI Services couldn’t transcribe this file.",
		"Retry", "5 ready · 1 failed", "Couldn’t transcribe"} {
		if !tt.HasText(want) {
			t.Errorf("batch lacks %q; texts %q", want, tt.Texts())
		}
	}
	if !slices.Equal(*notes, []string{"Customer interviews finished with a problem: 5 of 6 are ready. interview-delta-0926 couldn’t be transcribed."}) {
		t.Errorf("notifications %q", *notes)
	}
	if err := tt.Click("Retry"); err != nil {
		t.Fatal(err)
	}
	until(t, q, "the file sent again", func() bool { return len(fake.SpeechCalls()) == 7 })
	waitImport(t, a, q)
	if b, _ := findBatch(w.batches, w.batch); b.Files[3].State != store.FileFailed {
		t.Fatalf("after Retry: %+v", b.Files[3])
	}
}

func TestBatchMenusActOnTheBatchAndItsFiles(t *testing.T) {
	a, w, q, _, _ := importWindow(t, fakezoom.Script{})
	ext := &stubAddOn{}
	a.ext = ext
	tt := ui.NewTester(w.view, 1040, 800)
	confirm(t, tt, w, q, interviews(t, t.TempDir())[:3], "Customer interviews")
	waitImport(t, a, q)
	tt.Frame()
	ctx := context.Background()
	b, _ := findBatch(w.batches, w.batch)

	if err := tt.RightClick("Customer interviews"); err != nil {
		t.Fatal(err)
	}
	want := []string{"Rename", "slot:batch Customer interviews", "-", "Copy All as Markdown", "Export All…", "-",
		"Retry Failed", "Cancel Remaining", "-", "Ungroup", "Delete Batch"}
	if menu := tt.Menu(); !slices.Equal(menu, want) {
		t.Fatalf("batch menu %q, want %q", menu, want)
	}
	// The add-on's item hears the batch, with its sessions in order.
	if err := tt.ChooseMenuItem("slot:batch Customer interviews"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if len(ext.batchChosen) != 1 || len(ext.batchChosen[0].SessionIDs) != 3 || ext.batchChosen[0].SessionIDs[0] != b.Files[0].SessionID {
		t.Fatalf("add-on heard %+v", ext.batchChosen)
	}
	// Copy All as Markdown: the batch's name, each session a level down.
	if err := tt.RightClick("Customer interviews"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("Copy All as Markdown"); err != nil {
		t.Fatal(err)
	}
	until(t, q, "the Markdown copied", func() bool { tt.Frame(); return tt.Clipboard() != "" })
	md := tt.Clipboard()
	if !strings.HasPrefix(md, "# Customer interviews\n\n## interview-acme-0921\n") || strings.Count(md, "\n## interview-") != 3 ||
		!strings.Contains(md, "### Transcript") {
		t.Fatalf("Markdown:\n%s", md)
	}
	// Rename.
	if err := tt.RightClick("Customer interviews"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("Rename"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	snapshot(t, tt, "batch-rename")
	w.renaming.name = "Interviews, fall"
	if err := tt.Click("Rename"); err != nil {
		t.Fatal(err)
	}
	until(t, q, "renamed", func() bool { got, _ := findBatch(w.batches, b.ID); return got.Name == "Interviews, fall" })

	// A file's menu: Move Out of Batch leaves it a session on its own.
	if err := tt.RightClick("interview-birch-0922"); err != nil {
		t.Fatal(err)
	}
	want = []string{"Rename", "Session Info", "-", "Copy as Markdown", "Export…", "-", "Cancel Transcription", "Retry", "-",
		"Move Out of Batch", "Delete"}
	if menu := tt.Menu(); !slices.Equal(menu, want) {
		t.Fatalf("file menu %q, want %q", menu, want)
	}
	if err := tt.ChooseMenuItem("Move Out of Batch"); err != nil {
		t.Fatal(err)
	}
	until(t, q, "moved out", func() bool { got, _ := findBatch(w.batches, b.ID); return len(got.Files) == 2 })
	if alone, _ := a.store.ListSessions(ctx, store.ListQuery{Unbatched: true}); len(alone) != 1 || alone[0].ID != b.Files[1].SessionID {
		t.Fatalf("on their own: %+v", alone)
	}
	// Delete Batch asks, then moves its sessions to Recently Deleted.
	if err := tt.RightClick("Interviews, fall"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("Delete Batch"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !tt.HasText("Delete “Interviews, fall”?") || !tt.HasText("Its 2 sessions move to Recently Deleted. Files not transcribed yet are canceled.") {
		t.Fatalf("alert: texts %q", tt.Texts())
	}
	if err := tt.Click("Delete"); err != nil {
		t.Fatal(err)
	}
	until(t, q, "deleted", func() bool { return len(w.batches) == 0 })
	if trashed, _ := a.store.ListTrashed(ctx); len(trashed) != 2 {
		t.Fatalf("Recently Deleted holds %d sessions; want 2", len(trashed))
	}
}

func TestUngroupLeavesTheSessionsOnTheirOwn(t *testing.T) {
	a, w, q, _, _ := importWindow(t, fakezoom.Script{})
	tt := ui.NewTester(w.view, 1040, 700)
	confirm(t, tt, w, q, interviews(t, t.TempDir())[:2], "Pair")
	waitImport(t, a, q)
	tt.Frame()
	if err := tt.RightClick("Pair"); err != nil {
		t.Fatal(err)
	}
	if err := tt.ChooseMenuItem("Ungroup"); err != nil {
		t.Fatal(err)
	}
	until(t, q, "ungrouped", func() bool { return len(w.batches) == 0 && len(w.sessions) == 2 })
	tt.Frame()
	if !tt.HasText("interview-acme-0921") || tt.HasText("Pair") {
		t.Fatalf("texts %q", tt.Texts())
	}
}

func TestDroppingFilesShowsTheOverlayAndTurnsAwayWhatIsNotAudio(t *testing.T) {
	tt := ui.NewTester(func(c *ui.Context) {
		ui.Column(c).Fill().Children(func() { dropOverlay(c) })
	}, 1040, 700)
	snapshot(t, tt, "import-drop")
	for _, want := range []string{"Drop to transcribe audio files", "Each file becomes a session with its own transcript and recap."} {
		if !tt.HasText(want) {
			t.Errorf("overlay lacks %q; texts %q", want, tt.Texts())
		}
	}
	_, w, q, _, _ := importWindow(t, fakezoom.Script{})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.pdf"), []byte("%PDF"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.addFiles([]string{filepath.Join(dir, "notes.pdf"), dir})
	until(t, q, "a toast", func() bool { return len(w.toasts) == 1 })
	if w.sheet != nil || w.toasts[0].Title != "notes.pdf isn’t audio. "+filepath.Base(dir)+" is a folder." {
		t.Fatalf("sheet %+v, toast %q", w.sheet, w.toasts[0].Title)
	}
}

func TestReadFilesTurnsAwayWhatZoomDoesNotTake(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, size int64) string {
		path := filepath.Join(dir, name)
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := f.Truncate(size); err != nil { // sparse: a large file costs nothing
			t.Fatal(err)
		}
		return path
	}
	if err := os.Mkdir(filepath.Join(dir, "Recordings"), 0o700); err != nil {
		t.Fatal(err)
	}
	added, skipped := readFiles([]string{
		wavFile(t, dir, "Weekly sync.wav", 90), write("notes.txt", 10), write("silence.wav", 0),
		write("conference.mov", zoomai.MaxSpeechFileBytes+1), filepath.Join(dir, "Recordings"), filepath.Join(dir, "gone.mp3"),
		write("two\nlines.wav", 10),
	})
	if len(added) != 1 || added[0].Name != "Weekly sync.wav" || added[0].Duration != 90*time.Second || !filepath.IsAbs(added[0].Path) {
		t.Fatalf("added %+v", added)
	}
	got := skippedText(skipped, " and was left out")
	want := "notes.txt isn’t audio and was left out. silence.wav is empty and was left out. 5 other files and was left out."
	if len(skipped) != 6 || got != want && !strings.HasPrefix(got, "notes.txt isn’t audio and was left out. silence.wav is empty") {
		t.Fatalf("skipped %+v: %q", skipped, got)
	}
}

func TestRowsSayWhereAFileIs(t *testing.T) {
	created := time.Date(2026, 10, 20, 9, 58, 0, 0, time.Local)
	b := store.BatchSummary{Batch: store.Batch{Name: "Customer interviews", CreatedAt: created}}
	recorded := time.Date(2026, 9, 21, 15, 40, 0, 0, time.Local)
	saved := store.BatchFile{State: store.FileSaved, SessionID: "s", RecordedAt: recorded, Recap: "## Acme\n\n- Acme wants **SSO** before rollout.",
		Session: store.SessionSummary{ID: "s", StartedAt: recorded, EndedAt: recorded.Add(34*time.Minute + 12*time.Second),
			RecapStatus: store.RecapReady, SegmentCount: 3}}
	waiting := store.BatchFile{State: store.FileWaiting, Duration: 37 * time.Minute}
	failed := store.BatchFile{State: store.FileFailed, Problem: session.ProblemTranscribe, Duration: 22 * time.Minute}
	for _, c := range []struct {
		f         store.BatchFile
		p         fileProgress
		row, line string
	}{
		{saved, fileProgress{state: fileReady}, "Sep 21 · 34 min", "34 min · Acme wants SSO before rollout."},
		{waiting, fileProgress{state: fileWaiting}, "Waiting", "37 min · Waiting"},
		{waiting, fileProgress{state: fileUploading, sent: 0.42}, "Uploading · 42%", "37 min · Uploading… 42%"},
		{waiting, fileProgress{state: fileTranscribing}, "Transcribing…", "37 min · Transcribing…"},
		{saved, fileProgress{state: fileSummarizing}, "Writing recap…", "34 min · Writing the recap… The transcript is ready."},
		{failed, fileProgress{state: fileFailed}, "Couldn’t transcribe", "22 min · Zoom AI Services couldn’t transcribe this file."},
	} {
		if got := fileSubtitle(b, c.f, c.p); got != c.row {
			t.Errorf("row %q, want %q", got, c.row)
		}
		if got := fileLine(c.f, c.p); got != c.line {
			t.Errorf("line %q, want %q", got, c.line)
		}
	}
	for _, c := range []struct {
		bp    batchProgress
		older bool
		want  string
	}{
		{batchProgress{busy: true, ready: 2, total: 6}, false, "2 of 6 ready"},
		{batchProgress{busy: true, ready: 2, total: 6, pause: session.PausedOffline}, false, "Paused · waiting for network"},
		{batchProgress{busy: true, ready: 2, total: 6, pause: session.PausedRecording}, false, "Paused while recording reconnects"},
		{batchProgress{ready: 6, total: 6}, false, "09:58 · 6 sessions"},
		{batchProgress{ready: 6, total: 6}, true, "Oct 20 · 6 sessions"},
		{batchProgress{ready: 5, failed: 1, total: 6}, false, "5 ready · 1 failed"},
	} {
		if got := batchSubtitle(b, c.bp, c.older); got != c.want {
			t.Errorf("batch subtitle %q, want %q", got, c.want)
		}
	}
}

func TestQuitWhileImportingAsksAndSaysWhatIsLeft(t *testing.T) {
	a, w, q, fake, _ := importWindow(t, fakezoom.Script{HoldTranscript: "interview"})
	a.importer = session.NewImporter(session.ImporterOptions{Store: a.store, Client: a.client, Parallel: 1})
	t.Cleanup(a.importer.Stop)
	tt := ui.NewTester(w.view, 1040, 700)
	confirm(t, tt, w, q, interviews(t, t.TempDir())[:3], "Customer interviews")
	until(t, q, "the first file sent", func() bool { return len(fake.SpeechCalls()) == 1 && len(w.batches) == 1 })
	a.importing = w.batches
	opts := a.quitImporting()
	if opts.Message != "Quit while “Customer interviews” is importing?" ||
		opts.Detail != "2 files haven’t been uploaded yet. Capy picks up where it left off the next time it opens." ||
		!slices.Equal(opts.Buttons, []string{"Cancel", "Quit"}) || opts.DefaultButton != 0 {
		t.Fatalf("alert %+v", opts)
	}
}

func TestMenusOfferImportAndListTheBatchesUnderWay(t *testing.T) {
	a, w, q, fake, _ := importWindow(t, fakezoom.Script{HoldTranscript: "interview"})
	tt := ui.NewTester(w.view, 1040, 700)
	confirm(t, tt, w, q, interviews(t, t.TempDir())[:3], "Customer interviews")
	until(t, q, "two files sent", func() bool { return len(fake.SpeechCalls()) == 2 && len(w.batches) == 1 })
	a.importing = w.batches
	var labels []string
	for _, it := range a.trayMenu().Items() {
		labels = append(labels, it.Label)
	}
	i := slices.Index(labels, "Importing")
	if !slices.Contains(labels, "Import Audio…") || i < 0 || labels[i+1] != "Customer interviews — 0 of 3 ready" {
		t.Fatalf("menu bar menu %q", labels)
	}
	// The menu bar's File menu.
	var file []string
	for _, it := range a.appMenu().Items() {
		if it.Label == "File" {
			for _, sub := range it.Submenu {
				file = append(file, sub.Label+" "+sub.Accelerator)
			}
		}
	}
	if !slices.Equal(file, []string{"Record Alt+Cmd+C", "Import Audio… Cmd+O", " ", "Close Window Cmd+W"}) {
		t.Fatalf("File menu %q", file)
	}
	fake.Release()
	waitImport(t, a, q)
}

func TestImportsWaitWhileTheRecordingReconnects(t *testing.T) {
	a, w, q, fake, _ := importWindow(t, fakezoom.Script{})
	run := &fakeRun{snap: session.Snapshot{SessionID: "s1", Config: captionsCfg, Status: session.StatusReconnecting, StartedAt: time.Now()}}
	a.captions = &captionBar{app: a, cfg: captionsCfg, sessionSlot: sessionSlot{run: run}}
	a.holdImports()
	tt := ui.NewTester(w.view, 1040, 700)
	confirm(t, tt, w, q, interviews(t, t.TempDir())[:2], "Customer interviews")
	until(t, q, "the batch listed", func() bool { return len(w.batches) == 1 })
	tt.Frame()
	snapshot(t, tt, "batch-paused")
	if !tt.HasText("Paused while recording reconnects") || len(fake.SpeechCalls()) != 0 {
		t.Fatalf("held: %d files sent; texts %q", len(fake.SpeechCalls()), tt.Texts())
	}
	run.snap.Status = session.StatusLive
	a.holdImports()
	waitImport(t, a, q)
	if b, _ := findBatch(w.batches, w.batch); progressOfBatch(a.importer, b).ready != 2 {
		t.Fatalf("after the recording reconnected: %+v", b.Files)
	}
}
