package session_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onyx-dot-app/Capy/internal/fakezoom"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

// audioFile writes size bytes to a file named name and returns its path.
func audioFile(t *testing.T, name string, size int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTranscribeSavesTheFileAsAnEndedSession(t *testing.T) {
	h := newHarness(t, fakezoom.Script{})
	recorded := time.Date(2026, 9, 26, 15, 40, 0, 0, time.UTC)
	var stages []zoomai.SpeechStage
	id, err := session.Transcribe(context.Background(), h.deps, audioFile(t, "Weekly sync.m4a", 2048), session.FileOptions{
		Lang: "de-DE", StartedAt: recorded,
		Progress: func(p zoomai.SpeechProgress) {
			if !slices.Contains(stages, p.Stage) {
				stages = append(stages, p.Stage)
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	d, err := h.store.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Source != store.SourceFile || d.FileName != "Weekly sync.m4a" || d.BatchID != "" || d.LangA != "de-DE" ||
		d.LangB != "de-DE" || d.Translated {
		t.Fatalf("session %+v", d.SessionSummary)
	}
	// Zoom's logs find the request by it.
	if d.TrackingID != "FAKEZOOM_speech_1" {
		t.Errorf("tracking ID %q, want the answer's", d.TrackingID)
	}
	// Titled by its file, the summary whole below it; it starts when the
	// file was recorded and lasts as long as its transcript.
	if d.Title != "Weekly sync" || d.RecapStatus != store.RecapReady || d.Recap != "## Summary (de-DE)\n\n- Weekly sync.m4a was discussed" {
		t.Fatalf("title %q, recap %s %q", d.Title, d.RecapStatus, d.Recap)
	}
	if !d.StartedAt.Equal(recorded) || d.EndedAt.Sub(d.StartedAt) != 4500*time.Millisecond {
		t.Errorf("started %v, ended %v: want at %v, the transcript's 4.5 s", d.StartedAt, d.EndedAt, recorded)
	}
	want := []store.Segment{
		{Seq: 0, Lang: "de-DE", Text: "This is Weekly sync.m4a.", Speaker: "Speaker 1", StartMs: 500, EndMs: 2250},
		{Seq: 1, Lang: "de-DE", Text: "It is 2048 bytes long.", Speaker: "Speaker 2", StartMs: 3000, EndMs: 4500},
	}
	if len(d.Segments) != len(want) {
		t.Fatalf("segments %+v", d.Segments)
	}
	for i, g := range d.Segments {
		w := want[i]
		w.ID, w.SessionID = g.ID, id
		if g != w {
			t.Errorf("segment %d = %+v, want %+v", i, g, w)
		}
	}
	if !slices.Equal(stages, []zoomai.SpeechStage{zoomai.StageUploading, zoomai.StageTranscribing, zoomai.StageSummarizing}) {
		t.Errorf("stages %v", stages)
	}
	if calls := h.fake.SpeechCalls(); !slices.Equal(calls, []string{"Weekly sync.m4a/de-DE"}) {
		t.Errorf("speech calls %v", calls)
	}
}

func TestTranscribeSavesTheTranscriptBeforeTheSummary(t *testing.T) {
	h := newHarness(t, fakezoom.Script{HoldSummary: "slow"})
	ctx := context.Background()
	saved := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		_, err := session.Transcribe(ctx, h.deps, audioFile(t, "slow recap.wav", 10), session.FileOptions{
			Lang: "en-US", Saved: func(id string) { saved <- id }})
		done <- err
	}()
	var id string
	select {
	case id = <-saved:
	case <-time.After(10 * time.Second):
		t.Fatal("the transcript was not saved before the summary")
	}
	// Readable while the summary is written.
	d, err := h.store.GetSession(ctx, id)
	if err != nil || len(d.Segments) != 2 || d.EndedAt.IsZero() || d.RecapStatus != store.RecapPending {
		t.Fatalf("while summarizing: %+v with %d segments, %v", d.SessionSummary, len(d.Segments), err)
	}
	h.fake.Release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if d, _ := h.store.GetSession(ctx, id); d.RecapStatus != store.RecapReady {
		t.Fatalf("after: recap %s", d.RecapStatus)
	}
}

func TestTranscribeTranslatesTheTranscript(t *testing.T) {
	h := newHarness(t, fakezoom.Script{})
	ctx := context.Background()
	id, err := session.Transcribe(ctx, h.deps, audioFile(t, "Standup.mp3", 10), session.FileOptions{Lang: "en-US", Target: "fr-FR"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := h.store.GetSession(ctx, id)
	if err != nil || d.LangB != "fr-FR" || !d.Translated || len(d.Segments) != 2 {
		t.Fatalf("session %+v, %v", d.SessionSummary, err)
	}
	for _, seg := range d.Segments {
		if seg.TranslationLang != "fr-FR" || seg.Translation != "[fr-FR] "+seg.Text {
			t.Errorf("segment %q translated as %q (%s)", seg.Text, seg.Translation, seg.TranslationLang)
		}
	}
	// The summary is the file's own, in the language spoken.
	if d.Recap != "## Summary (en-US)\n\n- Standup.mp3 was discussed" {
		t.Errorf("recap %q", d.Recap)
	}
	// A target that is the spoken language translates nothing.
	id, err = session.Transcribe(ctx, h.deps, audioFile(t, "Same.mp3", 10), session.FileOptions{Lang: "en-US", Target: "en-US"})
	if d, _ := h.store.GetSession(ctx, id); err != nil || d.Translated || d.LangB != "en-US" {
		t.Fatalf("same target: %+v, %v", d.SessionSummary, err)
	}
}

func TestTranscribeLeavesAFailedTranslationToRetry(t *testing.T) {
	h := newHarness(t, fakezoom.Script{FailTranslate: "bytes long"})
	ctx := context.Background()
	id, err := session.Transcribe(ctx, h.deps, audioFile(t, "Standup.mp3", 10), session.FileOptions{Lang: "en-US", Target: "fr-FR"})
	if err != nil {
		t.Fatalf("a failed translation failed the file: %v", err)
	}
	d, _ := h.store.GetSession(ctx, id)
	if !session.Untranslated(d.Segments[1]) || session.Untranslated(d.Segments[0]) || d.RecapStatus != store.RecapReady {
		t.Fatalf("segments %+v", d.Segments)
	}
}

func TestTranscribeKeepsTheTranscriptWhenTheSummaryFailsAndRetriesIt(t *testing.T) {
	h := newHarness(t, fakezoom.Script{FailSpeechSummary: "flaky"})
	ctx := context.Background()
	id, err := session.Transcribe(ctx, h.deps, audioFile(t, "flaky call.mp3", 10), session.FileOptions{Lang: "en-US"})
	if id == "" || err == nil || errors.Is(err, session.ErrNotTranscribed) {
		t.Fatalf("id %q, err %v; want the transcript saved and the summary's failure", id, err)
	}
	d, _ := h.store.GetSession(ctx, id)
	if d.RecapStatus != store.RecapFailed || d.Title != "flaky call" || len(d.Segments) != 2 || d.EndedAt.IsZero() ||
		d.TrackingID != "FAKEZOOM_speech_1" {
		t.Fatalf("session %+v with %d segments, tracking ID %q", d.SessionSummary, len(d.Segments), d.TrackingID)
	}
	// Retry Recap summarizes the saved transcript, and keeps the file's title.
	if err := session.Recap(ctx, h.deps, id); err != nil {
		t.Fatal(err)
	}
	d, _ = h.store.GetSession(ctx, id)
	_, _, _, summarize := h.fake.Calls()
	if d.RecapStatus != store.RecapReady || d.Title != "flaky call" || !slices.Equal(summarize, []string{"full_summary/en-US"}) {
		t.Fatalf("after retry: %+v, summarize calls %v", d.SessionSummary, summarize)
	}
}

func TestTranscribeSavesNothingWhenTranscriptionFails(t *testing.T) {
	h := newHarness(t, fakezoom.Script{FailSpeech: "broken"})
	ctx := context.Background()
	for name, path := range map[string]string{
		"failed":      audioFile(t, "broken.wav", 10),
		"unsupported": audioFile(t, "notes.txt", 10),
		"missing":     filepath.Join(t.TempDir(), "gone.wav"),
		"bad name":    audioFile(t, "two\nlines.wav", 10), // never sent: the library would refuse it after
	} {
		id, err := session.Transcribe(ctx, h.deps, path, session.FileOptions{Lang: "en-US"})
		if id != "" || !errors.Is(err, session.ErrNotTranscribed) {
			t.Errorf("%s: id %q, err %v", name, id, err)
		}
		// The file's name is the user's: errors, which are logged, leave it out.
		if err != nil && strings.Contains(err.Error(), filepath.Base(path)) {
			t.Errorf("%s: error %q names the file", name, err)
		}
	}
	if list, _ := h.store.ListSessions(ctx, store.ListQuery{}); len(list) != 0 {
		t.Fatalf("saved %d sessions", len(list))
	}
	if calls := h.fake.SpeechCalls(); !slices.Equal(calls, []string{"broken.wav/en-US"}) {
		t.Errorf("speech calls %q, want the failed file's only", calls)
	}
}

func TestFileTitleDropsTheExtensionOnly(t *testing.T) {
	for name, want := range map[string]string{
		"Weekly sync.m4a":     "Weekly sync",
		"call.2026-10-19.wav": "call.2026-10-19",
		".mp3":                ".mp3",
	} {
		if got := session.FileTitle(name); got != want {
			t.Errorf("FileTitle(%q) = %q, want %q", name, got, want)
		}
	}
}
