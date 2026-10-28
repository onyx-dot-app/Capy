package views

import (
	"context"
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestAddOnLibraryChangesRefreshTheWindow(t *testing.T) {
	a, w, q, sessions, _ := libraryWindow(t, library(t), "First")
	tt := ui.NewTester(w.view, 1040, 700)
	ctx := context.Background()
	edit, err := a.store.RenameSession(ctx, sessions[0].ID, "Renamed", "agent", "")
	if err != nil {
		t.Fatal(err)
	}
	w.reload() // The agent's original edit has already appeared in the window.
	q.settle(t)
	tt.Frame()
	if w.detail.Title != "Renamed" || w.sessions[0].Title != "Renamed" {
		t.Fatalf("the rename is not shown: detail %q, sidebar %q", w.detail.Title, w.sessions[0].Title)
	}
	version, err := a.store.DataVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.UndoEdit(ctx, edit.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := a.store.DataVersion(ctx); err != nil || got != version {
		t.Fatalf("same-handle Undo changed DataVersion %d to %d: %v", version, got, err)
	}
	host{a}.LibraryChanged()
	q.settle(t)
	tt.Frame()
	if w.detail.Title != "First" || w.sessions[0].Title != "First" {
		t.Fatalf("Undo was not shown: detail %q, sidebar %q", w.detail.Title, w.sessions[0].Title)
	}
	// An add-on may undo while Capy's window is closed too.
	a.window = nil
	host{a}.LibraryChanged()
}
