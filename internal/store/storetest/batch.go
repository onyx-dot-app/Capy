package storetest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/store"
)

// batchCases are the contract of batches, run by Run.
var batchCases = []struct {
	name string
	fn   func(t *testing.T, s store.Store)
}{
	{"BatchesListedWithTheirFiles", batchesListed},
	{"SavedFileIsItsSession", savedFile},
	{"FileStates", fileStates},
	{"TrashedFilesLeftOut", trashedFilesLeftOut},
	{"RemoveFileKeepsItsSession", removeFile},
	{"UngroupKeepsTheSessions", ungroup},
	{"DeleteBatchTrashesItsSessions", deleteBatch},
	{"PurgeForgetsTheFile", purgeForgetsFile},
	{"BatchValidation", batchValidation},
}

// newBatch stores a batch created at at of files named names, the second
// recorded an hour before at and 90 seconds long, and returns it.
func newBatch(t *testing.T, s store.Store, at time.Time, names ...string) store.BatchSummary {
	t.Helper()
	b := store.Batch{ID: uuid.NewString(), Name: "Customer interviews", Lang: "en-US", Target: "zh-CN", CreatedAt: at}
	files := make([]store.BatchFile, len(names))
	for i, name := range names {
		files[i] = store.BatchFile{ID: uuid.NewString(), Path: "/Users/me/Recordings/" + name, Name: name, Size: int64(1000 * (i + 1))}
		if i == 1 {
			files[i].RecordedAt, files[i].Duration = at.Add(-time.Hour), 90*time.Second
		}
	}
	if err := s.CreateBatch(context.Background(), b, files); err != nil {
		t.Fatalf("CreateBatch: %v", err)
	}
	for i := range files {
		files[i].BatchID, files[i].Seq, files[i].State = b.ID, i, store.FileWaiting
	}
	return store.BatchSummary{Batch: b, Files: files}
}

// saveFile saves file f of b as an ended session, as the importer does,
// with recap text, and returns the session.
func saveFile(t *testing.T, s store.Store, b store.BatchSummary, f store.BatchFile, recap string) store.Session {
	t.Helper()
	ctx := context.Background()
	sess := store.Session{ID: uuid.NewString(), Mode: store.ModeCaptions, Source: store.SourceFile, LangA: b.Lang, LangB: b.Target,
		StartedAt: b.CreatedAt, FileName: f.Name}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := s.EndSession(ctx, sess.ID, b.CreatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRecap(ctx, sess.ID, store.Recap{Status: store.RecapReady, Title: "From the file", Text: recap}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveFile(ctx, f.ID, sess.ID); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	return sess
}

func listBatches(t *testing.T, s store.Store) []store.BatchSummary {
	t.Helper()
	list, err := s.ListBatches(context.Background())
	if err != nil {
		t.Fatalf("ListBatches: %v", err)
	}
	return list
}

func batchesListed(t *testing.T, s store.Store) {
	older := newBatch(t, s, base, "a.m4a")
	b := newBatch(t, s, base.Add(time.Hour), "interview-acme.m4a", "interview-birch.mp3", "週次.wav")
	list := listBatches(t, s)
	if len(list) != 2 || list[0].ID != b.ID || list[1].ID != older.ID {
		t.Fatalf("ListBatches = %+v; want the newest first", list)
	}
	got := list[0]
	if got.Name != b.Name || got.Lang != "en-US" || got.Target != "zh-CN" || !got.CreatedAt.Equal(b.CreatedAt) {
		t.Fatalf("batch = %+v", got.Batch)
	}
	if len(got.Files) != 3 {
		t.Fatalf("files = %+v", got.Files)
	}
	for i, f := range got.Files {
		want := b.Files[i]
		if f.ID != want.ID || f.BatchID != b.ID || f.Seq != i || f.Name != want.Name || f.Path != want.Path ||
			f.Size != want.Size || f.State != store.FileWaiting || f.SessionID != "" || f.Session.ID != "" {
			t.Errorf("file %d = %+v, want %+v", i, f, want)
		}
	}
	if f := got.Files[1]; f.Duration != 90*time.Second || !f.RecordedAt.Equal(base) {
		t.Errorf("metadata = %v at %v; want 1m30s at %v", f.Duration, f.RecordedAt, base)
	}
	if f := got.Files[0]; f.Duration != 0 || !f.RecordedAt.IsZero() {
		t.Errorf("a file without metadata = %v at %v", f.Duration, f.RecordedAt)
	}
	one, err := s.GetBatch(context.Background(), b.ID)
	if err != nil || one.ID != b.ID || len(one.Files) != 3 {
		t.Fatalf("GetBatch = %+v, %v", one, err)
	}
	_, err = s.GetBatch(context.Background(), uuid.NewString())
	wantErr(t, err, store.ErrNotFound)
}

func savedFile(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := newBatch(t, s, base, "acme.m4a", "birch.m4a")
	// Named while it was transcribed: its session takes the name.
	if err := s.RenameFile(ctx, b.Files[0].ID, "Acme interview"); err != nil {
		t.Fatal(err)
	}
	sess := saveFile(t, s, b, b.Files[0], "Acme wants SSO.")
	got := listBatches(t, s)[0].Files[0]
	if got.State != store.FileSaved || got.SessionID != sess.ID || got.Path != "" || got.Title != "" || got.Problem != "" {
		t.Fatalf("saved file = %+v", got)
	}
	// It joined the batch as it was saved.
	if got.Session.ID != sess.ID || got.Session.Title != "Acme interview" || got.Session.BatchID != b.ID ||
		got.Session.RecapStatus != store.RecapReady || got.Recap != "" {
		t.Fatalf("its session = %+v, recap %q; want listed, renamed, without its recap", got.Session, got.Recap)
	}
	detail, err := s.GetBatch(ctx, b.ID)
	if err != nil || detail.Files[0].Recap != "Acme wants SSO." || detail.Files[1].Recap != "" {
		t.Fatalf("GetBatch = %+v, %v; want the saved file's recap", detail.Files, err)
	}
	// A saved file is its session's: renamed and moved as one.
	wantErr(t, s.RenameFile(ctx, b.Files[0].ID, "Again"), store.ErrConflict)
	wantErr(t, s.SetFileState(ctx, b.Files[0].ID, store.FileFailed, "upload"), store.ErrConflict)
	wantErr(t, s.SaveFile(ctx, b.Files[0].ID, sess.ID), store.ErrConflict)
	// Its session lists with the batch, not on its own.
	alone, err := s.ListSessions(ctx, store.ListQuery{Unbatched: true})
	if err != nil || len(alone) != 0 {
		t.Fatalf("ListSessions(Unbatched) = %+v, %v; want none", alone, err)
	}
	all, err := s.ListSessions(ctx, store.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, all, sess.ID)
	// Only a file session is a file's.
	rec := ended(t, s, base)
	wantErr(t, s.SaveFile(ctx, b.Files[1].ID, rec.ID), store.ErrNotFound)
}

func fileStates(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := newBatch(t, s, base, "a.m4a")
	id := b.Files[0].ID
	for _, c := range []struct {
		state   store.FileState
		problem string
	}{{store.FileFailed, "transcribe"}, {store.FileCanceled, ""}, {store.FileWaiting, ""}} {
		if err := s.SetFileState(ctx, id, c.state, c.problem); err != nil {
			t.Fatalf("SetFileState(%s): %v", c.state, err)
		}
		if f := listBatches(t, s)[0].Files[0]; f.State != c.state || f.Problem != c.problem || f.Path != b.Files[0].Path {
			t.Fatalf("after SetFileState(%s, %q): %+v", c.state, c.problem, f)
		}
	}
	wantErr(t, s.SetFileState(ctx, uuid.NewString(), store.FileFailed, "upload"), store.ErrNotFound)
	wantErr(t, s.RenameFile(ctx, uuid.NewString(), "x"), store.ErrNotFound)
	if err := s.RenameBatch(ctx, b.ID, "Renamed"); err != nil {
		t.Fatal(err)
	}
	if got := listBatches(t, s)[0].Name; got != "Renamed" {
		t.Fatalf("name = %q", got)
	}
	wantErr(t, s.RenameBatch(ctx, uuid.NewString(), "x"), store.ErrNotFound)
}

func trashedFilesLeftOut(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := newBatch(t, s, base, "a.m4a", "b.m4a")
	first := saveFile(t, s, b, b.Files[0], "A.")
	trash := mustTrash(t, s, first.ID)
	files := listBatches(t, s)[0].Files
	if len(files) != 1 || files[0].ID != b.Files[1].ID {
		t.Fatalf("files = %+v; want the one not in Recently Deleted", files)
	}
	if _, err := s.UndoEdit(ctx, trash.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(listBatches(t, s)[0].Files); n != 2 {
		t.Fatalf("restored, the batch lists %d files; want 2", n)
	}
	// A batch whose files are all in Recently Deleted shows no more.
	second := saveFile(t, s, b, b.Files[1], "B.")
	mustTrash(t, s, first.ID)
	mustTrash(t, s, second.ID)
	if list := listBatches(t, s); len(list) != 0 {
		t.Fatalf("ListBatches = %+v; want none", list)
	}
	_, err := s.GetBatch(ctx, b.ID)
	wantErr(t, err, store.ErrNotFound)
}

func removeFile(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := newBatch(t, s, base, "a.m4a", "b.m4a")
	sess := saveFile(t, s, b, b.Files[0], "A.")
	if err := s.RemoveFile(ctx, b.Files[0].ID); err != nil {
		t.Fatal(err)
	}
	if d, err := s.GetSession(ctx, sess.ID); err != nil || d.BatchID != "" {
		t.Fatalf("the session moved out = %+v, %v; want it on its own", d.SessionSummary, err)
	}
	alone, err := s.ListSessions(ctx, store.ListQuery{Unbatched: true})
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, alone, sess.ID)
	// The last file out forgets the batch.
	if err := s.RemoveFile(ctx, b.Files[1].ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.GetBatch(ctx, b.ID)
	wantErr(t, err, store.ErrNotFound)
	wantErr(t, s.RemoveFile(ctx, b.Files[1].ID), store.ErrNotFound)
	// A file moved out while it was transcribed leaves its session, created
	// in the batch, on its own once saved.
	late := store.Session{ID: uuid.NewString(), Mode: store.ModeCaptions, Source: store.SourceFile, LangA: "en-US", LangB: "en-US",
		StartedAt: base, FileName: "b.m4a", BatchID: b.ID}
	if err := s.CreateSession(ctx, late); err != nil {
		t.Fatal(err)
	}
	wantErr(t, s.SaveFile(ctx, b.Files[1].ID, late.ID), store.ErrNotFound)
	if d, err := s.GetSession(ctx, late.ID); err != nil || d.BatchID != "" {
		t.Fatalf("the late session = %+v, %v; want it on its own", d.SessionSummary, err)
	}
}

func ungroup(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := newBatch(t, s, base, "a.m4a", "b.m4a")
	sess := saveFile(t, s, b, b.Files[0], "A.")
	if err := s.UngroupBatch(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if list := listBatches(t, s); len(list) != 0 {
		t.Fatalf("ListBatches = %+v; want none", list)
	}
	alone, err := s.ListSessions(ctx, store.ListQuery{Unbatched: true})
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, alone, sess.ID)
	wantErr(t, s.UngroupBatch(ctx, b.ID), store.ErrNotFound)
	// Its file not saved is forgotten with it.
	wantErr(t, s.SetFileState(ctx, b.Files[1].ID, store.FileCanceled, ""), store.ErrNotFound)
}

func deleteBatch(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := newBatch(t, s, base, "a.m4a", "b.m4a", "c.m4a")
	first := saveFile(t, s, b, b.Files[0], "A.")
	second := saveFile(t, s, b, b.Files[1], "B.")
	edits, err := s.DeleteBatch(ctx, b.ID, "user", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 2 || edits[0].SessionID != first.ID || edits[1].SessionID != second.ID || edits[0].Field != store.FieldTrash {
		t.Fatalf("edits = %+v; want both sessions trashed, in order", edits)
	}
	if list := listBatches(t, s); len(list) != 0 {
		t.Fatalf("ListBatches = %+v; want none", list)
	}
	if n := len(trashedIDs(t, s)); n != 2 {
		t.Fatalf("Recently Deleted holds %d sessions; want 2", n)
	}
	// Restored, a session stands on its own.
	if _, err := s.UndoEdit(ctx, edits[0].ID); err != nil {
		t.Fatal(err)
	}
	if d, err := s.GetSession(ctx, first.ID); err != nil || d.BatchID != "" {
		t.Fatalf("restored = %+v, %v; want it on its own", d.SessionSummary, err)
	}
	_, err = s.DeleteBatch(ctx, b.ID, "user", "")
	wantErr(t, err, store.ErrNotFound)
}

func purgeForgetsFile(t *testing.T, s store.Store) {
	ctx := context.Background()
	b := newBatch(t, s, base, "a.m4a", "b.m4a")
	first := saveFile(t, s, b, b.Files[0], "A.")
	mustTrash(t, s, first.ID)
	if err := s.PurgeSession(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if files := listBatches(t, s)[0].Files; len(files) != 1 || files[0].ID != b.Files[1].ID {
		t.Fatalf("files = %+v; want the one not purged", files)
	}
	// The last file purged forgets the batch.
	if err := s.RemoveFile(ctx, b.Files[1].ID); err != nil {
		t.Fatal(err)
	}
	_, err := s.GetBatch(ctx, b.ID)
	wantErr(t, err, store.ErrNotFound)
}

func batchValidation(t *testing.T, s store.Store) {
	ctx := context.Background()
	good := store.Batch{ID: uuid.NewString(), Name: "Interviews", Lang: "en-US", CreatedAt: base}
	file := func(mod func(*store.BatchFile)) []store.BatchFile {
		f := store.BatchFile{ID: uuid.NewString(), Path: "/tmp/a.m4a", Name: "a.m4a", Size: 1}
		mod(&f)
		return []store.BatchFile{f}
	}
	keep := func(*store.BatchFile) {}
	cases := []struct {
		name  string
		b     store.Batch
		files []store.BatchFile
	}{
		{"blank name", store.Batch{ID: good.ID, Name: "  ", Lang: "en-US", CreatedAt: base}, file(keep)},
		{"name of two lines", store.Batch{ID: good.ID, Name: "a\nb", Lang: "en-US", CreatedAt: base}, file(keep)},
		{"no language", store.Batch{ID: good.ID, Name: "x", CreatedAt: base}, file(keep)},
		{"no files", good, nil},
		{"too many files", good, make([]store.BatchFile, store.MaxBatchFiles+1)},
		{"relative path", good, file(func(f *store.BatchFile) { f.Path = "a.m4a" })},
		{"path of two lines", good, file(func(f *store.BatchFile) { f.Path = "/tmp/a\n.m4a" })},
		{"path too long", good, file(func(f *store.BatchFile) { f.Path = "/" + strings.Repeat("a", store.MaxPathLen) })},
		{"name with folders", good, file(func(f *store.BatchFile) { f.Name = "x/a.m4a" })},
		{"negative size", good, file(func(f *store.BatchFile) { f.Size = -1 })},
		{"file id", good, file(func(f *store.BatchFile) { f.ID = "1" })},
	}
	for _, c := range cases {
		wantErr(t, s.CreateBatch(ctx, c.b, c.files), store.ErrInvalid)
	}
	if list := listBatches(t, s); len(list) != 0 {
		t.Fatalf("an invalid batch was stored: %+v", list)
	}
	b := newBatch(t, s, base, "a.m4a")
	wantErr(t, s.CreateBatch(ctx, b.Batch, file(keep)), store.ErrConflict)
	id := b.Files[0].ID
	wantErr(t, s.SetFileState(ctx, id, store.FileFailed, ""), store.ErrInvalid)
	wantErr(t, s.SetFileState(ctx, id, store.FileWaiting, "upload"), store.ErrInvalid)
	wantErr(t, s.SetFileState(ctx, id, store.FileSaved, ""), store.ErrInvalid)
	wantErr(t, s.RenameBatch(ctx, b.ID, ""), store.ErrInvalid)
	wantErr(t, s.RenameFile(ctx, id, "a\nb"), store.ErrInvalid)
	wantErr(t, s.SaveFile(ctx, id, "not a uuid"), store.ErrInvalid)
}
