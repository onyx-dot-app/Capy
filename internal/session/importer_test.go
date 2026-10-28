package session_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/fakezoom"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

// importer is an Importer over h, recording the batches it finished.
type importer struct {
	*session.Importer
	mu       sync.Mutex
	finished []string
}

func newImporter(t *testing.T, h *harness, mod func(*session.ImporterOptions)) *importer {
	t.Helper()
	im := &importer{}
	o := session.ImporterOptions{
		Store:       h.store,
		Client:      func() (*zoomai.Client, error) { return h.deps.AI, nil },
		OfflineWait: 20 * time.Millisecond,
		Finished: func(id string) {
			im.mu.Lock()
			defer im.mu.Unlock()
			im.finished = append(im.finished, id)
		},
	}
	if mod != nil {
		mod(&o)
	}
	im.Importer = session.NewImporter(o)
	t.Cleanup(im.Stop)
	return im
}

func (im *importer) finishedBatches() []string {
	im.mu.Lock()
	defer im.mu.Unlock()
	return slices.Clone(im.finished)
}

// wait waits until im is idle.
func (im *importer) wait(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := im.Wait(ctx); err != nil {
		t.Fatalf("the importer never finished: %v", err)
	}
}

// eventually waits for cond, which polls.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
	}
}

// batchOf is a batch of files at paths, created at now, the first recorded
// a day before.
func batchOf(paths ...string) (store.Batch, []store.BatchFile) {
	b := store.Batch{ID: uuid.NewString(), Name: "Interviews", Lang: "en-US", CreatedAt: time.Now().UTC().Truncate(time.Millisecond)}
	files := make([]store.BatchFile, len(paths))
	for i, p := range paths {
		files[i] = store.BatchFile{ID: uuid.NewString(), Path: p, Name: filepath.Base(p), Size: 10}
	}
	files[0].RecordedAt = b.CreatedAt.Add(-24 * time.Hour)
	return b, files
}

func states(t *testing.T, st store.Store, batchID string) []string {
	t.Helper()
	b, err := st.GetBatch(context.Background(), batchID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range b.Files {
		s := string(f.State)
		if f.Problem != "" {
			s += ":" + f.Problem
		}
		out = append(out, s)
	}
	return out
}

func TestImporterTranscribesEachFileIntoASessionOfTheBatch(t *testing.T) {
	h := newHarness(t, fakezoom.Script{FailSpeech: "broken", FailSpeechSummary: "flaky"})
	im := newImporter(t, h, nil)
	ctx := context.Background()
	b, files := batchOf(audioFile(t, "acme.m4a", 10), audioFile(t, "flaky call.mp3", 10), audioFile(t, "broken.wav", 10),
		filepath.Join(t.TempDir(), "gone.m4a"))
	if err := im.Start(ctx, b, files); err != nil {
		t.Fatal(err)
	}
	im.wait(t)
	if got := states(t, h.store, b.ID); !slices.Equal(got, []string{"saved", "saved", "failed:transcribe", "failed:missing"}) {
		t.Fatalf("states %v", got)
	}
	got, _ := h.store.GetBatch(ctx, b.ID)
	acme, flaky := got.Files[0].Session, got.Files[1].Session
	// Each is a session of the batch, starting when its file was recorded,
	// else when it was imported.
	if acme.BatchID != b.ID || !acme.StartedAt.Equal(files[0].RecordedAt) || acme.RecapStatus != store.RecapReady {
		t.Errorf("acme = %+v", acme)
	}
	if flaky.BatchID != b.ID || !flaky.StartedAt.Equal(b.CreatedAt) || flaky.RecapStatus != store.RecapFailed {
		t.Errorf("flaky = %+v", flaky)
	}
	if fin := im.finishedBatches(); !slices.Equal(fin, []string{b.ID}) {
		t.Errorf("finished %v, want the batch once", fin)
	}
	if im.Busy("") {
		t.Error("busy once done")
	}
}

func TestImporterSendsTwoFilesAtATime(t *testing.T) {
	h := newHarness(t, fakezoom.Script{HoldTranscript: ".wav"})
	im := newImporter(t, h, nil)
	b, files := batchOf(audioFile(t, "1.wav", 10), audioFile(t, "2.wav", 10), audioFile(t, "3.wav", 10))
	if err := im.Start(context.Background(), b, files); err != nil {
		t.Fatal(err)
	}
	eventually(t, "two files sent", func() bool { return len(h.fake.SpeechCalls()) == 2 })
	time.Sleep(50 * time.Millisecond)
	if n := len(h.fake.SpeechCalls()); n != 2 {
		t.Fatalf("%d files sent at once; want 2", n)
	}
	// Only what has not been sent whole would be lost by quitting.
	if n := im.Unsent(b.ID); n != 1 {
		t.Errorf("Unsent = %d; want the one queued", n)
	}
	if p, ok := im.Progress(files[0].ID); !ok || p.Stage != zoomai.StageTranscribing {
		t.Errorf("progress = %+v, %v; want transcribing", p, ok)
	}
	h.fake.Release()
	im.wait(t)
	if got := states(t, h.store, b.ID); !slices.Equal(got, []string{"saved", "saved", "saved"}) {
		t.Fatalf("states %v", got)
	}
}

func TestImporterCancelsFilesQueuedAndUnderWay(t *testing.T) {
	h := newHarness(t, fakezoom.Script{HoldTranscript: "held"})
	im := newImporter(t, h, func(o *session.ImporterOptions) { o.Parallel = 1 })
	b, files := batchOf(audioFile(t, "held.wav", 10), audioFile(t, "next.wav", 10), audioFile(t, "last.wav", 10))
	if err := im.Start(context.Background(), b, files); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the first file sent", func() bool { return len(h.fake.SpeechCalls()) == 1 })
	im.Cancel(files[1].ID) // queued: never sent
	if fin := im.finishedBatches(); len(fin) != 0 {
		t.Fatalf("finished %v with a file under way", fin)
	}
	im.Cancel(files[0].ID) // under way: stopped before its transcript
	im.wait(t)
	if got := states(t, h.store, b.ID); !slices.Equal(got, []string{"canceled", "canceled", "saved"}) {
		t.Fatalf("states %v", got)
	}
	if calls := h.fake.SpeechCalls(); slices.Contains(calls, "next.wav/en-US") {
		t.Errorf("sent a canceled file: %v", calls)
	}
	// Retry sends a canceled file after all.
	if err := im.Retry(context.Background(), b.ID, files[1].ID); err != nil {
		t.Fatal(err)
	}
	im.wait(t)
	if got := states(t, h.store, b.ID); got[1] != "saved" {
		t.Fatalf("after Retry: %v", got)
	}
	// CancelBatch stops what is left of it.
	b2, files2 := batchOf(audioFile(t, "held 2.wav", 10), audioFile(t, "queued.wav", 10))
	if err := im.Start(context.Background(), b2, files2); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the held file sent", func() bool { return slices.Contains(h.fake.SpeechCalls(), "held 2.wav/en-US") })
	im.CancelBatch(b2.ID)
	im.wait(t)
	if got := states(t, h.store, b2.ID); !slices.Equal(got, []string{"canceled", "canceled"}) {
		t.Fatalf("after CancelBatch: %v", got)
	}
	if fin := im.finishedBatches(); fin[len(fin)-1] != b2.ID {
		t.Errorf("finished %v", fin)
	}
}

func TestImporterWaitsOfflineAndGoesOnByItself(t *testing.T) {
	h := newHarness(t, fakezoom.Script{})
	im := newImporter(t, h, nil)
	h.fake.SetOffline(true)
	b, files := batchOf(audioFile(t, "a.wav", 10), audioFile(t, "b.wav", 10))
	if err := im.Start(context.Background(), b, files); err != nil {
		t.Fatal(err)
	}
	eventually(t, "paused offline", func() bool { return im.Paused() == session.PausedOffline })
	// Offline is no failure: the files wait.
	if got := states(t, h.store, b.ID); !slices.Equal(got, []string{"waiting", "waiting"}) {
		t.Fatalf("offline, states %v", got)
	}
	if !im.Busy(b.ID) {
		t.Fatal("not busy while waiting for the connection")
	}
	h.fake.SetOffline(false)
	im.wait(t)
	if got := states(t, h.store, b.ID); !slices.Equal(got, []string{"saved", "saved"}) || im.Paused() != session.Running {
		t.Fatalf("online again: states %v, paused %v", got, im.Paused())
	}
}

func TestImporterHoldsWhileARecordingReconnects(t *testing.T) {
	h := newHarness(t, fakezoom.Script{})
	im := newImporter(t, h, nil)
	im.Hold(true)
	b, files := batchOf(audioFile(t, "a.wav", 10))
	if err := im.Start(context.Background(), b, files); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(h.fake.SpeechCalls()); n != 0 || im.Paused() != session.PausedRecording {
		t.Fatalf("held: %d files sent, paused %v", n, im.Paused())
	}
	im.Hold(false)
	im.wait(t)
	if got := states(t, h.store, b.ID); !slices.Equal(got, []string{"saved"}) {
		t.Fatalf("states %v", got)
	}
}

func TestImporterPicksUpWhereAQuitLeftOff(t *testing.T) {
	h := newHarness(t, fakezoom.Script{HoldTranscript: "held"})
	first := newImporter(t, h, func(o *session.ImporterOptions) { o.Parallel = 1 })
	b, files := batchOf(audioFile(t, "held.wav", 10), audioFile(t, "queued.wav", 10))
	if err := first.Start(context.Background(), b, files); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the first file sent", func() bool { return len(h.fake.SpeechCalls()) == 1 })
	first.Stop() // the app quits
	first.wait(t)
	if got := states(t, h.store, b.ID); !slices.Equal(got, []string{"waiting", "waiting"}) {
		t.Fatalf("after quitting, states %v; want both waiting", got)
	}
	if fin := first.finishedBatches(); len(fin) != 0 {
		t.Errorf("a quit finished %v", fin)
	}
	// The next launch.
	h.fake.Release()
	next := newImporter(t, h, nil)
	n, err := next.Resume(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("Resume = %d, %v; want 2", n, err)
	}
	next.wait(t)
	if got := states(t, h.store, b.ID); !slices.Equal(got, []string{"saved", "saved"}) {
		t.Fatalf("resumed, states %v", got)
	}
}

func TestImporterRetriesAFileThatFailed(t *testing.T) {
	h := newHarness(t, fakezoom.Script{})
	im := newImporter(t, h, nil)
	path := filepath.Join(t.TempDir(), "later.wav")
	b, files := batchOf(path)
	if err := im.Start(context.Background(), b, files); err != nil {
		t.Fatal(err)
	}
	im.wait(t)
	if got := states(t, h.store, b.ID); !slices.Equal(got, []string{"failed:missing"}) {
		t.Fatalf("states %v", got)
	}
	if err := os.WriteFile(path, make([]byte, 10), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := im.RetryFailed(context.Background(), b.ID); err != nil {
		t.Fatal(err)
	}
	im.wait(t)
	if got := states(t, h.store, b.ID); !slices.Equal(got, []string{"saved"}) {
		t.Fatalf("after RetryFailed, states %v", got)
	}
}

func TestImporterWithoutAKeyFailsTheFileForIt(t *testing.T) {
	h := newHarness(t, fakezoom.Script{})
	im := newImporter(t, h, func(o *session.ImporterOptions) {
		o.Client = func() (*zoomai.Client, error) { return nil, errors.New("no key in the Keychain") }
	})
	b, files := batchOf(audioFile(t, "a.wav", 10))
	if err := im.Start(context.Background(), b, files); err != nil {
		t.Fatal(err)
	}
	im.wait(t)
	if got := states(t, h.store, b.ID); !slices.Equal(got, []string{"failed:key"}) {
		t.Fatalf("states %v", got)
	}
}

func TestAFileMovedOutWhileTranscribedStaysOnItsOwn(t *testing.T) {
	h := newHarness(t, fakezoom.Script{HoldTranscript: "held"})
	im := newImporter(t, h, nil)
	ctx := context.Background()
	b, files := batchOf(audioFile(t, "held.wav", 10), audioFile(t, "other.wav", 10))
	if err := im.Start(ctx, b, files); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the held file sent", func() bool { return slices.Contains(h.fake.SpeechCalls(), "held.wav/en-US") })
	if err := h.store.RemoveFile(ctx, files[0].ID); err != nil {
		t.Fatal(err)
	}
	h.fake.Release()
	im.wait(t)
	alone, err := h.store.ListSessions(ctx, store.ListQuery{Unbatched: true})
	if err != nil || len(alone) != 1 || alone[0].FileName != "held.wav" {
		t.Fatalf("sessions on their own = %+v, %v; want the file moved out", alone, err)
	}
}

func TestBatchNameIsTheStartTheFilesShare(t *testing.T) {
	at := time.Date(2026, 10, 20, 9, 58, 0, 0, time.Local)
	for _, c := range []struct {
		names []string
		want  string
	}{
		{[]string{"interview-acme-0921.m4a", "interview-birch-0922.m4a"}, "interview"},
		{[]string{"Customer call 1.wav", "Customer call 2.wav", "Customer call 10.mp3"}, "Customer call"},
		{[]string{"週次ミーティング 1.m4a", "週次ミーティング 2.m4a"}, "週次ミーティング"},
		{[]string{"Weekly sync.m4a"}, "Weekly sync"},
		{[]string{"2026-10-01.wav", "2026-10-02.wav"}, "Imported Oct 20"},
		{[]string{"a.wav", "b.wav"}, "Imported Oct 20"},
	} {
		if got := session.BatchName(c.names, at); got != c.want {
			t.Errorf("BatchName(%q) = %q, want %q", c.names, got, c.want)
		}
	}
}
