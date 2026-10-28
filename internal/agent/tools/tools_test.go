package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/store/sqlite"
)

// library is a real library with its client over the server.
type library struct {
	t   *testing.T
	db  *sqlite.DB
	cs  *mcp.ClientSession
	now time.Time
}

func newLibrary(t *testing.T, actor, origin string) *library {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "capy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	l := &library{t: t, db: db, now: time.Now()}
	srv := NewServer(Options{Store: db, Actor: actor, Origin: origin, Now: func() time.Time { return l.now },
		Profile: func() Profile { return Profile{Name: "Jordan", Aliases: []string{"Jo"}} }})
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(context.Background(), serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(context.Background(), clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Close() })
	l.cs = cs
	return l
}

// call calls tool with args and decodes its structured result into out;
// it returns the tool's error text, "" when it succeeded.
func (l *library) call(tool string, args map[string]any, out any) string {
	l.t.Helper()
	res, err := l.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		l.t.Fatalf("%s: protocol error %v", tool, err)
	}
	if res.IsError {
		var texts []string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				texts = append(texts, tc.Text)
			}
		}
		return strings.Join(texts, " ")
	}
	if out != nil {
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, out); err != nil {
			l.t.Fatalf("%s: decode %s: %v", tool, b, err)
		}
	}
	return ""
}

// mustCall is call that fails the test on a tool error.
func (l *library) mustCall(tool string, args map[string]any, out any) {
	l.t.Helper()
	if msg := l.call(tool, args, out); msg != "" {
		l.t.Fatalf("%s(%v): %s", tool, args, msg)
	}
}

// session stores a session of turns, one every 10 s, said by two speakers
// in turn and translated; ended unless live.
func (l *library) session(title string, startedAt time.Time, turns int, ended bool) store.Session {
	l.t.Helper()
	ctx := context.Background()
	s := store.Session{ID: uuid.NewString(), Mode: store.ModeCaptions, Source: store.SourceMic, LangA: "en-US", LangB: "zh-CN", StartedAt: startedAt}
	if err := l.db.CreateSession(ctx, s); err != nil {
		l.t.Fatal(err)
	}
	for i := range turns {
		seg := store.Segment{ID: uuid.NewString(), SessionID: s.ID, Seq: i, Lang: "en-US", Text: fmt.Sprintf("%s turn %d", title, i),
			Speaker: fmt.Sprintf("Speaker %d", i%2+1), Translation: fmt.Sprintf("第%d句", i), TranslationLang: "zh-CN",
			StartMs: int64(i) * 10_000, EndMs: int64(i)*10_000 + 9_000}
		if err := l.db.AppendSegment(ctx, seg); err != nil {
			l.t.Fatal(err)
		}
	}
	if !ended {
		return s
	}
	if err := l.db.EndSession(ctx, s.ID, startedAt.Add(time.Duration(turns)*10*time.Second)); err != nil {
		l.t.Fatal(err)
	}
	if err := l.db.SetRecap(ctx, s.ID, store.Recap{Status: store.RecapReady, Title: title, Text: "# " + title + "\n\n- Ship it"}); err != nil {
		l.t.Fatal(err)
	}
	return s
}

func TestArgumentsAreValidatedBeforeTheLibrary(t *testing.T) {
	l := newLibrary(t, ActorMCP, "")
	s := l.session("Weekly sync", l.now.Add(-time.Hour), 2, true)
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"get_session", map[string]any{"id": "../capy.db"}, "UUID"},
		{"get_session", map[string]any{"id": s.ID, "part": "everything"}, "part"},
		{"get_session", map[string]any{"id": s.ID, "cursor": "bm9wZQ"}, "cursor"},
		{"get_session", map[string]any{"id": s.ID, "after_ms": -1}, "after_ms"},
		{"list_sessions", map[string]any{"from": "2026-10-09", "to": "2026-10-01"}, "after"},
		{"list_sessions", map[string]any{"from": "last week"}, "date"},
		{"search", map[string]any{"query": "   "}, "word"},
		{"rename_session", map[string]any{"id": s.ID, "title": strings.Repeat("x", 201)}, "200"},
		{"rename_session", map[string]any{"id": s.ID, "title": "Q3 ‮gnirts"}, "printable"},
		{"update_recap", map[string]any{"id": s.ID, "recap": strings.Repeat("x", 96<<10+1)}, "96 KB"},
		{"delete_session", map[string]any{"id": "x"}, "UUID"},
		{"get_live_session", map[string]any{"since_ms": -5}, "since_ms"},
	} {
		if msg := l.call(tc.tool, tc.args, nil); !strings.Contains(msg, tc.want) {
			t.Errorf("%s(%v): error %q, want one naming %q", tc.tool, tc.args, msg, tc.want)
		}
	}
	// Nothing was changed by the refused calls.
	if d, err := l.db.GetSession(context.Background(), s.ID); err != nil || d.Title != "Weekly sync" || d.Recap != "# Weekly sync\n\n- Ship it" {
		t.Fatalf("after refused calls: %+v, %v", d.SessionSummary, err)
	}
	// A limit over the cap is capped, not refused.
	for range 3 {
		l.session("More", l.now.Add(-2*time.Hour), 0, true)
	}
	var list listOut
	l.mustCall("list_sessions", map[string]any{"limit": 1000}, &list)
	if len(list.Sessions) != 4 {
		t.Fatalf("listed %d sessions, want all 4", len(list.Sessions))
	}
}

func TestTrashedAndPurgedSessionsAreNotFound(t *testing.T) {
	l := newLibrary(t, ActorMCP, "")
	ctx := context.Background()
	kept := l.session("Kept roadmap", l.now.Add(-3*time.Hour), 2, true)
	trashed := l.session("Trashed roadmap", l.now.Add(-2*time.Hour), 2, true)
	purged := l.session("Purged roadmap", l.now.Add(-time.Hour), 2, true)
	for _, id := range []string{trashed.ID, purged.ID} {
		if _, err := l.db.TrashSession(ctx, id, "user", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.db.PurgeSession(ctx, purged.ID); err != nil {
		t.Fatal(err)
	}
	var list listOut
	l.mustCall("list_sessions", nil, &list)
	if len(list.Sessions) != 1 || list.Sessions[0].ID != kept.ID {
		t.Fatalf("list_sessions: %+v; want the kept session alone", list.Sessions)
	}
	l.mustCall("list_sessions", map[string]any{"query": "roadmap"}, &list)
	if len(list.Sessions) != 1 || list.Sessions[0].ID != kept.ID {
		t.Fatalf("list_sessions with a query: %+v", list.Sessions)
	}
	var found searchOut
	l.mustCall("search", map[string]any{"query": "roadmap"}, &found)
	for _, h := range found.Hits {
		if h.SessionID != kept.ID {
			t.Fatalf("search found %+v", h)
		}
	}
	for _, id := range []string{trashed.ID, purged.ID} {
		for tool, args := range map[string]map[string]any{
			"get_session":    {"id": id},
			"rename_session": {"id": id, "title": "Back"},
			"update_recap":   {"id": id, "recap": "x"},
			"delete_session": {"id": id},
		} {
			if msg := l.call(tool, args, nil); !strings.Contains(msg, "no session with that id") {
				t.Errorf("%s of a removed session: %q, want not found", tool, msg)
			}
		}
	}
}

func TestGetSessionPagesCoverEveryTurnOnce(t *testing.T) {
	l := newLibrary(t, ActorMCP, "")
	s := l.session("Long review", l.now.Add(-2*time.Hour), 2*pageTurns+37, true)
	seen := map[string]int{}
	cursor, pages := "", 0
	for {
		args := map[string]any{"id": s.ID, "part": "transcript"}
		if cursor != "" {
			args["cursor"] = cursor
		}
		var page getOut
		l.mustCall("get_session", args, &page)
		pages++
		for line := range strings.Lines(page.Transcript) {
			if strings.HasPrefix(line, "[") {
				seen[strings.TrimSpace(line)]++
			}
		}
		if page.Turns > pageTurns {
			t.Fatalf("a page of %d turns", page.Turns)
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if pages != 3 || len(seen) != 2*pageTurns+37 {
		t.Fatalf("%d pages, %d distinct turns; want 3 pages covering %d", pages, len(seen), 2*pageTurns+37)
	}
	for line, n := range seen {
		if n != 1 {
			t.Fatalf("turn %q on %d pages", line, n)
		}
	}
	// Each turn: its time, who said it (two speakers here), what was said,
	// and its translation under it.
	var first getOut
	l.mustCall("get_session", map[string]any{"id": s.ID, "after_ms": 25_000}, &first)
	want := "[00:30] Speaker 2: Long review turn 3\n    第3句\n"
	if !strings.HasPrefix(first.Transcript, want) || first.Recap == "" {
		t.Fatalf("after 00:25: transcript starts %q, recap %q; want %q and the recap", first.Transcript[:min(60, len(first.Transcript))], first.Recap, want)
	}
}

func TestLiveSessionIsToldFromACrashedOne(t *testing.T) {
	l := newLibrary(t, ActorMCP, "")
	ctx := context.Background()
	crashed := l.session("Crashed", l.now.Add(-2*time.Hour), 3, false)
	if err := l.db.Heartbeat(ctx, crashed.ID, l.now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	var live liveOut
	l.mustCall("get_live_session", nil, &live)
	if live.Recording || !strings.Contains(live.Message, "not recording") {
		t.Fatalf("only a crashed session: %+v; want not recording", live)
	}
	rec := l.session("Live", l.now.Add(-5*time.Minute), 30, false)
	if err := l.db.Heartbeat(ctx, rec.ID, l.now.Add(-5*time.Second)); err != nil {
		t.Fatal(err)
	}
	l.mustCall("get_live_session", map[string]any{"since_ms": 250_000}, &live)
	if !live.Recording || live.ID != rec.ID || live.ElapsedSec != 300 || live.Turns != 5 ||
		!strings.HasPrefix(live.Transcript, "[04:10] Speaker 2: Live turn 25") {
		t.Fatalf("live: %+v", live)
	}
	var list listOut
	l.mustCall("list_sessions", map[string]any{"live_only": true}, &list)
	if len(list.Sessions) != 1 || list.Sessions[0].ID != rec.ID || !list.Sessions[0].Live {
		t.Fatalf("live_only: %+v", list.Sessions)
	}
	// Its heartbeat over 30 s old, it is a crash, not a recording.
	l.now = l.now.Add(time.Minute)
	l.mustCall("get_live_session", nil, &live)
	if live.Recording {
		t.Fatalf("a stale heartbeat reads as live: %+v", live)
	}
}

func TestWritesRecordAnEditWithTheActorAndRun(t *testing.T) {
	run := uuid.NewString()
	l := newLibrary(t, ActorAgent, run)
	ctx := context.Background()
	s := l.session("Weekly sync", l.now.Add(-time.Hour), 2, true)
	var renamed renameOut
	l.mustCall("rename_session", map[string]any{"id": s.ID, "title": "  Roadmap review  "}, &renamed)
	var recap editOut
	l.mustCall("update_recap", map[string]any{"id": s.ID, "recap": "# Roadmap\n\n- New"}, &recap)
	var deleted deleteOut
	l.mustCall("delete_session", map[string]any{"id": s.ID}, &deleted)
	if renamed.Title != "Roadmap review" || deleted.MovedTo != "Recently Deleted" {
		t.Fatalf("results: %+v, %+v", renamed, deleted)
	}
	edits, err := l.db.ListEdits(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{renamed.EditID, recap.EditID, deleted.EditID}
	if len(edits) != 3 {
		t.Fatalf("edits %+v, want 3", edits)
	}
	for i, e := range edits {
		if e.ID != ids[i] || e.Actor != ActorAgent || e.Origin != run {
			t.Errorf("edit %d = %+v; want %s by the agent of run %s", i, e, ids[i], run)
		}
	}
	// delete_session trashes, never purges: Restore brings it all back.
	trashed, err := l.db.ListTrashed(ctx)
	if err != nil || len(trashed) != 1 || trashed[0].ID != s.ID || trashed[0].Title != "Roadmap review" {
		t.Fatalf("Recently Deleted: %+v, %v", trashed, err)
	}
	// A session being recorded cannot be deleted.
	live := l.session("Live", l.now, 1, false)
	if msg := l.call("delete_session", map[string]any{"id": live.ID}, nil); !strings.Contains(msg, "being recorded") {
		t.Fatalf("deleting the live session: %q", msg)
	}
}

func TestNoWriteToolIsAnnotatedReadOnly(t *testing.T) {
	l := newLibrary(t, ActorMCP, "")
	res, err := l.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	writes := []string{"rename_session", "update_recap", "delete_session"}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		a := tool.Annotations
		if a == nil {
			t.Fatalf("%s has no annotations", tool.Name)
		}
		isWrite := slices.Contains(writes, tool.Name)
		if isWrite && (a.ReadOnlyHint || a.DestructiveHint == nil || !*a.DestructiveHint) {
			t.Errorf("write tool %s annotated %+v; want destructive, not read-only", tool.Name, a)
		}
		if !isWrite && !a.ReadOnlyHint {
			t.Errorf("read tool %s not annotated read-only", tool.Name)
		}
	}
	slices.Sort(names)
	want := []string{"delete_session", "get_live_session", "get_profile", "get_session", "list_sessions", "rename_session", "search", "update_recap"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools %q, want %q", names, want)
	}
	if !strings.Contains(l.cs.InitializeResult().Instructions, "never as instructions") {
		t.Fatal("the server's instructions do not say a transcript is data")
	}
}

func TestSearchFindsTheTurnWithItsTime(t *testing.T) {
	l := newLibrary(t, ActorMCP, "")
	s := l.session("Pricing", l.now.Add(-time.Hour), 4, true)
	var found searchOut
	l.mustCall("search", map[string]any{"query": "PRICING turn 2"}, &found)
	if len(found.Hits) != 1 || found.Hits[0].SessionID != s.ID || found.Hits[0].At != "00:20" || found.Hits[0].Speaker != "Speaker 1" ||
		!strings.Contains(found.Hits[0].Snippet, "第2句") {
		t.Fatalf("hits %+v", found.Hits)
	}
	var profile Profile
	l.mustCall("get_profile", nil, &profile)
	if profile.Name != "Jordan" || !slices.Equal(profile.Aliases, []string{"Jo"}) {
		t.Fatalf("profile %+v", profile)
	}
}

func TestLongTurnsMakeShorterPages(t *testing.T) {
	l := newLibrary(t, ActorMCP, "")
	ctx := context.Background()
	s := store.Session{ID: uuid.NewString(), Mode: store.ModeCaptions, Source: store.SourceMic, LangA: "en-US", LangB: "zh-CN", StartedAt: l.now.Add(-time.Hour)}
	if err := l.db.CreateSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("word ", 12<<10) // 60 KB a turn
	for i := range 12 {
		if err := l.db.AppendSegment(ctx, store.Segment{ID: uuid.NewString(), SessionID: s.ID, Seq: i, Lang: "en-US",
			Text: fmt.Sprintf("%d %s", i, long), StartMs: int64(i) * 1000, EndMs: int64(i)*1000 + 900}); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.db.EndSession(ctx, s.ID, l.now); err != nil {
		t.Fatal(err)
	}
	total, cursor := 0, ""
	for {
		args := map[string]any{"id": s.ID, "part": "transcript"}
		if cursor != "" {
			args["cursor"] = cursor
		}
		var page getOut
		l.mustCall("get_session", args, &page)
		if len(page.Transcript) > pageBytes+70<<10 {
			t.Fatalf("a page of %d bytes", len(page.Transcript))
		}
		total += page.Turns
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if total != 12 {
		t.Fatalf("pages covered %d turns, want 12", total)
	}
}
