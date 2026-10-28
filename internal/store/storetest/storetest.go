// Package storetest is the contract suite every store.Store adapter must pass.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/onyx-dot-app/Capy/internal/store"

	"github.com/google/uuid"
)

var base = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

// Run runs the contract suite. open must return a fresh, empty store and
// register its own cleanup.
func Run(t *testing.T, open func(t *testing.T) store.Store) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(t *testing.T, s store.Store)
	}{
		{"SegmentsOrderedBySeq", segmentsOrderedBySeq},
		{"DuplicateSeqAndIDConflict", duplicateConflicts},
		{"TranslationVisible", translationVisible},
		{"RecapAndEndVisibleInList", recapAndEnd},
		{"FileSessionKeepsItsFileName", fileName},
		{"PurgedSessionHidden", purgedHidden},
		{"WritesToMissingOrPurgedSession", writesToMissing},
		{"TrashedSessionHidden", trashedHidden},
		{"TrashRefusedBeforeTheSessionEnds", trashRefusedWhileRecording},
		{"RestoreReturnsTheSession", restore},
		{"RenameWinsOverTheRecapTitle", renameWins},
		{"EditAndUndo", editAndUndo},
		{"UndoRefusedAfterALaterChange", undoRefusedAfterChange},
		{"UndoTrashRestores", undoTrash},
		{"PurgeErasesEdits", purgeErasesEdits},
		{"ExpiredAfterTheRetention", expired},
		{"Heartbeat", heartbeat},
		{"TargetChangesWhileRecording", targetWhileRecording},
		{"GapSegmentsNotCounted", gapNotCounted},
		{"ListOrderAndPagination", listPagination},
		{"Search", search},
		{"SearchHidesPurgedAndTrashedSessions", searchHidesRemoved},
		{"SearchSpecialCharactersAreInert", searchSpecial},
		{"SearchRange", searchRange},
		{"Validation", validation},
	}
	for _, c := range append(append(cases, batchCases...), highlightCases...) {
		t.Run(c.name, func(t *testing.T) { c.fn(t, open(t)) })
	}
}

func newSession(t *testing.T, s store.Store, startedAt time.Time) store.Session {
	t.Helper()
	sess := store.Session{
		ID: uuid.NewString(), Mode: store.ModeCaptions, Source: store.SourceMic,
		LangA: "en-US", LangB: "zh-CN", StartedAt: startedAt,
	}
	if err := s.CreateSession(context.Background(), sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return sess
}

func segment(sessionID string, seq int, text string) store.Segment {
	return store.Segment{
		ID: uuid.NewString(), SessionID: sessionID, Seq: seq, Lang: "en-US", Text: text,
		StartMs: int64(seq) * 1000, EndMs: int64(seq)*1000 + 900,
	}
}

func mustAppend(t *testing.T, s store.Store, seg store.Segment) {
	t.Helper()
	if err := s.AppendSegment(context.Background(), seg); err != nil {
		t.Fatalf("AppendSegment(seq %d): %v", seg.Seq, err)
	}
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func sameIDs(t *testing.T, got []store.SessionSummary, want ...string) {
	t.Helper()
	ids := make([]string, len(got))
	for i, s := range got {
		ids[i] = s.ID
	}
	if fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("session ids = %v, want %v", ids, want)
	}
}

func segmentsOrderedBySeq(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := newSession(t, s, base)
	for _, seq := range []int{2, 0, 1} {
		seg := segment(sess.ID, seq, fmt.Sprintf("line %d", seq))
		seg.Speaker = fmt.Sprintf("Speaker %d", seq)
		mustAppend(t, s, seg)
	}
	d, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Segments) != 3 {
		t.Fatalf("got %d segments", len(d.Segments))
	}
	for i, g := range d.Segments {
		if g.Seq != i || g.Text != fmt.Sprintf("line %d", i) || g.Speaker != fmt.Sprintf("Speaker %d", i) {
			t.Fatalf("segment %d = seq %d text %q speaker %q", i, g.Seq, g.Text, g.Speaker)
		}
	}
	if !d.StartedAt.Equal(base) || d.LangA != "en-US" || d.LangB != "zh-CN" || d.Mode != store.ModeCaptions || d.Source != store.SourceMic {
		t.Fatalf("session fields lost: %+v", d.SessionSummary)
	}
	if d.RecapStatus != store.RecapPending || !d.EndedAt.IsZero() {
		t.Fatalf("new session: status %q ended %v", d.RecapStatus, d.EndedAt)
	}
}

func duplicateConflicts(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := newSession(t, s, base)
	first := segment(sess.ID, 0, "one")
	mustAppend(t, s, first)

	wantErr(t, s.AppendSegment(ctx, segment(sess.ID, 0, "same seq")), store.ErrConflict)
	dupID := segment(sess.ID, 1, "same id")
	dupID.ID = first.ID
	wantErr(t, s.AppendSegment(ctx, dupID), store.ErrConflict)
	wantErr(t, s.CreateSession(ctx, sess), store.ErrConflict)

	// A seq may repeat across sessions.
	other := newSession(t, s, base.Add(time.Minute))
	mustAppend(t, s, segment(other.ID, 0, "other"))

	d, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Segments) != 1 || d.Segments[0].Text != "one" {
		t.Fatalf("rejected writes changed the transcript: %+v", d.Segments)
	}
}

func translationVisible(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := newSession(t, s, base)
	a, b := segment(sess.ID, 0, "hello"), segment(sess.ID, 1, "bye")
	mustAppend(t, s, a)
	mustAppend(t, s, b)
	if d, err := s.GetSession(ctx, sess.ID); err != nil || d.Translated {
		t.Fatalf("a session without translations: translated %v, %v", d.Translated, err)
	}
	// A turn appended to be translated is awaited in its language: the
	// session counts as translated before the translation arrives.
	awaited := segment(sess.ID, 3, "later")
	awaited.TranslationLang = "zh-CN"
	mustAppend(t, s, awaited)
	list, err := s.ListSessions(ctx, store.ListQuery{})
	if err != nil || len(list) != 1 || !list[0].Translated {
		t.Fatalf("list: %+v, %v; want the session translated", list, err)
	}
	if err := s.SetTranslation(ctx, a.ID, "zh-CN", "你好"); err != nil {
		t.Fatal(err)
	}
	d, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Segments[0]; got.Translation != "你好" || got.TranslationLang != "zh-CN" || got.Text != "hello" {
		t.Fatalf("segment 0 = %+v", got)
	}
	if got := d.Segments[1]; got.Translation != "" || got.TranslationLang != "" {
		t.Fatalf("untranslated segment changed: %+v", got)
	}
	// A translation supplied with the segment is stored too.
	pre := segment(sess.ID, 2, "thanks")
	pre.Translation, pre.TranslationLang = "谢谢", "zh-CN"
	mustAppend(t, s, pre)
	d, err = s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Segments[2].Translation != "谢谢" || !d.Translated {
		t.Fatalf("appended translation lost: %+v", d.Segments[2])
	}
	if got := d.Segments[3]; got.TranslationLang != "zh-CN" || got.Translation != "" {
		t.Fatalf("awaited translation: %+v", got)
	}
	wantErr(t, s.SetTranslation(ctx, uuid.NewString(), "zh-CN", "x"), store.ErrNotFound)
}

func recapAndEnd(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := newSession(t, s, base)
	ended := base.Add(5 * time.Minute)
	if err := s.EndSession(ctx, sess.ID, ended); err != nil {
		t.Fatal(err)
	}
	recap := store.Recap{Status: store.RecapReady, Title: "Weekly sync", Text: "Weekly sync\n- ship it"}
	if err := s.SetRecap(ctx, sess.ID, recap); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSessions(ctx, store.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Title != "Weekly sync" || list[0].RecapStatus != store.RecapReady || !list[0].EndedAt.Equal(ended) {
		t.Fatalf("list = %+v", list)
	}
	d, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Recap != recap.Text {
		t.Fatalf("recap text = %q", d.Recap)
	}
	// A failed retry replaces the previous recap.
	if err := s.SetRecap(ctx, sess.ID, store.Recap{Status: store.RecapFailed}); err != nil {
		t.Fatal(err)
	}
	d, err = s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.RecapStatus != store.RecapFailed || d.Title != "" || d.Recap != "" {
		t.Fatalf("after failed recap: %+v", d)
	}
}

func fileName(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := store.Session{ID: uuid.NewString(), Mode: store.ModeCaptions, Source: store.SourceFile,
		LangA: "ja-JP", LangB: "ja-JP", StartedAt: base, FileName: "週次ミーティング.m4a", BatchID: uuid.NewString(),
		TrackingID: "v=2.0;clid=us01;rid=WEB_f2a7c3164c9c0aa134f59fedda450c5e"}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	d, err := s.GetSession(ctx, sess.ID)
	if err != nil || d.Source != store.SourceFile || d.FileName != sess.FileName || d.BatchID != sess.BatchID ||
		d.TrackingID != sess.TrackingID {
		t.Fatalf("GetSession = %+v (tracking ID %q), %v", d.SessionSummary, d.TrackingID, err)
	}
	list, err := s.ListSessions(ctx, store.ListQuery{})
	if err != nil || len(list) != 1 || list[0].FileName != sess.FileName || list[0].BatchID != sess.BatchID {
		t.Fatalf("ListSessions = %+v, %v", list, err)
	}
	// Names macOS gives are kept: a screen recording's, with a narrow
	// space before PM, and an emoji joined.
	for _, name := range []string{"Screen Recording 2026-10-19 at 10.15.32\u202fPM.mov", "Team 👩\u200d💻 sync.m4a"} {
		if err := s.CreateSession(ctx, store.Session{ID: uuid.NewString(), Mode: store.ModeCaptions, Source: store.SourceFile,
			LangA: "en-US", LangB: "en-US", StartedAt: base, FileName: name}); err != nil {
			t.Errorf("file name %q: %v", name, err)
		}
	}
	// A long name in a script of three bytes a character is a name macOS
	// allows: it is kept.
	long := store.Session{ID: uuid.NewString(), Mode: store.ModeCaptions, Source: store.SourceFile,
		LangA: "zh-CN", LangB: "zh-CN", StartedAt: base, FileName: strings.Repeat("会", 250) + ".m4a"}
	if err := s.CreateSession(ctx, long); err != nil {
		t.Fatalf("a long file name: %v", err)
	}
	// A recording has none.
	rec := newSession(t, s, base.Add(time.Minute))
	if d, err := s.GetSession(ctx, rec.ID); err != nil || d.FileName != "" || d.TrackingID != "" {
		t.Fatalf("recording's file name = %q, tracking ID %q, %v", d.FileName, d.TrackingID, err)
	}
}

func purgedHidden(t *testing.T, s store.Store) {
	ctx := context.Background()
	keep := ended(t, s, base)
	gone := ended(t, s, base.Add(time.Minute))
	mustAppend(t, s, segment(gone.ID, 0, "secret roadmap"))
	mustAppend(t, s, segment(keep.ID, 0, "public roadmap"))

	// Only a session in Recently Deleted is purged.
	wantErr(t, s.PurgeSession(ctx, keep.ID), store.ErrNotFound)
	mustTrash(t, s, gone.ID)
	if err := s.PurgeSession(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSessions(ctx, store.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, list, keep.ID)
	_, err = s.GetSession(ctx, gone.ID)
	wantErr(t, err, store.ErrNotFound)
	hits, err := s.Search(ctx, "roadmap", 0)
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, hits, keep.ID)
	wantErr(t, s.PurgeSession(ctx, gone.ID), store.ErrNotFound)
	wantErr(t, s.PurgeSession(ctx, uuid.NewString()), store.ErrNotFound)
}

func writesToMissing(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := newSession(t, s, base)
	seg := segment(sess.ID, 0, "x")
	mustAppend(t, s, seg)
	if err := s.EndSession(ctx, sess.ID, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	mustTrash(t, s, sess.ID)
	if err := s.PurgeSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	_, renameErr := s.RenameSession(ctx, sess.ID, "x", "user", "")
	_, recapErr := s.UpdateRecap(ctx, sess.ID, "x", "user", "")
	_, trashErr := s.TrashSession(ctx, sess.ID, "user", "")
	_, restoreErr := s.RestoreSession(ctx, sess.ID, "user", "")
	for name, err := range map[string]error{
		"append":      s.AppendSegment(ctx, segment(sess.ID, 1, "late")),
		"translation": s.SetTranslation(ctx, seg.ID, "zh-CN", "x"),
		"recap":       s.SetRecap(ctx, sess.ID, store.Recap{Status: store.RecapReady}),
		"end":         s.EndSession(ctx, sess.ID, base),
		"heartbeat":   s.Heartbeat(ctx, sess.ID, base),
		"target":      s.SetTarget(ctx, sess.ID, "ja-JP"),
		"rename":      renameErr,
		"edit recap":  recapErr,
		"trash":       trashErr,
		"restore":     restoreErr,
	} {
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s on purged session: error = %v, want ErrNotFound", name, err)
		}
	}
	missing := uuid.NewString()
	wantErr(t, s.AppendSegment(ctx, segment(missing, 0, "x")), store.ErrNotFound)
	wantErr(t, s.SetRecap(ctx, missing, store.Recap{Status: store.RecapReady}), store.ErrNotFound)
	wantErr(t, s.EndSession(ctx, missing, base), store.ErrNotFound)
}

func gapNotCounted(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := newSession(t, s, base)
	mustAppend(t, s, segment(sess.ID, 0, "before"))
	gap := segment(sess.ID, 1, "")
	gap.Lang = store.GapLang
	mustAppend(t, s, gap)
	mustAppend(t, s, segment(sess.ID, 2, "after"))

	list, err := s.ListSessions(ctx, store.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if list[0].SegmentCount != 2 {
		t.Fatalf("SegmentCount = %d, want 2", list[0].SegmentCount)
	}
	d, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.SegmentCount != 2 || len(d.Segments) != 3 || d.Segments[1].Lang != store.GapLang {
		t.Fatalf("detail: count %d, %d segments", d.SegmentCount, len(d.Segments))
	}
}

func listPagination(t *testing.T, s store.Store) {
	ctx := context.Background()
	oldest := newSession(t, s, base)
	newest := newSession(t, s, base.Add(2*time.Hour))
	middle := newSession(t, s, base.Add(time.Hour))
	// Same start time: id breaks the tie, descending.
	tieA := newSession(t, s, base.Add(30*time.Minute))
	tieB := newSession(t, s, base.Add(30*time.Minute))
	tieHi, tieLo := tieA.ID, tieB.ID
	if tieLo > tieHi {
		tieHi, tieLo = tieLo, tieHi
	}

	all, err := s.ListSessions(ctx, store.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, all, newest.ID, middle.ID, tieHi, tieLo, oldest.ID)

	page, err := s.ListSessions(ctx, store.ListQuery{Limit: 2, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, page, middle.ID, tieHi)
	page, err = s.ListSessions(ctx, store.ListQuery{Limit: 2, Offset: 4})
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, page, oldest.ID)
	page, err = s.ListSessions(ctx, store.ListQuery{Offset: 5})
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, page)
}

func search(t *testing.T, s store.Store) {
	ctx := context.Background()
	en := newSession(t, s, base)
	zh := newSession(t, s, base.Add(time.Hour))
	enSeg := segment(en.ID, 0, "Quarterly budget review")
	mustAppend(t, s, enSeg)
	mustAppend(t, s, segment(en.ID, 1, "next steps"))
	zhSeg := segment(zh.ID, 0, "今天的会议很重要")
	mustAppend(t, s, zhSeg)
	if err := s.SetTranslation(ctx, enSeg.ID, "zh-CN", "季度预算评审"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTranslation(ctx, zhSeg.ID, "en-US", "Today's meeting matters"); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, query string
		want        []string
	}{
		{"latin word, case-insensitive", "BUDGET", []string{en.ID}},
		{"latin prefix of last word", "quarterly bud", []string{en.ID}},
		{"all words must match in one segment", "budget steps", nil},
		{"cjk three characters", "今天的会议", []string{zh.ID}},
		{"cjk two characters (below trigram size)", "会议", []string{zh.ID}},
		{"cjk single character", "重", []string{zh.ID}},
		{"short latin word", "of", nil},
		{"short and long words together", "next st", []string{en.ID}},
		{"translation of cjk segment", "meeting", []string{zh.ID}},
		{"cjk translation", "预算评审", []string{en.ID}},
		{"no match", "nonexistent", nil},
		{"blank query", "   ", nil},
		{"empty query", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.Search(ctx, c.query, 0)
			if err != nil {
				t.Fatal(err)
			}
			sameIDs(t, got, c.want...)
		})
	}

	// A translation written after the segment is searchable, and a replaced
	// one no longer is.
	if err := s.SetTranslation(ctx, enSeg.ID, "de-DE", "Quartalsbudget"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Search(ctx, "Quartalsbudget", 0)
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, got, en.ID)
	got, err = s.Search(ctx, "预算评审", 0)
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, got)

	// Results are distinct sessions, newest first, honouring the limit.
	mustAppend(t, s, segment(zh.ID, 1, "budget again"))
	mustAppend(t, s, segment(zh.ID, 2, "budget thrice"))
	got, err = s.Search(ctx, "budget", 0)
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, got, zh.ID, en.ID)
	got, err = s.Search(ctx, "budget", 1)
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, got, zh.ID)
}

func searchHidesRemoved(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, remove := range []func(id string) error{
		func(id string) error {
			if _, err := s.TrashSession(ctx, id, "user", ""); err != nil {
				return err
			}
			return s.PurgeSession(ctx, id)
		},
		func(id string) error { _, err := s.TrashSession(ctx, id, "user", ""); return err },
	} {
		sess := ended(t, s, base)
		seg := segment(sess.ID, 0, "findable phrase")
		mustAppend(t, s, seg)
		if err := s.SetTranslation(ctx, seg.ID, "zh-CN", "可找到的短语"); err != nil {
			t.Fatal(err)
		}
		if err := remove(sess.ID); err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{"findable", "可找到的", "短语"} {
			got, err := s.Search(ctx, q, 0)
			if err != nil {
				t.Fatal(err)
			}
			sameIDs(t, got)
		}
	}
}

func searchSpecial(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := newSession(t, s, base)
	mustAppend(t, s, segment(sess.ID, 0, `she said "hello" AND (left) 50% off_now`))
	mustAppend(t, s, segment(sess.ID, 1, "plain words"))

	for _, q := range []string{`"`, `""`, `*`, `AND`, `OR NOT`, `(`, `)`, `a AND`, `"unterminated`, `foo*`, `NEAR(a b)`, `col:x`, `-x`, `^x`, `%`, `_`, `\`} {
		if _, err := s.Search(ctx, q, 0); err != nil {
			t.Errorf("Search(%q) error: %v", q, err)
		}
	}
	for q, want := range map[string]bool{
		`"hello"`:     true,
		`AND (left`:   true,
		`50%`:         true,
		`off_now`:     true,
		`%`:           true,
		`hello*`:      false, // operators are literal text, not syntax
		`o_`:          false, // LIKE wildcards are literal too
		`plain words`: true,
		`plain AND`:   false, // AND is a word to find, not an operator
	} {
		got, err := s.Search(ctx, q, 0)
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		if (len(got) == 1) != want {
			t.Errorf("Search(%q) found %d sessions, want match=%v", q, len(got), want)
		}
	}
}

func validation(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := newSession(t, s, base)
	good := segment(sess.ID, 0, "ok")

	badSession := func(mut func(*store.Session)) store.Session {
		v := store.Session{ID: uuid.NewString(), Mode: store.ModeConversation, Source: store.SourceSystem, LangA: "en-US", LangB: "es-ES", StartedAt: base}
		mut(&v)
		return v
	}
	for name, v := range map[string]store.Session{
		"id":                       badSession(func(v *store.Session) { v.ID = "not-a-uuid" }),
		"mode":                     badSession(func(v *store.Session) { v.Mode = "karaoke" }),
		"source":                   badSession(func(v *store.Session) { v.Source = "" }),
		"lang a":                   badSession(func(v *store.Session) { v.LangA = "" }),
		"lang b":                   badSession(func(v *store.Session) { v.LangB = "" }),
		"started":                  badSession(func(v *store.Session) { v.StartedAt = time.Time{} }),
		"file name of a recording": badSession(func(v *store.Session) { v.FileName = "a.wav" }),
		"file without a name":      badSession(func(v *store.Session) { v.Source = store.SourceFile }),
		"file name with folders": badSession(func(v *store.Session) {
			v.Source, v.FileName = store.SourceFile, "Downloads/a.wav"
		}),
		"file name on two lines": badSession(func(v *store.Session) { v.Source, v.FileName = store.SourceFile, "a\nb.wav" }),
		"file name reordered": badSession(func(v *store.Session) {
			v.Source, v.FileName = store.SourceFile, "invoice\u202ewav.exe"
		}),
		"file without a language": badSession(func(v *store.Session) {
			v.Source, v.FileName, v.LangA = store.SourceFile, "a.wav", ""
		}),
		"file without a start": badSession(func(v *store.Session) {
			v.Source, v.FileName, v.StartedAt = store.SourceFile, "a.wav", time.Time{}
		}),
		"batch of a recording": badSession(func(v *store.Session) { v.BatchID = uuid.NewString() }),
		"batch not a UUID": badSession(func(v *store.Session) {
			v.Source, v.FileName, v.BatchID = store.SourceFile, "a.wav", "batch-1"
		}),
		"tracking id of a recording": badSession(func(v *store.Session) { v.TrackingID = "WEB_1" }),
		"tracking id on two lines": badSession(func(v *store.Session) {
			v.Source, v.FileName, v.TrackingID = store.SourceFile, "a.wav", "WEB_1\nWEB_2"
		}),
		"tracking id too long": badSession(func(v *store.Session) {
			v.Source, v.FileName, v.TrackingID = store.SourceFile, "a.wav", strings.Repeat("a", store.MaxTrackingIDLen+1)
		}),
	} {
		if err := s.CreateSession(ctx, v); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("CreateSession with bad %s: error = %v, want ErrInvalid", name, err)
		}
	}

	badSegment := func(mut func(*store.Segment)) store.Segment {
		v := segment(sess.ID, 5, "ok")
		mut(&v)
		return v
	}
	tooLong := make([]byte, store.MaxSegmentText+1)
	for i := range tooLong {
		tooLong[i] = 'a'
	}
	for name, v := range map[string]store.Segment{
		"id":               badSegment(func(v *store.Segment) { v.ID = "" }),
		"session id":       badSegment(func(v *store.Segment) { v.SessionID = "x" }),
		"seq":              badSegment(func(v *store.Segment) { v.Seq = -1 }),
		"start > end":      badSegment(func(v *store.Segment) { v.StartMs, v.EndMs = 10, 5 }),
		"negative start":   badSegment(func(v *store.Segment) { v.StartMs = -1 }),
		"lang":             badSegment(func(v *store.Segment) { v.Lang = "" }),
		"empty text":       badSegment(func(v *store.Segment) { v.Text = "" }),
		"gap with text":    badSegment(func(v *store.Segment) { v.Lang = store.GapLang }),
		"long text":        badSegment(func(v *store.Segment) { v.Text = string(tooLong) }),
		"invalid utf-8":    badSegment(func(v *store.Segment) { v.Text = "\xff\xfe" }),
		"translation lang": badSegment(func(v *store.Segment) { v.Translation = "x" }),
		"long speaker":     badSegment(func(v *store.Segment) { v.Speaker = string(tooLong[:store.MaxSpeakerLen+1]) }),
	} {
		if err := s.AppendSegment(ctx, v); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("AppendSegment with bad %s: error = %v, want ErrInvalid", name, err)
		}
	}

	wantErr(t, s.SetTranslation(ctx, "x", "zh-CN", "t"), store.ErrInvalid)
	wantErr(t, s.SetTranslation(ctx, good.ID, "", "t"), store.ErrInvalid)
	wantErr(t, s.SetTranslation(ctx, good.ID, "zh-CN", ""), store.ErrInvalid)
	wantErr(t, s.SetRecap(ctx, "x", store.Recap{Status: store.RecapReady}), store.ErrInvalid)
	wantErr(t, s.SetRecap(ctx, sess.ID, store.Recap{Status: "done"}), store.ErrInvalid)
	wantErr(t, s.EndSession(ctx, "x", base), store.ErrInvalid)
	wantErr(t, s.EndSession(ctx, sess.ID, time.Time{}), store.ErrInvalid)
	wantErr(t, s.PurgeSession(ctx, "x"), store.ErrInvalid)
	wantErr(t, s.Heartbeat(ctx, "x", base), store.ErrInvalid)
	wantErr(t, s.SetTarget(ctx, "x", "ja-JP"), store.ErrInvalid)
	wantErr(t, s.SetTarget(ctx, sess.ID, ""), store.ErrInvalid)
	for name, edit := range map[string]func() (store.Edit, error){
		"session id":      func() (store.Edit, error) { return s.RenameSession(ctx, "x", "t", "user", "") },
		"title newline":   func() (store.Edit, error) { return s.RenameSession(ctx, sess.ID, "two\nlines", "user", "") },
		"title bidi":      func() (store.Edit, error) { return s.RenameSession(ctx, sess.ID, "Q3 \u202egnirts", "agent", "") },
		"title invisible": func() (store.Edit, error) { return s.RenameSession(ctx, sess.ID, "a\u200bb", "agent", "") },
		"title too long": func() (store.Edit, error) {
			return s.RenameSession(ctx, sess.ID, string(tooLong[:store.MaxTitleLen+1]), "user", "")
		},
		"no actor":      func() (store.Edit, error) { return s.RenameSession(ctx, sess.ID, "t", "", "") },
		"actor control": func() (store.Edit, error) { return s.RenameSession(ctx, sess.ID, "t", "user\x00", "") },
		"origin too long": func() (store.Edit, error) {
			return s.RenameSession(ctx, sess.ID, "t", "agent", string(tooLong[:store.MaxTagLen+1]))
		},
		"recap too long": func() (store.Edit, error) {
			return s.UpdateRecap(ctx, sess.ID, string(make([]byte, store.MaxRecapText+1)), "user", "")
		},
		"trash actor": func() (store.Edit, error) { return s.TrashSession(ctx, sess.ID, "", "") },
		"undo id":     func() (store.Edit, error) { return s.UndoEdit(ctx, "x") },
	} {
		if _, err := edit(); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("edit with bad %s: error = %v, want ErrInvalid", name, err)
		}
	}
	_, err := s.ListEdits(ctx, "")
	wantErr(t, err, store.ErrInvalid)
	_, err = s.GetSession(ctx, "x")
	wantErr(t, err, store.ErrInvalid)
	_, err = s.ListSessions(ctx, store.ListQuery{Limit: -1})
	wantErr(t, err, store.ErrInvalid)
	_, err = s.ListSessions(ctx, store.ListQuery{Offset: -1})
	wantErr(t, err, store.ErrInvalid)
	_, err = s.Search(ctx, "x", -1)
	wantErr(t, err, store.ErrInvalid)
	_, err = s.Search(ctx, string(tooLong[:store.MaxSearchQueryLen+1]), 0)
	wantErr(t, err, store.ErrInvalid)

	// No rejected write left anything behind.
	d, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Segments) != 0 || d.RecapStatus != store.RecapPending {
		t.Fatalf("rejected writes changed the session: %+v", d)
	}
	list, err := s.ListSessions(ctx, store.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("rejected CreateSession stored %d sessions", len(list))
	}
}

// ended creates a session that has ended, which may be trashed.
func ended(t *testing.T, s store.Store, startedAt time.Time) store.Session {
	t.Helper()
	sess := newSession(t, s, startedAt)
	if err := s.EndSession(context.Background(), sess.ID, startedAt.Add(time.Minute)); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	return sess
}

func mustTrash(t *testing.T, s store.Store, id string) store.Edit {
	t.Helper()
	e, err := s.TrashSession(context.Background(), id, "user", "")
	if err != nil {
		t.Fatalf("TrashSession: %v", err)
	}
	return e
}

func trashedIDs(t *testing.T, s store.Store) []store.TrashedSession {
	t.Helper()
	list, err := s.ListTrashed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func title(t *testing.T, s store.Store, id string) string {
	t.Helper()
	d, err := s.GetSession(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	return d.Title
}

func trashedHidden(t *testing.T, s store.Store) {
	ctx := context.Background()
	keep := ended(t, s, base)
	gone := ended(t, s, base.Add(time.Minute))
	seg := segment(gone.ID, 0, "kept while trashed")
	mustAppend(t, s, seg)
	before := time.Now().Add(-time.Second)
	mustTrash(t, s, gone.ID)

	list, err := s.ListSessions(ctx, store.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, list, keep.ID)
	_, err = s.GetSession(ctx, gone.ID)
	wantErr(t, err, store.ErrNotFound)
	trashed := trashedIDs(t, s)
	if len(trashed) != 1 || trashed[0].ID != gone.ID || trashed[0].TrashedAt.Before(before) || trashed[0].SegmentCount != 1 {
		t.Fatalf("Recently Deleted = %+v; want the trashed session with its text and when", trashed)
	}
	// Edits to it are refused, as to a missing session.
	_, renameErr := s.RenameSession(ctx, gone.ID, "x", "user", "")
	_, recapErr := s.UpdateRecap(ctx, gone.ID, "x", "user", "")
	_, trashErr := s.TrashSession(ctx, gone.ID, "user", "")
	for name, err := range map[string]error{"append": s.AppendSegment(ctx, segment(gone.ID, 1, "late")),
		"rename": renameErr, "edit recap": recapErr, "trash again": trashErr} {
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s on a trashed session: error = %v, want ErrNotFound", name, err)
		}
	}
	// The recap and translations still being written as it was trashed
	// are kept, for a restore.
	if err := s.SetRecap(ctx, gone.ID, store.Recap{Status: store.RecapReady, Title: "Late recap", Text: "# Late recap"}); err != nil {
		t.Fatalf("SetRecap on a trashed session: %v", err)
	}
	if err := s.SetTranslation(ctx, seg.ID, "zh-CN", "译文"); err != nil {
		t.Fatalf("SetTranslation on a trashed session: %v", err)
	}
	if _, err := s.RestoreSession(ctx, gone.ID, "user", ""); err != nil {
		t.Fatal(err)
	}
	d, err := s.GetSession(ctx, gone.ID)
	if err != nil || d.Title != "Late recap" || d.Segments[0].Translation != "译文" {
		t.Fatalf("restored: %+v, %v; want the recap and translation written while trashed", d, err)
	}
}

func trashRefusedWhileRecording(t *testing.T, s store.Store) {
	ctx := context.Background()
	live := newSession(t, s, base)
	_, err := s.TrashSession(ctx, live.ID, "agent", "run")
	wantErr(t, err, store.ErrConflict)
	list, err := s.ListSessions(ctx, store.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, list, live.ID)
	if n := len(trashedIDs(t, s)); n != 0 {
		t.Fatalf("a refused trash left %d sessions in Recently Deleted", n)
	}
	if edits, err := s.ListEdits(ctx, "run"); err != nil || len(edits) != 0 {
		t.Fatalf("a refused trash recorded edits %+v, %v", edits, err)
	}
}

func restore(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := ended(t, s, base)
	_, err := s.RestoreSession(ctx, sess.ID, "user", "")
	wantErr(t, err, store.ErrNotFound) // not in Recently Deleted
	mustTrash(t, s, sess.ID)
	e, err := s.RestoreSession(ctx, sess.ID, "user", "")
	if err != nil || e.Field != store.FieldTrash || e.NewValue != "" || e.OldValue == "" {
		t.Fatalf("restore: %+v, %v", e, err)
	}
	list, err := s.ListSessions(ctx, store.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, list, sess.ID)
	if n := len(trashedIDs(t, s)); n != 0 {
		t.Fatalf("restored, %d sessions remain in Recently Deleted", n)
	}
	mustTrash(t, s, sess.ID) // and it may be trashed again
}

func renameWins(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := ended(t, s, base)
	recap := func(title string) {
		t.Helper()
		if err := s.SetRecap(ctx, sess.ID, store.Recap{Status: store.RecapReady, Title: title, Text: "# " + title}); err != nil {
			t.Fatal(err)
		}
	}
	recap("Weekly sync")
	if _, err := s.RenameSession(ctx, sess.ID, "Roadmap review", "user", ""); err != nil {
		t.Fatal(err)
	}
	recap("Retried recap") // Retry Recap keeps the rename
	list, err := s.ListSessions(ctx, store.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Title != "Roadmap review" || title(t, s, sess.ID) != "Roadmap review" {
		t.Fatalf("after a new recap: list %q, detail %q; want the rename", list[0].Title, title(t, s, sess.ID))
	}
	if _, err := s.RenameSession(ctx, sess.ID, "", "user", ""); err != nil {
		t.Fatal(err)
	}
	if got := title(t, s, sess.ID); got != "Retried recap" {
		t.Fatalf("rename cleared: title %q, want the recap's", got)
	}
}

func editAndUndo(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := ended(t, s, base)
	if err := s.SetRecap(ctx, sess.ID, store.Recap{Status: store.RecapReady, Title: "Weekly sync", Text: "old recap"}); err != nil {
		t.Fatal(err)
	}
	rename, err := s.RenameSession(ctx, sess.ID, "Roadmap", "agent", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	recap, err := s.UpdateRecap(ctx, sess.ID, "new recap", "agent", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenameSession(ctx, newSession(t, s, base).ID, "Other", "agent", "run-2"); err != nil {
		t.Fatal(err)
	}
	edits, err := s.ListEdits(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 2 || edits[0].ID != rename.ID || edits[1].ID != recap.ID {
		t.Fatalf("run-1's edits = %+v, want the rename then the recap edit", edits)
	}
	if e := edits[0]; e.SessionID != sess.ID || e.Field != store.FieldTitle || e.OldValue != "" || e.NewValue != "Roadmap" ||
		e.Actor != "agent" || e.Origin != "run-1" || e.CreatedAt.IsZero() || !e.UndoneAt.IsZero() {
		t.Fatalf("rename edit = %+v", e)
	}
	if e := edits[1]; e.Field != store.FieldRecap || e.OldValue != "old recap" || e.NewValue != "new recap" {
		t.Fatalf("recap edit = %+v", e)
	}

	undone, err := s.UndoEdit(ctx, recap.ID)
	if err != nil || undone.UndoneAt.IsZero() || undone.SessionID != sess.ID {
		t.Fatalf("undo recap edit: %+v, %v", undone, err)
	}
	if d, err := s.GetSession(ctx, sess.ID); err != nil || d.Recap != "old recap" {
		t.Fatalf("after undo: recap %q, %v", d.Recap, err)
	}
	if _, err := s.UndoEdit(ctx, rename.ID); err != nil {
		t.Fatal(err)
	}
	if got := title(t, s, sess.ID); got != "Weekly sync" {
		t.Fatalf("after undoing the rename: title %q, want the recap's", got)
	}
	_, err = s.UndoEdit(ctx, rename.ID)
	wantErr(t, err, store.ErrConflict) // undone already
	_, err = s.UndoEdit(ctx, uuid.NewString())
	wantErr(t, err, store.ErrNotFound)
	if edits, err = s.ListEdits(ctx, "run-1"); err != nil || edits[0].UndoneAt.IsZero() || edits[1].UndoneAt.IsZero() {
		t.Fatalf("after undo, edits = %+v, %v; want both undone", edits, err)
	}
	if e, err := s.GetEdit(ctx, rename.ID); err != nil || e.ID != rename.ID || e.NewValue != "Roadmap" || e.UndoneAt.IsZero() {
		t.Fatalf("GetEdit(rename) = %+v, %v; want it, undone", e, err)
	}
	_, err = s.GetEdit(ctx, uuid.NewString())
	wantErr(t, err, store.ErrNotFound)
}

func undoRefusedAfterChange(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := ended(t, s, base)
	first, err := s.RenameSession(ctx, sess.ID, "First", "agent", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RenameSession(ctx, sess.ID, "Second", "user", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.UndoEdit(ctx, first.ID)
	wantErr(t, err, store.ErrConflict)
	if got := title(t, s, sess.ID); got != "Second" {
		t.Fatalf("a refused undo changed the title to %q", got)
	}
	// Undone in turn, each finds its own value back.
	if _, err := s.UndoEdit(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UndoEdit(ctx, first.ID); err != nil {
		t.Fatalf("undo after the later edit was undone: %v", err)
	}
	// A Retry Recap since an edit of the recap.
	edit, err := s.UpdateRecap(ctx, sess.ID, "agent's recap", "agent", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetRecap(ctx, sess.ID, store.Recap{Status: store.RecapReady, Title: "T", Text: "retried recap"}); err != nil {
		t.Fatal(err)
	}
	_, err = s.UndoEdit(ctx, edit.ID)
	wantErr(t, err, store.ErrConflict)
	if d, err := s.GetSession(ctx, sess.ID); err != nil || d.Recap != "retried recap" {
		t.Fatalf("a refused undo changed the recap to %q, %v", d.Recap, err)
	}
}

func undoTrash(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := ended(t, s, base)
	rename, err := s.RenameSession(ctx, sess.ID, "Renamed", "user", "")
	if err != nil {
		t.Fatal(err)
	}
	trash := mustTrash(t, s, sess.ID)
	// Its other edits wait for the session to come back.
	_, err = s.UndoEdit(ctx, rename.ID)
	wantErr(t, err, store.ErrNotFound)
	if _, err := s.UndoEdit(ctx, trash.ID); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSessions(ctx, store.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, list, sess.ID)
	if n := len(trashedIDs(t, s)); n != 0 {
		t.Fatalf("undoing the trash left %d sessions in Recently Deleted", n)
	}
}

func purgeErasesEdits(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := ended(t, s, base)
	rename, err := s.RenameSession(ctx, sess.ID, "Secret project", "agent", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	mustTrash(t, s, sess.ID)
	if err := s.PurgeSession(ctx, sess.ID); err != nil { // from Recently Deleted
		t.Fatal(err)
	}
	if edits, err := s.ListEdits(ctx, "run-1"); err != nil || len(edits) != 0 {
		t.Fatalf("after purge, edits = %+v, %v; want none", edits, err)
	}
	_, err = s.UndoEdit(ctx, rename.ID)
	wantErr(t, err, store.ErrNotFound)
	if n := len(trashedIDs(t, s)); n != 0 {
		t.Fatalf("a purged session stays in Recently Deleted (%d)", n)
	}
}

func expired(t *testing.T, s store.Store) {
	ctx := context.Background()
	old := ended(t, s, base)
	restored := ended(t, s, base.Add(time.Minute))
	purged := ended(t, s, base.Add(2*time.Minute))
	mustTrash(t, s, old.ID)
	mustTrash(t, s, restored.ID)
	mustTrash(t, s, purged.ID)
	if _, err := s.RestoreSession(ctx, restored.ID, "user", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeSession(ctx, purged.ID); err != nil {
		t.Fatal(err)
	}
	at := trashedIDs(t, s)[0].TrashedAt
	for now, want := range map[time.Time][]string{
		at.Add(store.TrashRetention):                    nil,
		at.Add(store.TrashRetention + time.Millisecond): {old.ID},
	} {
		got, err := s.ListExpired(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != fmt.Sprint(append([]string{}, want...)) {
			t.Errorf("expired %v after trashing: %v, want %v", now.Sub(at), got, want)
		}
	}
}

func heartbeat(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := newSession(t, s, base)
	if d, err := s.GetSession(ctx, sess.ID); err != nil || !d.HeartbeatAt.IsZero() {
		t.Fatalf("before a heartbeat: %v, %v", d.HeartbeatAt, err)
	}
	at := base.Add(10 * time.Second)
	if err := s.Heartbeat(ctx, sess.ID, at); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSessions(ctx, store.ListQuery{})
	if err != nil || !list[0].HeartbeatAt.Equal(at) {
		t.Fatalf("heartbeat at %v, listed %v, %v", at, list[0].HeartbeatAt, err)
	}
	if err := s.EndSession(ctx, sess.ID, at); err != nil {
		t.Fatal(err)
	}
	wantErr(t, s.Heartbeat(ctx, sess.ID, at.Add(10*time.Second)), store.ErrNotFound) // ended
	wantErr(t, s.Heartbeat(ctx, uuid.NewString(), at), store.ErrNotFound)
}

func targetWhileRecording(t *testing.T, s store.Store) {
	ctx := context.Background()
	sess := newSession(t, s, base)
	if err := s.SetTarget(ctx, sess.ID, "ja-JP"); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSessions(ctx, store.ListQuery{})
	if err != nil || list[0].LangA != "en-US" || list[0].LangB != "ja-JP" {
		t.Fatalf("listed %+v, %v; want en-US → ja-JP", list, err)
	}
	if err := s.EndSession(ctx, sess.ID, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	wantErr(t, s.SetTarget(ctx, sess.ID, "de-DE"), store.ErrNotFound) // ended
	if d, err := s.GetSession(ctx, sess.ID); err != nil || d.LangB != "ja-JP" {
		t.Fatalf("after the end: %s, %v; want ja-JP kept", d.LangB, err)
	}
	wantErr(t, s.SetTarget(ctx, uuid.NewString(), "ja-JP"), store.ErrNotFound)
}

func searchRange(t *testing.T, s store.Store) {
	ctx := context.Background()
	var ids []string
	for day := range 3 {
		sess := newSession(t, s, base.Add(time.Duration(day)*24*time.Hour))
		mustAppend(t, s, segment(sess.ID, 0, "pricing review"))
		ids = append(ids, sess.ID)
	}
	for name, c := range map[string]struct {
		from, to time.Time
		want     []string
	}{
		"open":          {want: []string{ids[2], ids[1], ids[0]}},
		"from":          {from: base.Add(24 * time.Hour), want: []string{ids[2], ids[1]}},
		"to, included":  {to: base.Add(24 * time.Hour), want: []string{ids[1], ids[0]}},
		"between":       {from: base.Add(time.Hour), to: base.Add(47 * time.Hour), want: []string{ids[1]}},
		"none in range": {from: base.Add(72 * time.Hour), want: nil},
	} {
		got, err := s.SearchRange(ctx, "pricing", c.from, c.to, 1)
		if err != nil {
			t.Fatal(err)
		}
		// The limit counts sessions in the range, newest first.
		want := c.want
		if len(want) > 1 {
			want = want[:1]
		}
		ids := make([]string, len(got))
		for i, g := range got {
			ids[i] = g.ID
		}
		if fmt.Sprint(ids) != fmt.Sprint(append([]string{}, want...)) {
			t.Errorf("%s: %v, want %v", name, ids, want)
		}
	}
}
