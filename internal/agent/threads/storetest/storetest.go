// Package storetest is the contract suite every threads.Store adapter must
// pass.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/agent/threads"
)

// Run runs the contract suite. open must return a fresh, empty store and
// register its own cleanup.
func Run(t *testing.T, open func(t *testing.T) threads.Store) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(t *testing.T, s threads.Store)
	}{
		{"MessagesOrderedBySeq", messagesOrdered},
		{"AppendBumpsUpdatedAt", appendBumps},
		{"ListThreadsBySessionAndCapability", listThreads},
		{"SetConversationAndStatus", setConversationAndStatus},
		{"SetAgentForgetsTheConversation", setAgent},
		{"DeleteThread", deleteThread},
		{"EraseSessionLeavesOnlyOtherSessions", eraseSession},
		{"DuplicateIDsConflict", duplicates},
		{"NotFound", notFound},
		{"Validation", validation},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, open(t)) })
	}
}

func sessionThread(t *testing.T, s threads.Store, sessionID, title string) threads.Thread {
	t.Helper()
	th, err := s.CreateThread(context.Background(), threads.Thread{ID: uuid.NewString(), Capability: "ask",
		SessionID: sessionID, Scope: threads.ScopeSession, Agent: "claude-code", Title: title})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return th
}

func crossThread(t *testing.T, s threads.Store, capability, scope, title string) threads.Thread {
	t.Helper()
	th, err := s.CreateThread(context.Background(), threads.Thread{ID: uuid.NewString(), Capability: capability,
		Scope: scope, Agent: "codex", Title: title})
	if err != nil {
		t.Fatalf("CreateThread: %v", err)
	}
	return th
}

func appendMsg(t *testing.T, s threads.Store, threadID string, role threads.Role, text string) threads.Message {
	t.Helper()
	m, err := s.AppendMessage(context.Background(), threads.Message{ID: uuid.NewString(), ThreadID: threadID, Role: role, Text: text})
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	return m
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func ids(list []threads.Thread) string {
	out := make([]string, len(list))
	for i, th := range list {
		out[i] = th.Title
	}
	return strings.Join(out, ",")
}

func messagesOrdered(t *testing.T, s threads.Store) {
	ctx := context.Background()
	session := uuid.NewString()
	th := sessionThread(t, s, session, "What was decided?")
	edit := uuid.NewString()
	q := appendMsg(t, s, th.ID, threads.RoleUser, "What was decided?")
	a := appendMsg(t, s, th.ID, threads.RoleAgent, "To ship on Friday [01:05].")
	e := appendMsg(t, s, th.ID, threads.RoleEdit, edit)
	if q.Seq != 0 || a.Seq != 1 || e.Seq != 2 || q.Status != threads.StatusDone || q.CreatedAt.IsZero() {
		t.Fatalf("appended %+v %+v %+v; want seqs 0, 1, 2, done", q, a, e)
	}
	// Another thread counts from 0 again.
	other := sessionThread(t, s, session, "Who spoke?")
	if m := appendMsg(t, s, other.ID, threads.RoleUser, "Who spoke?"); m.Seq != 0 {
		t.Fatalf("first message of another thread at seq %d", m.Seq)
	}
	d, err := s.GetThread(ctx, th.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != th.ID || d.SessionID != session || d.Scope != threads.ScopeSession || d.Agent != "claude-code" ||
		d.Capability != "ask" || d.Title != "What was decided?" || d.CreatedAt.IsZero() {
		t.Fatalf("thread = %+v", d.Thread)
	}
	if len(d.Messages) != 3 {
		t.Fatalf("got %d messages", len(d.Messages))
	}
	for i, want := range []threads.Message{q, a, e} {
		got := d.Messages[i]
		if got.ID != want.ID || got.Seq != i || got.Role != want.Role || got.Text != want.Text || got.Status != threads.StatusDone {
			t.Fatalf("message %d = %+v, want %+v", i, got, want)
		}
	}
}

func appendBumps(t *testing.T, s threads.Store) {
	ctx := context.Background()
	th := sessionThread(t, s, uuid.NewString(), "Q")
	time.Sleep(5 * time.Millisecond)
	appendMsg(t, s, th.ID, threads.RoleUser, "Q")
	d, err := s.GetThread(ctx, th.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !d.UpdatedAt.After(d.CreatedAt) {
		t.Fatalf("updated %v, created %v: an append must bump it", d.UpdatedAt, d.CreatedAt)
	}
}

func listThreads(t *testing.T, s threads.Store) {
	ctx := context.Background()
	a, b := uuid.NewString(), uuid.NewString()
	for _, title := range []string{"a1", "a2", "a3"} {
		sessionThread(t, s, a, title)
		time.Sleep(2 * time.Millisecond) // created in order
	}
	sessionThread(t, s, b, "b1")
	week := threads.RangeScope(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC))
	crossThread(t, s, "ask", threads.ScopeAll, "all1")
	time.Sleep(2 * time.Millisecond)
	crossThread(t, s, "ask", week, "range1")
	time.Sleep(2 * time.Millisecond)
	batch := uuid.NewString()
	crossThread(t, s, "ask", threads.BatchScope(batch), "batch1")
	crossThread(t, s, "later", threads.ScopeAll, "other capability")

	for _, c := range []struct {
		q    threads.ListQuery
		want string
	}{
		{threads.ListQuery{SessionID: a}, "a1,a2,a3"}, // oldest first: the History menu lists the newest last
		{threads.ListQuery{SessionID: a, Capability: "ask"}, "a1,a2,a3"},
		{threads.ListQuery{SessionID: a, Capability: "later"}, ""},
		{threads.ListQuery{SessionID: b}, "b1"},
		{threads.ListQuery{Capability: "ask"}, "all1,range1,batch1"}, // cross-session only
		{threads.ListQuery{Capability: "later"}, "other capability"},
		{threads.ListQuery{SessionID: uuid.NewString()}, ""},
	} {
		got, err := s.ListThreads(ctx, c.q)
		if err != nil {
			t.Fatal(err)
		}
		if ids(got) != c.want {
			t.Errorf("ListThreads(%+v) = %q, want %q", c.q, ids(got), c.want)
		}
	}
	got, err := s.ListThreads(ctx, threads.ListQuery{Capability: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if got[1].Scope != week || got[1].SessionID != "" {
		t.Fatalf("range thread = %+v", got[1])
	}
	if from, to, ok := threads.ParseRange(got[1].Scope); !ok || from.Day() != 1 || to.Day() != 8 {
		t.Fatalf("ParseRange(%q) = %v, %v, %v", got[1].Scope, from, to, ok)
	}
	if id, ok := threads.ParseBatch(got[2].Scope); !ok || id != batch || got[2].SessionID != "" {
		t.Fatalf("batch thread = %+v; ParseBatch = %q, %v", got[2], id, ok)
	}
}

func setConversationAndStatus(t *testing.T, s threads.Store) {
	ctx := context.Background()
	th := sessionThread(t, s, uuid.NewString(), "Q")
	if th.ConversationID != "" {
		t.Fatalf("new thread has conversation %q", th.ConversationID)
	}
	q := appendMsg(t, s, th.ID, threads.RoleUser, "Q")
	conv := uuid.NewString()
	if err := s.SetConversation(ctx, th.ID, conv); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMessageStatus(ctx, q.ID, threads.StatusUnanswered); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessage(ctx, threads.Message{ID: uuid.NewString(), ThreadID: th.ID, Role: threads.RoleAgent,
		Text: "So far:", Status: threads.StatusIncomplete}); err != nil {
		t.Fatal(err)
	}
	d, err := s.GetThread(ctx, th.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.ConversationID != conv || d.Messages[0].Status != threads.StatusUnanswered || d.Messages[1].Status != threads.StatusIncomplete {
		t.Fatalf("thread %q, statuses %q %q", d.ConversationID, d.Messages[0].Status, d.Messages[1].Status)
	}
	// Answered on Ask Again: done again.
	if err := s.SetMessageStatus(ctx, q.ID, threads.StatusDone); err != nil {
		t.Fatal(err)
	}
	if d, _ = s.GetThread(ctx, th.ID); d.Messages[0].Status != threads.StatusDone {
		t.Fatalf("status %q, want done", d.Messages[0].Status)
	}
}

func setAgent(t *testing.T, s threads.Store) {
	ctx := context.Background()
	th := sessionThread(t, s, uuid.NewString(), "Q")
	appendMsg(t, s, th.ID, threads.RoleUser, "Q")
	if err := s.SetConversation(ctx, th.ID, "claude-conv"); err != nil {
		t.Fatal(err)
	}
	other := sessionThread(t, s, uuid.NewString(), "Other")
	if err := s.SetConversation(ctx, other.ID, "other-conv"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAgent(ctx, th.ID, "codex"); err != nil {
		t.Fatal(err)
	}
	d, err := s.GetThread(ctx, th.ID)
	if err != nil || d.Agent != "codex" || d.ConversationID != "" || len(d.Messages) != 1 {
		t.Fatalf("after SetAgent: agent %q, conversation %q, %d messages, %v; want codex, none, the question kept",
			d.Agent, d.ConversationID, len(d.Messages), err)
	}
	if d, _ := s.GetThread(ctx, other.ID); d.Agent != "claude-code" || d.ConversationID != "other-conv" {
		t.Fatalf("another thread changed: %q, %q", d.Agent, d.ConversationID)
	}
	// The new agent's conversation is recorded as before.
	if err := s.SetConversation(ctx, th.ID, "codex-conv"); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetThread(ctx, th.ID); d.Agent != "codex" || d.ConversationID != "codex-conv" {
		t.Fatalf("after the new conversation: %q, %q", d.Agent, d.ConversationID)
	}
}

func deleteThread(t *testing.T, s threads.Store) {
	ctx := context.Background()
	gone := crossThread(t, s, "ask", threads.ScopeAll, "gone")
	keep := crossThread(t, s, "ask", threads.ScopeAll, "keep")
	m := appendMsg(t, s, gone.ID, threads.RoleUser, "gone")
	appendMsg(t, s, keep.ID, threads.RoleUser, "keep")
	if err := s.DeleteThread(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	_, err := s.GetThread(ctx, gone.ID)
	wantErr(t, err, threads.ErrNotFound)
	// Its messages went with it.
	wantErr(t, s.SetMessageStatus(ctx, m.ID, threads.StatusDone), threads.ErrNotFound)
	list, err := s.ListThreads(ctx, threads.ListQuery{Capability: "ask"})
	if err != nil || ids(list) != "keep" {
		t.Fatalf("after delete: %q, %v", ids(list), err)
	}
	wantErr(t, s.DeleteThread(ctx, gone.ID), threads.ErrNotFound)
}

func eraseSession(t *testing.T, s threads.Store) {
	ctx := context.Background()
	purged, kept := uuid.NewString(), uuid.NewString()
	var erased []threads.Message
	for i := range 2 {
		th := sessionThread(t, s, purged, fmt.Sprintf("purged %d", i))
		erased = append(erased, appendMsg(t, s, th.ID, threads.RoleUser, "secret"), appendMsg(t, s, th.ID, threads.RoleAgent, "answer"))
	}
	keep := sessionThread(t, s, kept, "kept")
	appendMsg(t, s, keep.ID, threads.RoleUser, "kept question")
	cross := crossThread(t, s, "ask", threads.ScopeAll, "cross")

	if err := s.EraseSession(ctx, purged); err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListThreads(ctx, threads.ListQuery{SessionID: purged}); err != nil || len(list) != 0 {
		t.Fatalf("after erase, the session keeps threads %q, %v", ids(list), err)
	}
	for _, m := range erased {
		if err := s.SetMessageStatus(ctx, m.ID, threads.StatusDone); !errors.Is(err, threads.ErrNotFound) {
			t.Fatalf("message %s of an erased thread: %v, want ErrNotFound", m.ID, err)
		}
	}
	if d, err := s.GetThread(ctx, keep.ID); err != nil || len(d.Messages) != 1 {
		t.Fatalf("another session's thread: %+v, %v", d, err)
	}
	if _, err := s.GetThread(ctx, cross.ID); err != nil {
		t.Fatalf("a cross-session thread: %v", err)
	}
	// A session without threads, as most are, erases without error.
	if err := s.EraseSession(ctx, uuid.NewString()); err != nil {
		t.Fatalf("erase a session without threads: %v", err)
	}
}

func duplicates(t *testing.T, s threads.Store) {
	ctx := context.Background()
	th := sessionThread(t, s, uuid.NewString(), "Q")
	_, err := s.CreateThread(ctx, th)
	wantErr(t, err, threads.ErrConflict)
	m := appendMsg(t, s, th.ID, threads.RoleUser, "Q")
	_, err = s.AppendMessage(ctx, threads.Message{ID: m.ID, ThreadID: th.ID, Role: threads.RoleAgent, Text: "A"})
	wantErr(t, err, threads.ErrConflict)
	if d, err := s.GetThread(ctx, th.ID); err != nil || len(d.Messages) != 1 {
		t.Fatalf("a refused append changed the thread: %+v, %v", d.Messages, err)
	}
}

func notFound(t *testing.T, s threads.Store) {
	ctx := context.Background()
	missing := uuid.NewString()
	_, err := s.AppendMessage(ctx, threads.Message{ID: uuid.NewString(), ThreadID: missing, Role: threads.RoleUser, Text: "Q"})
	wantErr(t, err, threads.ErrNotFound)
	_, err = s.GetThread(ctx, missing)
	wantErr(t, err, threads.ErrNotFound)
	wantErr(t, s.SetConversation(ctx, missing, "conv"), threads.ErrNotFound)
	wantErr(t, s.SetAgent(ctx, missing, "codex"), threads.ErrNotFound)
	wantErr(t, s.SetMessageStatus(ctx, uuid.NewString(), threads.StatusDone), threads.ErrNotFound)
	wantErr(t, s.DeleteThread(ctx, missing), threads.ErrNotFound)
}

func validation(t *testing.T, s threads.Store) {
	ctx := context.Background()
	session := uuid.NewString()
	good := func(mut func(*threads.Thread)) threads.Thread {
		th := threads.Thread{ID: uuid.NewString(), Capability: "ask", SessionID: session, Scope: threads.ScopeSession, Agent: "codex"}
		mut(&th)
		return th
	}
	for name, th := range map[string]threads.Thread{
		"id":                  good(func(th *threads.Thread) { th.ID = "x" }),
		"capability":          good(func(th *threads.Thread) { th.Capability = "" }),
		"agent":               good(func(th *threads.Thread) { th.Agent = "" }),
		"agent control":       good(func(th *threads.Thread) { th.Agent = "co\x00dex" }),
		"session id":          good(func(th *threads.Thread) { th.SessionID = "x" }),
		"session scope alone": good(func(th *threads.Thread) { th.SessionID = "" }),
		"all with a session":  good(func(th *threads.Thread) { th.Scope = threads.ScopeAll }),
		"unknown scope":       good(func(th *threads.Thread) { th.Scope = "week" }),
		"range backwards":     good(func(th *threads.Thread) { th.SessionID, th.Scope = "", "range:20-10" }),
		"range malformed":     good(func(th *threads.Thread) { th.SessionID, th.Scope = "", "range:x-1" }),
		"batch not a uuid":    good(func(th *threads.Thread) { th.SessionID, th.Scope = "", "batch:x" }),
		"batch with session":  good(func(th *threads.Thread) { th.Scope = threads.BatchScope(uuid.NewString()) }),
		"title too long":      good(func(th *threads.Thread) { th.Title = strings.Repeat("a", threads.MaxTitleLen+1) }),
		"title utf-8":         good(func(th *threads.Thread) { th.Title = "\xff" }),
	} {
		if _, err := s.CreateThread(ctx, th); !errors.Is(err, threads.ErrInvalid) {
			t.Errorf("CreateThread with bad %s: error = %v, want ErrInvalid", name, err)
		}
	}
	th := sessionThread(t, s, session, "Q")
	msg := func(mut func(*threads.Message)) threads.Message {
		m := threads.Message{ID: uuid.NewString(), ThreadID: th.ID, Role: threads.RoleUser, Text: "Q"}
		mut(&m)
		return m
	}
	for name, m := range map[string]threads.Message{
		"id":            msg(func(m *threads.Message) { m.ID = "" }),
		"thread id":     msg(func(m *threads.Message) { m.ThreadID = "x" }),
		"role":          msg(func(m *threads.Message) { m.Role = "system" }),
		"status":        msg(func(m *threads.Message) { m.Status = "pending" }),
		"edit not id":   msg(func(m *threads.Message) { m.Role, m.Text = threads.RoleEdit, "renamed it" }),
		"text too long": msg(func(m *threads.Message) { m.Text = strings.Repeat("a", threads.MaxTextLen+1) }),
		"text utf-8":    msg(func(m *threads.Message) { m.Text = "\xff\xfe" }),
	} {
		if _, err := s.AppendMessage(ctx, m); !errors.Is(err, threads.ErrInvalid) {
			t.Errorf("AppendMessage with bad %s: error = %v, want ErrInvalid", name, err)
		}
	}
	wantErr(t, s.SetConversation(ctx, th.ID, ""), threads.ErrInvalid)
	wantErr(t, s.SetConversation(ctx, th.ID, strings.Repeat("a", 65)), threads.ErrInvalid)
	wantErr(t, s.SetConversation(ctx, "x", "conv"), threads.ErrInvalid)
	wantErr(t, s.SetAgent(ctx, th.ID, ""), threads.ErrInvalid)
	wantErr(t, s.SetAgent(ctx, "x", "codex"), threads.ErrInvalid)
	wantErr(t, s.SetMessageStatus(ctx, uuid.NewString(), "pending"), threads.ErrInvalid)
	wantErr(t, s.EraseSession(ctx, "x"), threads.ErrInvalid)
	wantErr(t, s.DeleteThread(ctx, "x"), threads.ErrInvalid)
	_, err := s.GetThread(ctx, "x")
	wantErr(t, err, threads.ErrInvalid)
	_, err = s.ListThreads(ctx, threads.ListQuery{})
	wantErr(t, err, threads.ErrInvalid)
	_, err = s.ListThreads(ctx, threads.ListQuery{SessionID: "x"})
	wantErr(t, err, threads.ErrInvalid)
	// Validation errors never repeat user text.
	_, err = s.CreateThread(ctx, good(func(th *threads.Thread) { th.Title, th.Scope = "secret question", "bogus" }))
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("error %v repeats the title", err)
	}
	// Nothing rejected was stored.
	if d, err := s.GetThread(ctx, th.ID); err != nil || len(d.Messages) != 0 {
		t.Fatalf("rejected appends changed the thread: %+v, %v", d.Messages, err)
	}
	if list, err := s.ListThreads(ctx, threads.ListQuery{SessionID: session}); err != nil || len(list) != 1 {
		t.Fatalf("rejected creates stored %d threads, %v", len(list), err)
	}
}
