package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/store/storetest"

	"github.com/google/uuid"
)

func openAt(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db
}

func newSession() store.Session {
	return store.Session{
		ID: uuid.NewString(), Mode: store.ModeCaptions, Source: store.SourceMic,
		LangA: "en-US", LangB: "zh-CN", StartedAt: time.Now(),
	}
}

func TestContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		db := openAt(t, filepath.Join(t.TempDir(), "capy.db"))
		t.Cleanup(func() { _ = db.Close() })
		return db
	})
}

func TestReopenKeepsDataAndMigrationsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dir with space", "capy.db")
	sess := newSession()
	seg := store.Segment{ID: uuid.NewString(), SessionID: sess.ID, Lang: "en-US", Text: "persistent words"}

	db := openAt(t, path)
	if err := db.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendSegment(ctx, seg); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db = openAt(t, path)
	defer db.Close()
	d, err := db.GetSession(ctx, sess.ID)
	if err != nil || len(d.Segments) != 1 {
		t.Fatalf("after reopen: %+v, %v", d, err)
	}
	hits, err := db.Search(ctx, "persistent", 0)
	if err != nil || len(hits) != 1 {
		t.Fatalf("search after reopen: %v, %v", hits, err)
	}
	var version int
	if err := db.sql.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 10 {
		t.Fatalf("user_version = %d, %v", version, err)
	}
}

// TestUpgradeKeepsSegments opens a database of the first schema, as an
// earlier build left it: its segments read back, without a speaker.
func TestUpgradeKeepsSegments(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "capy.db")
	v1, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := migrationFS.ReadFile("migrations/001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	sess, segID := newSession(), uuid.NewString()
	for _, stmt := range []string{string(schema), "PRAGMA user_version = 1"} {
		if _, err := v1.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := v1.Exec(`INSERT INTO sessions (id, mode, source, lang_a, lang_b, started_at, created_at, updated_at)
VALUES (?, 'captions', 'mic', 'en-US', 'zh-CN', 1, 1, 1)`, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := v1.Exec(`INSERT INTO segments (id, session_id, seq, lang, text, start_ms, end_ms, created_at, updated_at)
VALUES (?, ?, 0, 'en-US', 'from before', 0, 900, 1, 1)`, segID, sess.ID); err != nil {
		t.Fatal(err)
	}
	_ = v1.Close()

	db := openAt(t, path)
	defer db.Close()
	d, err := db.GetSession(ctx, sess.ID)
	if err != nil || len(d.Segments) != 1 || d.Segments[0].Text != "from before" || d.Segments[0].Speaker != "" {
		t.Fatalf("after upgrade: %+v, %v", d.Segments, err)
	}
}

// TestUpgradeKeepsHighlights opens a database of schema 9, before polish:
// its highlights read back unpolished.
func TestUpgradeKeepsHighlights(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "capy.db")
	v9, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries[:9] {
		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v9.Exec(string(body)); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
	}
	sess, hID := newSession(), uuid.NewString()
	for _, stmt := range []string{
		"PRAGMA user_version = 9",
		`INSERT INTO sessions (id, mode, source, lang_a, lang_b, started_at, created_at, updated_at)
VALUES ('` + sess.ID + `', 'captions', 'mic', 'en-US', 'zh-CN', 1, 1, 1)`,
		`INSERT INTO highlights (id, session_id, seq, kind, confidence, created_at) VALUES ('` + hID + `', '` + sess.ID + `', 3, 'decision', 0.9, 1)`,
	} {
		if _, err := v9.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	_ = v9.Close()

	db := openAt(t, path)
	defer db.Close()
	d, err := db.GetSession(ctx, sess.ID)
	if err != nil || len(d.Highlights) != 1 || d.Highlights[0].ID != hID || d.Highlights[0].Polish != nil || !d.Highlights[0].PolishedAt.IsZero() {
		t.Fatalf("after upgrade: %+v, %v", d.Highlights, err)
	}
}

func TestOpenRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capy.db")
	db := openAt(t, path)
	if _, err := db.sql.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if db, err := Open(context.Background(), path); err == nil {
		_ = db.Close()
		t.Fatal("Open accepted a database from a newer build")
	}
}

func TestPrivateFilePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Capy")
	path := filepath.Join(dir, "capy.db")
	db := openAt(t, path)
	defer db.Close()

	for p, want := range map[string]os.FileMode{dir: 0o700, path: 0o600} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", p, got, want)
		}
	}
}

func TestPurgeErasesPrivateText(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "capy.db")
	db := openAt(t, path)
	defer db.Close()
	sess := newSession()
	// A file of a batch, whose name and path are the user's too.
	batch := store.Batch{ID: uuid.NewString(), Name: "Secret batch", Lang: "en-US", CreatedAt: time.Now()}
	bf := store.BatchFile{ID: uuid.NewString(), Path: "/Users/me/secret-folder/secret-file.m4a", Name: "secret-file.m4a", Size: 1}
	if err := db.CreateBatch(ctx, batch, []store.BatchFile{bf}); err != nil {
		t.Fatal(err)
	}
	sess.Source, sess.FileName, sess.BatchID, sess.TrackingID = store.SourceFile, "secret-file.m4a", batch.ID, "WEB_1"
	seg := store.Segment{ID: uuid.NewString(), SessionID: sess.ID, Lang: "en-US", Text: "confidential", Translation: "机密内容", TranslationLang: "zh-CN"}
	if err := db.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveFile(ctx, bf.ID, sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendSegment(ctx, seg); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRecap(ctx, sess.ID, store.Recap{Status: store.RecapReady, Title: "T", Text: "secret recap"}); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession(ctx, sess.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Highlights say which turns mattered to the user.
	if err := db.AddHighlights(ctx, []store.Highlight{{ID: uuid.NewString(), SessionID: sess.ID, Kind: store.KindAskedYou,
		Confidence: 0.9, CreatedAt: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	// Edits keep the values they replaced: those must go too.
	for _, edit := range []func() (store.Edit, error){
		func() (store.Edit, error) { return db.RenameSession(ctx, sess.ID, "secret title", "user", "") },
		func() (store.Edit, error) { return db.UpdateRecap(ctx, sess.ID, "edited recap", "agent", "run") },
		func() (store.Edit, error) { return db.TrashSession(ctx, sess.ID, "user", "") },
	} {
		if _, err := edit(); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.PurgeSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}

	var text, translation, title, custom, file, tracking, recap string
	var deleted sql.NullInt64
	err := db.sql.QueryRow(`SELECT g.text, g.translation, s.title, s.custom_title, s.file_name, s.tracking_id, s.recap, s.deleted_at
		FROM segments g JOIN sessions s ON s.id = g.session_id WHERE g.id = ?`, seg.ID).
		Scan(&text, &translation, &title, &custom, &file, &tracking, &recap, &deleted)
	if err != nil {
		t.Fatalf("tombstone row missing: %v", err)
	}
	if text != "" || translation != "" || title != "" || custom != "" || file != "" || tracking != "" || recap != "" || !deleted.Valid {
		t.Fatalf("purged row still holds data: %q %q %q %q %q %q %q deleted=%v", text, translation, title, custom, file, tracking,
			recap, deleted)
	}
	var edits int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM session_edits WHERE session_id = ?`, sess.ID).Scan(&edits); err != nil || edits != 0 {
		t.Fatalf("purged session keeps %d edits, %v", edits, err)
	}
	var highlights int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM highlights WHERE session_id = ?`, sess.ID).Scan(&highlights); err != nil || highlights != 0 {
		t.Fatalf("purged session keeps %d highlights, %v", highlights, err)
	}
	var files, batches int
	if err := db.sql.QueryRow(`SELECT (SELECT COUNT(*) FROM batch_files), (SELECT COUNT(*) FROM batches)`).Scan(&files, &batches); err != nil ||
		files != 0 || batches != 0 {
		t.Fatalf("purged session keeps %d files of %d batches, %v", files, batches, err)
	}
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM segments_fts WHERE segments_fts MATCH '"confidential"'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("FTS index still holds deleted text: n=%d err=%v", n, err)
	}
	// Nor may the bytes survive in freed pages or the write-ahead log, where
	// a copy of the file (a backup, strings capy.db) would reveal them.
	for _, f := range []string{path, path + "-wal"} {
		raw, err := os.ReadFile(f)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"confidential", "机密内容", "secret recap", "secret title", "edited recap", "secret-file",
			"Secret batch", "secret-folder"} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Errorf("%s still contains %q after delete", filepath.Base(f), secret)
			}
		}
	}
}

func TestUpdatesBumpUpdatedAt(t *testing.T) {
	ctx := context.Background()
	db := openAt(t, filepath.Join(t.TempDir(), "capy.db"))
	defer db.Close()
	sess := newSession()
	if err := db.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec("UPDATE sessions SET updated_at = 1 WHERE id = ?", sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession(ctx, sess.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	var updated int64
	if err := db.sql.QueryRow("SELECT updated_at FROM sessions WHERE id = ?", sess.ID).Scan(&updated); err != nil || updated <= 1 {
		t.Fatalf("updated_at = %d, %v", updated, err)
	}
}

func TestConcurrentAppendAndList(t *testing.T) {
	ctx := context.Background()
	db := openAt(t, filepath.Join(t.TempDir(), "capy.db"))
	defer db.Close()
	sess := newSession()
	if err := db.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}

	const writers, perWriter = 4, 50
	var wg sync.WaitGroup
	errs := make(chan error, writers*perWriter+writers*40)
	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				seg := store.Segment{ID: uuid.NewString(), SessionID: sess.ID, Seq: w*perWriter + i, Lang: "en-US", Text: "chunk"}
				if err := db.AppendSegment(ctx, seg); err != nil {
					errs <- err
				}
			}
		})
		wg.Go(func() {
			for range 20 {
				if _, err := db.ListSessions(ctx, store.ListQuery{}); err != nil {
					errs <- err
				}
				if _, err := db.Search(ctx, "chunk", 0); err != nil {
					errs <- err
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	d, err := db.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Segments) != writers*perWriter {
		t.Fatalf("stored %d segments, want %d", len(d.Segments), writers*perWriter)
	}
}

// TestDataVersionSeesAnotherProcesssWrites stands for Capy mcp, which
// edits the library from a process of its own.
func TestDataVersionSeesAnotherProcesssWrites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "capy.db")
	app, other := openAt(t, path), openAt(t, path)
	defer app.Close()
	defer other.Close()
	sess := newSession()
	if err := app.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	before, err := app.DataVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.EndSession(ctx, sess.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if v, err := app.DataVersion(ctx); err != nil || v != before {
		t.Fatalf("its own write moved the version %d → %d, %v", before, v, err)
	}
	if _, err := other.RenameSession(ctx, sess.ID, "Renamed elsewhere", "mcp", ""); err != nil {
		t.Fatal(err)
	}
	if v, err := app.DataVersion(ctx); err != nil || v == before {
		t.Fatalf("another handle's write left the version at %d, %v", v, err)
	}
	if d, err := app.GetSession(ctx, sess.ID); err != nil || d.Title != "Renamed elsewhere" {
		t.Fatalf("the other handle's rename: %q, %v", d.Title, err)
	}
}

// TestPurgeReportsTextLeftInTheLog keeps a reader open in another
// connection, as Capy mcp may, so that the checkpoint cannot empty the
// write-ahead log: the purge must say so rather than succeed.
func TestPurgeReportsTextLeftInTheLog(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "capy.db")
	db, reader := openAt(t, path), openAt(t, path)
	defer db.Close()
	defer reader.Close()
	sess := newSession()
	if err := db.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession(ctx, sess.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.TrashSession(ctx, sess.ID, "user", ""); err != nil {
		t.Fatal(err)
	}
	const marker = "private-transcript-checkpoint-retry"
	if err := db.SetRecap(ctx, sess.ID, store.Recap{Status: store.RecapReady, Text: marker}); err != nil {
		t.Fatal(err)
	}
	tx, err := reader.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := db.PurgeSession(ctx, sess.ID); !errors.Is(err, errCheckpointBusy) {
		t.Fatalf("purge with a reader open: %v, want errCheckpointBusy", err)
	}
	wal, err := os.ReadFile(path + "-wal")
	if err != nil || !bytes.Contains(wal, []byte(marker)) {
		t.Fatalf("blocked checkpoint did not retain the fixture text: %v", err)
	}
	_ = tx.Rollback()
	// The logical tombstone must remain available for an erase retry.
	pending, err := db.ListTrashed(ctx)
	if err != nil || len(pending) != 1 || !pending[0].PurgePending {
		t.Fatalf("pending purge hidden from Recently Deleted: %+v, %v", pending, err)
	}
	if err := db.PurgeSession(ctx, sess.ID); err != nil {
		t.Fatalf("retry purge: %v", err)
	}
	wal, err = os.ReadFile(path + "-wal")
	if err != nil || bytes.Contains(wal, []byte(marker)) {
		t.Fatalf("retry left private text in the log: %v", err)
	}
	if pending, err := db.ListExpired(ctx, time.Now()); err != nil || len(pending) != 0 {
		t.Fatalf("finished purge still pending: %v, %v", pending, err)
	}
}

// TestConcurrentOpensMigrateOnce opens one new file from several handles
// at once, as the app and Capy mcp may at first launch.
func TestConcurrentOpensMigrateOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capy.db")
	const n = 4
	dbs := make([]*DB, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { dbs[i], errs[i] = Open(context.Background(), path) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		defer dbs[i].Close()
	}
	var version int
	if err := dbs[0].sql.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 10 {
		t.Fatalf("user_version = %d, %v", version, err)
	}
	if err := dbs[1].CreateSession(context.Background(), newSession()); err != nil {
		t.Fatalf("write after concurrent opens: %v", err)
	}
}

func TestUndoTrashCannotRestoreAClaimedPurge(t *testing.T) {
	ctx := context.Background()
	db := openAt(t, filepath.Join(t.TempDir(), "capy.db"))
	defer db.Close()
	sess := newSession()
	if err := db.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := db.EndSession(ctx, sess.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	edit, err := db.TrashSession(ctx, sess.ID, "user", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ClaimPurge(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UndoEdit(ctx, edit.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Undo restored a partially erased session: %v", err)
	}
	if list, err := db.ListSessions(ctx, store.ListQuery{}); err != nil || len(list) != 0 {
		t.Fatalf("claimed session visible after Undo: %v, %v", list, err)
	}
}
