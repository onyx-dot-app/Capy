package session_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onyx-dot-app/Capy/internal/audio"
	"github.com/onyx-dot-app/Capy/internal/fakezoom"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/store/sqlite"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

const testKey = "test-key"

// silence writes a WAV of n frames of silence and returns its path.
func silence(t *testing.T, frames int) string {
	t.Helper()
	pcm := make([]byte, frames*audio.FrameBytes)
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(16000), uint32(32000), uint16(2), uint16(16)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	path := filepath.Join(t.TempDir(), "silence.wav")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// turns spaces texts one second of audio apart.
func turns(texts ...string) []fakezoom.Turn {
	out := make([]fakezoom.Turn, len(texts))
	for i, text := range texts {
		out[i] = fakezoom.Turn{Text: text, AfterBytes: (i + 1) * 10 * audio.FrameBytes}
	}
	return out
}

type harness struct {
	fake     *fakezoom.Server
	store    *sqlite.DB
	deps     session.Deps
	realtime bool // pace audio like a live capture
}

func newHarness(t *testing.T, script fakezoom.Script) *harness {
	t.Helper()
	if script.APIKey == "" {
		script.APIKey = testKey
	}
	fake := fakezoom.New(script)
	t.Cleanup(fake.Close)
	ai, err := zoomai.NewClient(testKey, zoomai.WithBaseURL(fake.URL))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "capy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &harness{fake: fake, store: db, deps: session.Deps{AI: ai, Store: db}}
}

// runToEnd plays a WAV of frames through a session and waits for its recap.
func (h *harness) runToEnd(t *testing.T, cfg session.Config, frames int) (session.Snapshot, store.SessionDetail) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run, err := session.Start(ctx, h.deps, cfg, audio.NewWAV(silence(t, frames), h.realtime), nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-run.Done():
	case <-ctx.Done():
		t.Fatal("session did not finish")
	}
	snap := run.Snapshot()
	detail, err := h.store.GetSession(ctx, snap.SessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	return snap, detail
}

type seg struct{ lang, text, translation string }

func segs(d store.SessionDetail) []seg {
	var out []seg
	for _, s := range d.Segments {
		out = append(out, seg{s.Lang, s.Text, s.Translation})
	}
	return out
}

func TestCaptionsTranscribesTranslatesAndSummarizes(t *testing.T) {
	h := newHarness(t, fakezoom.Script{Turns: turns("Welcome to the demo.", "Capy listens and translates.")})
	snap, d := h.runToEnd(t, session.Config{Source: store.SourceReplay, LangA: "en-US", LangB: "ja-JP", Translate: true}, 25)

	want := []seg{
		{"en-US", "Welcome to the demo.", "[ja-JP] Welcome to the demo."},
		{"en-US", "Capy listens and translates.", "[ja-JP] Capy listens and translates."},
	}
	if got := segs(d); !slices.Equal(got, want) {
		t.Fatalf("segments %+v, want %+v", got, want)
	}
	if snap.Status != session.StatusDone || d.RecapStatus != store.RecapReady || d.EndedAt.IsZero() {
		t.Fatalf("status %s, recap %s, ended %v", snap.Status, d.RecapStatus, d.EndedAt)
	}
	if d.Title != "Recap (ja-JP)" || !strings.Contains(d.Recap, "2 lines discussed") {
		t.Fatalf("title %q, recap %q", d.Title, d.Recap)
	}
	_, languages, translate, summarize := h.fake.Calls()
	if !slices.Equal(languages, []string{"en-US"}) || !slices.Equal(summarize, []string{"full_summary/ja-JP"}) ||
		len(translate) != 2 {
		t.Fatalf("calls: live %v, translate %v, summarize %v", languages, translate, summarize)
	}
}

func TestCaptionsWithoutEnglishPivotThroughEnglish(t *testing.T) {
	h := newHarness(t, fakezoom.Script{Turns: turns("はじめまして、よろしくお願いします。")})
	_, d := h.runToEnd(t, session.Config{Source: store.SourceReplay, LangA: "ja-JP", LangB: "es-ES", Translate: true}, 25)

	_, _, translate, _ := h.fake.Calls()
	if want := []string{"ja-JP>en-US", "en-US>es-ES"}; !slices.Equal(translate, want) {
		t.Fatalf("translate calls %v, want %v", translate, want)
	}
	if got := d.Segments[0].Translation; got != "[es-ES] [en-US] はじめまして、よろしくお願いします。" {
		t.Fatalf("pivoted translation %q", got)
	}
}

func TestDroppedConnectionReconnectsAndMarksTheGap(t *testing.T) {
	h := newHarness(t, fakezoom.Script{
		Turns:          turns("Before the drop.", "After the drop."),
		DropAfterBytes: 12 * audio.FrameBytes,
	})
	// Live audio keeps arriving while the drop is noticed and the session
	// reconnects; unpaced audio could all be sent before the drop is seen.
	h.realtime = true
	snap, d := h.runToEnd(t, session.Config{Source: store.SourceReplay, LangA: "en-US", LangB: "zh-CN"}, 30)

	connections, _, _, _ := h.fake.Calls()
	if connections != 2 {
		t.Fatalf("%d connections, want a reconnect", connections)
	}
	var langs []string
	for _, s := range d.Segments {
		langs = append(langs, s.Lang)
	}
	if !slices.Equal(langs, []string{"en-US", store.GapLang, "en-US"}) {
		t.Fatalf("segment languages %v, want a gap between the turns", langs)
	}
	// Scribe's clock starts over on the new connection; the session's goes on.
	if before, after := d.Segments[0], d.Segments[2]; after.StartMs <= before.EndMs {
		t.Fatalf("turn after the reconnect starts at %d ms, before the end of the earlier turn (%d ms)", after.StartMs, before.EndMs)
	}
	if snap.Status != session.StatusDone || snap.Err != nil {
		t.Fatalf("status %s, err %v", snap.Status, snap.Err)
	}
}

func TestCaptionsInTheSpokenLanguageAreNotTranslated(t *testing.T) {
	h := newHarness(t, fakezoom.Script{Turns: turns("Just captions.", "No translation.")})
	snap, d := h.runToEnd(t, session.Config{Source: store.SourceReplay, LangA: "en-US", LangB: "en-US", Translate: true}, 25)

	want := []seg{{"en-US", "Just captions.", ""}, {"en-US", "No translation.", ""}}
	if got := segs(d); !slices.Equal(got, want) {
		t.Fatalf("segments %+v, want %+v", got, want)
	}
	for _, l := range snap.Lines {
		if l.Translating || l.TranslateFailed {
			t.Fatalf("line %+v waits for a translation", l)
		}
	}
	_, _, translate, summarize := h.fake.Calls()
	if len(translate) != 0 || !slices.Equal(summarize, []string{"full_summary/en-US"}) {
		t.Fatalf("calls: translate %v, summarize %v", translate, summarize)
	}
	if failed, err := session.RetryTranslations(context.Background(), h.deps, d.ID); err != nil || failed != 0 {
		t.Fatalf("retry: %d failed, %v", failed, err)
	}
	if _, _, translate, _ := h.fake.Calls(); len(translate) != 0 {
		t.Fatalf("retry translated %v", translate)
	}
}

func TestFailedTranslationKeepsTheTurnAndCanBeRetried(t *testing.T) {
	h := newHarness(t, fakezoom.Script{Turns: turns("This one translates.", "This one will fail."), FailTranslate: "fail"})
	snap, d := h.runToEnd(t, session.Config{Source: store.SourceReplay, LangA: "en-US", LangB: "de-DE", Translate: true}, 25)

	if d.Segments[1].Text != "This one will fail." || d.Segments[1].Translation != "" {
		t.Fatalf("failed turn %+v", d.Segments[1])
	}
	if !snap.Lines[1].TranslateFailed || snap.Lines[0].TranslateFailed {
		t.Fatalf("lines %+v", snap.Lines)
	}
	if d.RecapStatus != store.RecapReady {
		t.Fatalf("recap %s: a failed translation must not block the recap", d.RecapStatus)
	}
	failed, err := session.RetryTranslations(context.Background(), h.deps, d.ID)
	if err != nil || failed != 1 {
		t.Fatalf("retry: %d failed, %v", failed, err)
	}
}

func TestFailedSummaryIsRecordedForRetry(t *testing.T) {
	defer session.SetSummarizeRetryWait(time.Millisecond)()
	h := newHarness(t, fakezoom.Script{Turns: turns("Hello there."), FailSummarize: 3})
	snap, d := h.runToEnd(t, session.Config{Source: store.SourceReplay, LangA: "en-US", LangB: "fr-FR", Translate: true}, 15)

	if snap.Status != session.StatusFailed || d.RecapStatus != store.RecapFailed {
		t.Fatalf("status %s, recap %s", snap.Status, d.RecapStatus)
	}
	if _, _, _, summarize := h.fake.Calls(); len(summarize) != 3 {
		t.Fatalf("summarize calls %v, want three attempts", summarize)
	}
	if len(d.Segments) != 1 || d.Segments[0].Translation == "" {
		t.Fatalf("the transcript must survive a failed recap: %+v", d.Segments)
	}
	if err := session.Recap(context.Background(), h.deps, d.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if d, _ = h.store.GetSession(context.Background(), d.ID); d.RecapStatus != store.RecapReady || d.Recap == "" {
		t.Fatalf("after the retry: recap %s %q", d.RecapStatus, d.Recap)
	}
}

func TestSummarizerFailingOnceIsAskedAgain(t *testing.T) {
	defer session.SetSummarizeRetryWait(time.Millisecond)()
	h := newHarness(t, fakezoom.Script{Turns: turns("Hello there."), FailSummarize: 1})
	snap, d := h.runToEnd(t, session.Config{Source: store.SourceReplay, LangA: "en-US", LangB: "fr-FR"}, 15)

	if snap.Status != session.StatusDone || d.RecapStatus != store.RecapReady || d.Recap == "" {
		t.Fatalf("status %s, recap %s %q", snap.Status, d.RecapStatus, d.Recap)
	}
	if _, _, _, summarize := h.fake.Calls(); len(summarize) != 2 {
		t.Fatalf("summarize calls %v, want a second attempt", summarize)
	}
}

func TestRecapCutShortIsMarkedFailed(t *testing.T) {
	defer session.SetSummarizeRetryWait(time.Minute)()
	h := newHarness(t, fakezoom.Script{FailSummarize: 3})
	id := fmtUUID(1)
	if err := h.store.CreateSession(context.Background(), store.Session{ID: id, Mode: store.ModeCaptions,
		Source: store.SourceReplay, LangA: "en-US", LangB: "es-ES", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.AppendSegment(context.Background(), store.Segment{ID: fmtUUID(2), SessionID: id, Lang: "en-US", Text: "Hello."}); err != nil {
		t.Fatal(err)
	}
	// The deadline passes while waiting to ask again.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := session.Recap(ctx, h.deps, id); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want the deadline", err)
	}
	if d, _ := h.store.GetSession(context.Background(), id); d.RecapStatus != store.RecapFailed {
		t.Fatalf("recap %s: a recap cut short must be offered for retry, not left pending", d.RecapStatus)
	}
}

func TestRejectedKeyStartsNoSession(t *testing.T) {
	h := newHarness(t, fakezoom.Script{APIKey: "another-key"})
	_, err := session.Start(context.Background(), h.deps,
		session.Config{Source: store.SourceReplay, LangA: "en-US", LangB: "zh-CN"},
		audio.NewWAV(silence(t, 5), false), nil)
	if !errors.Is(err, zoomai.ErrUnauthorized) {
		t.Fatalf("err %v, want ErrUnauthorized", err)
	}
	list, err := h.store.ListSessions(context.Background(), store.ListQuery{})
	if err != nil || len(list) != 0 {
		t.Fatalf("sessions %v, %v; want none", list, err)
	}
}

func TestLongTranscriptIsSummarizedInChunks(t *testing.T) {
	h := newHarness(t, fakezoom.Script{})
	ctx := context.Background()
	id := "6f9c2a52-8a43-4c35-9a3a-4f1f0b7c1d10"
	if err := h.store.CreateSession(ctx, store.Session{ID: id, Mode: store.ModeCaptions, Source: store.SourceReplay,
		LangA: "en-US", LangB: "es-ES", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("a long meeting goes on and on ", 3) // ~90 bytes
	for i := range 1500 {                                       // ~135 KB, over the 96 KB limit
		seg := store.Segment{ID: fmtUUID(i), SessionID: id, Seq: i, Lang: "en-US", Text: line}
		if err := h.store.AppendSegment(ctx, seg); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.Recap(ctx, h.deps, id); err != nil {
		t.Fatal(err)
	}
	_, _, _, summarize := h.fake.Calls()
	// Never translated, it is summarized in the language spoken.
	if len(summarize) != 3 || summarize[0] != "summary/en-US" || summarize[1] != "summary/en-US" || summarize[2] != "full_summary/en-US" {
		t.Fatalf("summarize calls %v, want two chunk summaries then the full summary", summarize)
	}
}

func fmtUUID(i int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
}

func TestFatalScribeErrorEndsTheSessionWithoutReconnecting(t *testing.T) {
	h := newHarness(t, fakezoom.Script{
		Turns:           turns("Said before the error."),
		FatalAfterBytes: 15 * audio.FrameBytes,
	})
	h.realtime = true
	snap, d := h.runToEnd(t, session.Config{Source: store.SourceReplay, LangA: "en-US", LangB: "zh-CN"}, 25)

	connections, _, _, _ := h.fake.Calls()
	if connections != 1 {
		t.Fatalf("%d connections: a fatal error must not reconnect", connections)
	}
	if snap.Err == nil || !strings.Contains(snap.Err.Error(), "quota_exceeded") {
		t.Fatalf("err %v, want the fatal error", snap.Err)
	}
	if len(d.Segments) != 1 || d.Segments[0].Text != "Said before the error." || d.EndedAt.IsZero() {
		t.Fatalf("segments %+v, ended %v: the turns before the error must be kept", d.Segments, d.EndedAt)
	}
}

// liveSource emits silence like a microphone until its context ends, then a
// short tail, as capture hardware does while it stops.
type liveSource struct{}

func (liveSource) Err() error { return nil }

func (liveSource) Start(ctx context.Context) (<-chan []byte, error) {
	out := make(chan []byte, 64)
	go func() {
		defer close(out)
		for ctx.Err() == nil {
			out <- make([]byte, audio.FrameBytes)
			time.Sleep(2 * time.Millisecond)
		}
		for range 20 {
			out <- make([]byte, audio.FrameBytes)
		}
	}()
	return out, nil
}

func TestStopMidSentenceKeepsTheSentenceInProgress(t *testing.T) {
	h := newHarness(t, fakezoom.Script{Turns: []fakezoom.Turn{
		{Text: "The first sentence.", AfterBytes: 10 * audio.FrameBytes},
		{Text: "The sentence being spoken at Stop.", AfterBytes: 100000 * audio.FrameBytes},
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run, err := session.Start(ctx, h.deps,
		session.Config{Source: store.SourceMic, LangA: "en-US", LangB: "zh-CN"}, liveSource{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for len(run.Snapshot().Lines) == 0 {
		if ctx.Err() != nil {
			t.Fatal("the first sentence never completed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // the second sentence is under way
	run.Stop()
	<-run.Done()

	d, err := h.store.GetSession(ctx, run.Snapshot().SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, s := range d.Segments {
		texts = append(texts, s.Text)
	}
	if !slices.Equal(texts, []string{"The first sentence.", "The sentence being spoken at Stop."}) {
		t.Fatalf("segments %q: Stop must flush the sentence in progress", texts)
	}
}

func TestInterruptedSessionsAreEndedForRetry(t *testing.T) {
	h := newHarness(t, fakezoom.Script{})
	ctx := context.Background()
	started := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	// open was recording; summarizing and trashed had ended, their recap
	// being written, as Capy quit; done was finished.
	open, done, summarizing, trashed := fmtUUID(1), fmtUUID(2), fmtUUID(4), fmtUUID(5)
	for _, id := range []string{open, done, summarizing, trashed} {
		if err := h.store.CreateSession(ctx, store.Session{ID: id, Mode: store.ModeCaptions, Source: store.SourceMic,
			LangA: "en-US", LangB: "zh-CN", StartedAt: started}); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.store.AppendSegment(ctx, store.Segment{ID: fmtUUID(3), SessionID: open, Lang: "en-US",
		Text: "Said before the crash.", StartMs: 1000, EndMs: 4500}); err != nil {
		t.Fatal(err)
	}
	finished := started.Add(time.Minute)
	for _, id := range []string{done, summarizing, trashed} {
		if err := h.store.EndSession(ctx, id, finished); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.store.SetRecap(ctx, done, store.Recap{Status: store.RecapReady, Title: "Done", Text: "# Done"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.TrashSession(ctx, trashed, "user", ""); err != nil {
		t.Fatal(err)
	}

	if err := session.RecoverInterrupted(ctx, h.store); err != nil {
		t.Fatal(err)
	}
	o, _ := h.store.GetSession(ctx, open)
	if !o.EndedAt.Equal(started.Add(4500*time.Millisecond)) || o.RecapStatus != store.RecapFailed {
		t.Fatalf("interrupted session: ended %v, recap %s; want ended at its last turn, recap failed", o.EndedAt, o.RecapStatus)
	}
	s, _ := h.store.GetSession(ctx, summarizing)
	if !s.EndedAt.Equal(finished) || s.RecapStatus != store.RecapFailed {
		t.Fatalf("session quit while summarizing: ended %v, recap %s; want its end kept, recap failed", s.EndedAt, s.RecapStatus)
	}
	d, _ := h.store.GetSession(ctx, done)
	if !d.EndedAt.Equal(finished) || d.RecapStatus != store.RecapReady {
		t.Fatalf("finished session changed: ended %v, recap %s", d.EndedAt, d.RecapStatus)
	}
	if _, err := h.store.RestoreSession(ctx, trashed, "user", ""); err != nil {
		t.Fatal(err)
	}
	if r, _ := h.store.GetSession(ctx, trashed); r.RecapStatus != store.RecapFailed {
		t.Fatalf("restored session's recap %s, want failed", r.RecapStatus)
	}
}

// fedSource emits the frames a test feeds it, until the test closes it.
type fedSource chan []byte

func (fedSource) Err() error { return nil }

func (f fedSource) Start(context.Context) (<-chan []byte, error) { return f, nil }

func (f fedSource) feed(frames int) {
	for range frames {
		f <- make([]byte, audio.FrameBytes)
	}
}

// waitFor polls until cond holds.
func waitFor(t *testing.T, ctx context.Context, what string, cond func() bool) {
	t.Helper()
	for !cond() {
		if ctx.Err() != nil {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// switchMidway runs a session of two turns in cfg and switches the spoken
// language to code between them, once the first completed. With backlog,
// that many frames are captured while the connection before flushes;
// without, the frames come once the new connection opened.
func switchMidway(t *testing.T, h *harness, cfg session.Config, code string, backlog int) (session.Snapshot, store.SessionDetail) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	src := make(fedSource, 64)
	run, err := session.Start(ctx, h.deps, cfg, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.feed(10)
	waitFor(t, ctx, "the first turn", func() bool { return len(run.Snapshot().Lines) == 1 })
	if err := run.SetSpoken(code); err != nil {
		t.Fatal(err)
	}
	if got := run.Snapshot().Config.LangA; got != code {
		t.Fatalf("snapshot spoken language %s, want %s at once", got, code)
	}
	if backlog > 0 {
		waitFor(t, ctx, "the flush of the connection before", func() bool { return h.fake.Flushes() == 1 })
		src.feed(backlog)
	} else {
		waitFor(t, ctx, "the connection in the new language", func() bool {
			connections, _, _, _ := h.fake.Calls()
			return connections == 2
		})
		src.feed(10)
	}
	close(src)
	select {
	case <-run.Done():
	case <-ctx.Done():
		t.Fatal("session did not finish")
	}
	snap := run.Snapshot()
	d, err := h.store.GetSession(ctx, snap.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	return snap, d
}

func TestSwitchingTheSpokenLanguageMidSession(t *testing.T) {
	h := newHarness(t, fakezoom.Script{Turns: turns("Hello there.", "こんにちは。"), CloseDelay: 200 * time.Millisecond})
	snap, d := switchMidway(t, h, session.Config{Source: store.SourceMic, LangA: "en-US", LangB: "zh-CN", Translate: true}, "ja-JP", 10)

	// Each turn is saved and translated in the language it was spoken in,
	// without a gap: the audio captured while switching waited for the new
	// connection.
	want := []seg{
		{"en-US", "Hello there.", "[zh-CN] Hello there."},
		{"ja-JP", "こんにちは。", "[zh-CN] [en-US] こんにちは。"},
	}
	if got := segs(d); !slices.Equal(got, want) {
		t.Fatalf("segments %+v, want %+v", got, want)
	}
	// The new connection's clock starts where the audio sent before ended:
	// the fake ends a turn at its connection's audio, 900 ms after its start.
	if after := d.Segments[1]; after.StartMs != 1100 || after.EndMs != 2000 {
		t.Fatalf("turn after the switch at %d–%d ms, want 1100–2000", after.StartMs, after.EndMs)
	}
	_, languages, _, _ := h.fake.Calls()
	if !slices.Equal(languages, []string{"en-US", "ja-JP"}) {
		t.Fatalf("live sessions in %v", languages)
	}
	// The session keeps the languages it started with.
	if d.LangA != "en-US" || d.LangB != "zh-CN" || snap.Config.LangB != "zh-CN" {
		t.Fatalf("stored %s → %s, snapshot %+v", d.LangA, d.LangB, snap.Config)
	}
	if snap.Status != session.StatusDone || snap.Err != nil {
		t.Fatalf("status %s, err %v", snap.Status, snap.Err)
	}
}

func TestSwitchingCaptionsWithoutTranslationStaysUntranslated(t *testing.T) {
	h := newHarness(t, fakezoom.Script{Turns: turns("Just captions.", "字幕だけ。")})
	snap, d := switchMidway(t, h, session.Config{Source: store.SourceMic, LangA: "en-US", LangB: "zh-CN"}, "ja-JP", 0)

	want := []seg{{"en-US", "Just captions.", ""}, {"ja-JP", "字幕だけ。", ""}}
	if got := segs(d); !slices.Equal(got, want) {
		t.Fatalf("segments %+v, want %+v", got, want)
	}
	if snap.Config.LangA != "ja-JP" || snap.Config.Translate {
		t.Fatalf("snapshot %+v, want ja-JP captions without translation", snap.Config)
	}
	if failed, err := session.RetryTranslations(context.Background(), h.deps, d.ID); err != nil || failed != 0 {
		t.Fatalf("retry: %d failed, %v", failed, err)
	}
	if _, _, translate, _ := h.fake.Calls(); len(translate) != 0 {
		t.Fatalf("translated %v", translate)
	}
}

func TestSwitchThatOverflowsTheCaptureBufferMarksTheGap(t *testing.T) {
	h := newHarness(t, fakezoom.Script{Turns: turns("Hello there.", "こんにちは。"), CloseDelay: 200 * time.Millisecond})
	// The source's whole buffer fills while switching: capture would drop
	// what comes next.
	_, d := switchMidway(t, h, session.Config{Source: store.SourceMic, LangA: "en-US", LangB: "zh-CN"}, "ja-JP", 64)

	var langs []string
	for _, s := range d.Segments {
		langs = append(langs, s.Lang)
	}
	if !slices.Equal(langs, []string{"en-US", store.GapLang, "ja-JP"}) {
		t.Fatalf("segment languages %v, want a gap before the turn in the new language", langs)
	}
}

func TestAutoDetectsEachTurnsLanguageAndSpeaker(t *testing.T) {
	script := fakezoom.Script{Turns: turns("大家好。", "Hello, everyone.", "Habari.")}
	script.Turns[0].Language, script.Turns[0].Speaker = "zh", "Speaker 1"
	script.Turns[1].Language, script.Turns[1].Speaker = "en", "Speaker 2"
	script.Turns[2].Language = "sw" // not in the catalog
	h := newHarness(t, script)
	snap, d := h.runToEnd(t, session.Config{Source: store.SourceReplay, LangA: "auto", LangB: "en-US", Translate: true}, 35)

	// Each turn is translated from the language detected, unless it was
	// spoken in English or in a language Translator does not know.
	want := []seg{
		{"zh-CN", "大家好。", "[en-US] 大家好。"},
		{"en-US", "Hello, everyone.", ""},
		{"und", "Habari.", ""},
	}
	if got := segs(d); !slices.Equal(got, want) {
		t.Fatalf("segments %+v, want %+v", got, want)
	}
	if d.Segments[0].Speaker != "Speaker 1" || d.Segments[1].Speaker != "Speaker 2" || snap.Lines[1].Speaker != "Speaker 2" {
		t.Fatalf("speakers %+v, lines %+v", d.Segments, snap.Lines)
	}
	for _, l := range snap.Lines {
		if l.Translating || l.TranslateFailed {
			t.Fatalf("line %+v waits for a translation", l)
		}
	}
	_, languages, translate, summarize := h.fake.Calls()
	if !slices.Equal(languages, []string{"auto"}) || !slices.Equal(translate, []string{"zh-CN>en-US"}) ||
		!slices.Equal(summarize, []string{"full_summary/en-US"}) {
		t.Fatalf("calls: live %v, translate %v, summarize %v", languages, translate, summarize)
	}
}

func TestTogglingTranslationAppliesToTheTurnsCompletedAfter(t *testing.T) {
	h := newHarness(t, fakezoom.Script{Turns: turns("Before.", "While on.", "After.")})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	src := make(fedSource, 64)
	run, err := session.Start(ctx, h.deps, session.Config{Source: store.SourceMic, LangA: "en-US", LangB: "ja-JP"}, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, on := range []bool{true, false} {
		src.feed(10)
		waitFor(t, ctx, "a turn", func() bool { return len(run.Snapshot().Lines) == i+1 })
		run.SetTranslate(on)
	}
	src.feed(10)
	close(src)
	<-run.Done()
	d, err := h.store.GetSession(ctx, run.Snapshot().SessionID)
	if err != nil {
		t.Fatal(err)
	}

	want := []seg{{"en-US", "Before.", ""}, {"en-US", "While on.", "[ja-JP] While on."}, {"en-US", "After.", ""}}
	if got := segs(d); !slices.Equal(got, want) {
		t.Fatalf("segments %+v, want %+v", got, want)
	}
	// Lines said with translation off await no translation: nothing to retry.
	if failed, err := session.RetryTranslations(ctx, h.deps, d.ID); err != nil || failed != 0 {
		t.Fatalf("retry: %d failed, %v", failed, err)
	}
	connections, _, translate, summarize := h.fake.Calls()
	if connections != 1 || !slices.Equal(translate, []string{"en-US>ja-JP"}) || !slices.Equal(summarize, []string{"full_summary/ja-JP"}) {
		t.Fatalf("calls: %d connections, translate %v, summarize %v", connections, translate, summarize)
	}
}

func TestTranslationUnderWayCompletesWhenTurnedOff(t *testing.T) {
	h := newHarness(t, fakezoom.Script{Turns: turns("Being translated."), TranslateDelay: 300 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	src := make(fedSource, 64)
	run, err := session.Start(ctx, h.deps, session.Config{Source: store.SourceMic, LangA: "en-US", LangB: "ja-JP", Translate: true}, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.feed(10)
	waitFor(t, ctx, "the turn", func() bool { return len(run.Snapshot().Lines) == 1 })
	run.SetTranslate(false)
	close(src)
	<-run.Done()
	if l := run.Snapshot().Lines[0]; l.Translation != "[ja-JP] Being translated." || l.Translating {
		t.Fatalf("line %+v: the translation under way must complete", l)
	}
}

func TestChangingTheTargetAppliesToTheTurnsCompletedAfter(t *testing.T) {
	h := newHarness(t, fakezoom.Script{Turns: turns("Before.", "After.", "Still after.")})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	src := make(fedSource, 64)
	run, err := session.Start(ctx, h.deps, session.Config{Source: store.SourceMic, LangA: "en-US", LangB: "ja-JP", Translate: true}, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	src.feed(10)
	waitFor(t, ctx, "the first turn", func() bool { return len(run.Snapshot().Lines) == 1 })
	if err := run.SetTarget("de-DE"); err != nil {
		t.Fatal(err)
	}
	if got := run.Snapshot().Config.LangB; got != "de-DE" {
		t.Fatalf("snapshot target %s, want de-DE at once", got)
	}
	src.feed(20)
	waitFor(t, ctx, "the turns after", func() bool { return len(run.Snapshot().Lines) == 3 })
	// The session is saved with the target it translates into now.
	if d, err := h.store.GetSession(ctx, run.Snapshot().SessionID); err != nil || d.LangB != "de-DE" {
		t.Fatalf("while recording: stored target %s, %v; want de-DE", d.LangB, err)
	}
	close(src)
	<-run.Done()
	if err := run.SetTarget("fr-FR"); err != nil || run.Snapshot().Config.LangB != "de-DE" {
		t.Fatalf("after the end: %v, target %s; want de-DE kept", err, run.Snapshot().Config.LangB)
	}
	d, err := h.store.GetSession(ctx, run.Snapshot().SessionID)
	if err != nil {
		t.Fatal(err)
	}

	want := []seg{
		{"en-US", "Before.", "[ja-JP] Before."},
		{"en-US", "After.", "[de-DE] After."},
		{"en-US", "Still after.", "[de-DE] Still after."},
	}
	if got := segs(d); !slices.Equal(got, want) {
		t.Fatalf("segments %+v, want %+v", got, want)
	}
	// No reconnect, and the recap is in the target chosen last.
	connections, _, translate, summarize := h.fake.Calls()
	slices.Sort(translate) // translations run concurrently
	if connections != 1 || !slices.Equal(translate, []string{"en-US>de-DE", "en-US>de-DE", "en-US>ja-JP"}) ||
		!slices.Equal(summarize, []string{"full_summary/de-DE"}) {
		t.Fatalf("calls: %d connections, translate %v, summarize %v", connections, translate, summarize)
	}
	if err := run.SetTarget("auto"); err == nil {
		t.Fatal("auto accepted as a target")
	}
}

func TestALiveSessionHeartbeatsUntilItEnds(t *testing.T) {
	defer session.SetHeartbeatEvery(50 * time.Millisecond)()
	h := newHarness(t, fakezoom.Script{Turns: turns("Still here.")})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run, err := session.Start(ctx, h.deps, session.Config{Source: store.SourceReplay, LangA: "en-US", LangB: "zh-CN"},
		audio.NewWAV(silence(t, 20), true), nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	id := run.Snapshot().SessionID
	// While it records, the library has its heartbeat, beat after beat: it
	// is live, not a session a crash left open.
	var first time.Time
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		d, err := h.store.GetSession(ctx, id)
		if err == nil && d.EndedAt.IsZero() && !d.HeartbeatAt.IsZero() {
			if first.IsZero() {
				first = d.HeartbeatAt
			} else if d.HeartbeatAt.After(first) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no second heartbeat while recording: first %v, now %+v, %v", first, d.SessionSummary, err)
		}
	}
	<-run.Done()
	d, err := h.store.GetSession(ctx, id)
	if err != nil || d.EndedAt.IsZero() || d.HeartbeatAt.After(d.EndedAt) {
		t.Fatalf("ended: heartbeat %v, ended %v, %v; want the last heartbeat before the end", d.HeartbeatAt, d.EndedAt, err)
	}
}
