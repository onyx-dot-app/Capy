package views

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/store/sqlite"
)

// settle runs what the window's goroutines post until they go quiet.
func (q posted) settle(t *testing.T) {
	t.Helper()
	for {
		select {
		case fn := <-q:
			fn()
		case <-time.After(300 * time.Millisecond):
			return
		}
	}
}

// library is the path of a new library file.
func library(t *testing.T) string { return filepath.Join(t.TempDir(), "capy.db") }

// libraryWindow is Capy's window over a real library with the sessions
// seeded, loaded as at launch, and a purge hook that records what it
// erased.
func libraryWindow(t *testing.T, path string, titles ...string) (*App, *mainWindow, posted, []store.Session, *[]string) {
	t.Helper()
	a, w, q := storeApp(t, path)
	a.purger = session.NewPurger(a.store)
	var erased []string
	a.purger.OnPurge(func(_ context.Context, id string) error {
		erased = append(erased, id)
		return nil
	})
	var seeded []store.Session
	for _, title := range titles {
		seeded = append(seeded, seed(t, a.store, title, 2))
		time.Sleep(2 * time.Millisecond) // a start of its own, newest last
	}
	w.reload()
	q.settle(t)
	return a, w, q, seeded, &erased
}

func TestDeleteMovesToRecentlyDeletedAndUndoRestores(t *testing.T) {
	a, w, q, s, _ := libraryWindow(t, library(t), "First", "Second")
	tt := ui.NewTester(w.view, 1040, 700)
	if w.selected != s[1].ID {
		t.Fatalf("selected %q, want the newest", w.selected)
	}
	// ⌘⌫ asks nothing: the session moves to Recently Deleted, the next
	// older shows, and a toast offers Undo. Pressed twice before the first
	// is done, it does it once and reports no failure.
	tt.Key(ui.Cmd, ui.KeyBackspace)
	tt.Key(ui.Cmd, ui.KeyBackspace)
	q.settle(t)
	if w.notice != "" {
		t.Fatalf("a repeated ⌘⌫ reported %q", w.notice)
	}
	tt.Frame()
	snapshot(t, tt, "window-deleted-toast")
	if tt.HasText("Delete this session?") {
		t.Fatal("Delete asked for confirmation")
	}
	if w.selected != s[0].ID || !tt.HasText("Moved “Second” to Recently Deleted") || !tt.HasText("Undo") {
		t.Fatalf("after ⌘⌫: selected %q, texts %q; want First and the toast", w.selected, tt.Texts())
	}
	if trashed, err := a.store.ListTrashed(context.Background()); err != nil || len(trashed) != 1 || trashed[0].ID != s[1].ID {
		t.Fatalf("Recently Deleted holds %+v, %v; want Second", trashed, err)
	}
	if _, ok := tt.Find("Recently Deleted"); !ok {
		t.Fatal("no Recently Deleted row in the sidebar")
	}
	if err := tt.Click("Undo"); err != nil {
		t.Fatal(err)
	}
	q.settle(t)
	tt.Frame()
	if w.selected != s[1].ID {
		t.Fatalf("after Undo: selected %q, want Second back", w.selected)
	}
	if _, ok := tt.Find("Recently Deleted"); ok || len(w.trashed) != 0 {
		t.Fatalf("after Undo, Recently Deleted still shows %d sessions", len(w.trashed))
	}
}

func TestDeleteButtonNeedsASessionThatEnded(t *testing.T) {
	a, w, q, s, _ := libraryWindow(t, library(t), "First")
	tt := ui.NewTester(w.view, 1040, 700)
	if err := tt.Click("Delete"); err != nil {
		t.Fatal(err)
	}
	q.settle(t)
	if trashed, _ := a.store.ListTrashed(context.Background()); len(trashed) != 1 || trashed[0].ID != s[0].ID {
		t.Fatalf("Delete: Recently Deleted holds %+v", trashed)
	}
	// A session being recorded has no ⌘⌫.
	live := store.SessionSummary{ID: "s2", Mode: store.ModeCaptions, LangA: "en-US", LangB: "zh-CN", StartedAt: time.Now()}
	a.captions = &captionBar{app: a, cfg: captionsCfg, sessionSlot: sessionSlot{run: &fakeRun{snap: session.Snapshot{
		SessionID: "s2", Config: captionsCfg, Status: session.StatusLive, StartedAt: time.Now()}}}}
	w.sessions, w.selected, w.detail = []store.SessionSummary{live}, "s2", &store.SessionDetail{SessionSummary: live}
	tt.Frame()
	tt.Key(ui.Cmd, ui.KeyBackspace)
	select {
	case fn := <-q:
		fn()
		t.Fatal("⌘⌫ on the session being recorded tried to delete it")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestRecentlyDeletedRestoresAndDeletesNow(t *testing.T) {
	a, w, q, s, erased := libraryWindow(t, library(t), "First", "Second")
	for _, sess := range s {
		if _, err := a.store.TrashSession(context.Background(), sess.ID, "user", ""); err != nil {
			t.Fatal(err)
		}
	}
	w.reload()
	q.settle(t)
	tt := ui.NewTester(w.view, 1040, 700)
	if !tt.HasText("No Sessions") {
		t.Fatalf("every session trashed: texts %q", tt.Texts())
	}
	if err := tt.Click("Recently Deleted"); err != nil {
		t.Fatal(err)
	}
	snapshot(t, tt, "window-recently-deleted")
	want := trashText(time.Now(), time.Now())
	for _, text := range []string{"First", "Second", want, "Sessions are erased for good 30 days after they’re deleted."} {
		if !tt.HasText(text) {
			t.Errorf("Recently Deleted lacks %q; texts %q", text, tt.Texts())
		}
	}
	// Restore puts it back in its day group, and selects it.
	if err := tt.Click("Restore First"); err != nil {
		t.Fatal(err)
	}
	q.settle(t)
	tt.Frame()
	day, _ := dayGroup(s[0].StartedAt, time.Now())
	if w.selected != s[0].ID || w.trashOpen || !tt.HasText(day) {
		t.Fatalf("after Restore: selected %q, Recently Deleted open %v, texts %q", w.selected, w.trashOpen, tt.Texts())
	}
	// Delete Now asks, then erases the session and what else Capy keeps.
	if err := tt.Click("Recently Deleted"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("Delete Second Now"); err != nil {
		t.Fatal(err)
	}
	if !tt.HasText("Delete “Second” now?") || !tt.HasText("You can’t undo this.") {
		t.Fatalf("Delete Now asks nothing; texts %q", tt.Texts())
	}
	if err := tt.Click("Delete Now"); err != nil {
		t.Fatal(err)
	}
	q.settle(t)
	tt.Frame()
	if len(*erased) != 1 || (*erased)[0] != s[1].ID {
		t.Fatalf("purge hooks erased %v, want Second", *erased)
	}
	if _, err := a.store.GetSession(context.Background(), s[1].ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Second after Delete Now: %v", err)
	}
	if len(w.trashed) != 0 || !tt.HasText("No deleted sessions.") {
		t.Fatalf("Recently Deleted still lists %d sessions; texts %q", len(w.trashed), tt.Texts())
	}
}

func TestDeleteNowKeepsTheSessionWhenAHookFails(t *testing.T) {
	a, w, q, s, _ := libraryWindow(t, library(t), "First")
	a.purger = session.NewPurger(a.store)
	fail := true
	a.purger.OnPurge(func(context.Context, string) error {
		if fail {
			return errors.New("threads busy")
		}
		return nil
	})
	if _, err := a.store.TrashSession(context.Background(), s[0].ID, "user", ""); err != nil {
		t.Fatal(err)
	}
	w.reload()
	q.settle(t)
	tt := ui.NewTester(w.view, 1040, 700)
	w.openTrash()
	w.confirm, w.purging = false, ""
	tt.Frame()
	if err := tt.Click("Delete First Now"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("Delete Now"); err != nil {
		t.Fatal(err)
	}
	q.settle(t)
	tt.Frame()
	if !tt.HasText("Couldn’t delete the session.") || len(w.trashed) != 1 {
		t.Fatalf("a failed Delete Now: texts %q, %d in Recently Deleted; want the note and the session kept", tt.Texts(), len(w.trashed))
	}
	if !tt.HasText("Erasure pending · retry Delete Now") {
		t.Fatalf("pending erasure has no retry guidance: %q", tt.Texts())
	}
	notice := w.notice
	if err := tt.Click("Restore First"); err != nil {
		t.Fatal(err)
	}
	q.settle(t)
	if w.notice != notice || !w.trashOpen {
		t.Fatal("disabled Restore attempted to restore a pending erase")
	}
	// Delete Now retries the claimed erase once its hook can finish.
	fail = false
	if err := tt.Click("Delete First Now"); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click("Delete Now"); err != nil {
		t.Fatal(err)
	}
	q.settle(t)
	tt.Frame()
	if len(w.trashed) != 0 || !tt.HasText("No deleted sessions.") {
		t.Fatalf("retry did not finish erasure: %q", tt.Texts())
	}
}

func TestARenameByAnotherProcessShowsWithinASecond(t *testing.T) {
	path := library(t)
	_, w, q, s, _ := libraryWindow(t, path, "First")
	other, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.watch(ctx)
	tt := ui.NewTester(w.view, 1040, 700)
	// The first read of the version is the baseline: rename until a
	// reload follows.
	deadline := time.Now().Add(10 * time.Second)
	for i := 0; ; i++ {
		if _, err := other.RenameSession(context.Background(), s[0].ID, fmt.Sprintf("Renamed %d", i), "mcp", ""); err != nil {
			t.Fatal(err)
		}
		select {
		case fn := <-q:
			fn()
		case <-time.After(1500 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("no reload after renames by another handle")
			}
			continue
		}
		break
	}
	q.settle(t)
	tt.Frame()
	if !tt.HasText("Renamed ") {
		t.Fatalf("the sidebar does not show the rename; texts %q", tt.Texts())
	}
}

func TestTrashTextSaysWhenItWasDeletedAndWillBeErased(t *testing.T) {
	now := time.Date(2026, 10, 12, 15, 0, 0, 0, time.Local)
	for at, want := range map[time.Time]string{
		time.Date(2026, 10, 12, 9, 0, 0, 0, time.Local): "Deleted today · erased on Nov 11",
		time.Date(2026, 10, 11, 9, 0, 0, 0, time.Local): "Deleted yesterday · erased on Nov 10",
		time.Date(2026, 10, 3, 9, 0, 0, 0, time.Local):  "Deleted Oct 3 · erased on Nov 2",
	} {
		if got := trashText(at, now); got != want {
			t.Errorf("trashText(%v) = %q, want %q", at, got, want)
		}
	}
}
