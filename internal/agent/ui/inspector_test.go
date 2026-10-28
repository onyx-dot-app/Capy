package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/onyx-dot-app/Capy/internal/agent/ask"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/fake"
	"github.com/onyx-dot-app/Capy/internal/agent/prefs"
	"github.com/onyx-dot-app/Capy/internal/agent/threads"
	threadsqlite "github.com/onyx-dot-app/Capy/internal/agent/threads/sqlite"
	"github.com/onyx-dot-app/Capy/internal/agent/tools"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/store/sqlite"
	"github.com/onyx-dot-app/Capy/internal/views/addon"
	"github.com/onyx-dot-app/Capy/internal/views/kit"
)

// named is the fake under an agent's name, as the boards show one.
type named struct {
	*fake.Agent
	info gateway.Info
}

func (n named) Info() gateway.Info { return n.info }

// ContinueCommand is a command of the test's, as the CLIs' adapters give.
func (n named) ContinueCommand(id string, tools gateway.Tools) ([]string, error) {
	return []string{"/bin/agent", "--resume", id}, nil
}

func claudeLike(script fake.Script, sends bool) named {
	info := gateway.Info{ID: "claude-code", Name: "Claude Code", Command: "claude", Streams: true}
	if sends {
		info.Sends = &gateway.Recipient{Provider: "Anthropic", Account: "your Claude account"}
	}
	return named{Agent: &fake.Agent{Script: script}, info: info}
}

// askHost is the app as the inspector's tests stand for it: what it was
// asked to show, and Update run on the test's goroutine.
type askHost struct {
	host
	mu    sync.Mutex
	shown []string
	// live is the session recording; capsule whether the bar is one,
	// until ExpandCaptionBar; keys the shortcuts registered.
	live    *addon.Session
	capsule bool
	keys    map[string]func()
	changes int // library refreshes requested by the add-on
}

func (h *askHost) ExpandCaptionBar() bool {
	was := h.capsule
	h.capsule = false
	return was
}

func (h *askHost) RegisterShortcut(acc string, fn func()) error {
	if h.keys == nil {
		h.keys = map[string]func(){}
	}
	h.keys[acc] = fn
	return nil
}

func (h *askHost) ShowTranscript(id string, atMs int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.shown = append(h.shown, id+"@"+time.Duration(atMs*int64(time.Millisecond)).String())
}

func (h *askHost) ShowSession(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.shown = append(h.shown, id)
}

func (h *askHost) LiveSession() *addon.Session { return h.live }
func (h *askHost) Window() *mygo.Window        { return nil }
func (h *askHost) LibraryChanged()             { h.changes++ }

type inspectorFixture struct {
	a       *AddOn
	h       *askHost
	agent   named
	gw      *gateway.Gateway
	lib     *sqlite.DB
	prefs   *prefs.File
	tt      *ui.Tester
	session *addon.Session // the session in view
	opened  chan string    // apps opened, off the main thread
	scripts chan string    // Continue in… scripts opened
}

const rehearsal = "Launch rehearsal"

// newInspector is the inspector open on a session of the library, over
// Ask with agent, which reaches Capy's real tools.
func newInspector(t *testing.T, agent named, gwo gateway.Options) *inspectorFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	lib, err := sqlite.Open(ctx, filepath.Join(dir, "capy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lib.Close() })
	th, err := threadsqlite.Open(ctx, filepath.Join(dir, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { th.Close() })
	f := &inspectorFixture{h: &askHost{host: host{q: make(chan func(), 64)}}, agent: agent, lib: lib, opened: make(chan string, 4), scripts: make(chan string, 4),
		prefs: prefs.Load(filepath.Join(dir, "agent.json"))}
	if err := f.prefs.Update(func(p *prefs.Prefs) { p.InspectorOpen = true }); err != nil {
		t.Fatal(err)
	}
	f.session = &addon.Session{ID: recorded(t, lib), Title: rehearsal}
	gwo.Server = func(run string) *mcp.Server {
		return tools.NewServer(tools.Options{Store: lib, Actor: tools.ActorAgent, Origin: run})
	}
	f.gw = gateway.New(gwo, agent)
	t.Cleanup(f.gw.Close)
	ak := ask.New(ask.Options{Gateway: f.gw, Threads: th, Library: lib, ContinueDir: dir,
		Agent: func() string { return agent.info.ID },
		Open:  func(path string) error { f.scripts <- path; return nil }})
	f.a = New(Options{Gate: &gate{on: true}, Gateway: f.gw, Ask: ak, Prefs: f.prefs,
		OpenApp: func(name string) error { f.opened <- name; return nil }})
	f.a.Start(f.h)
	f.tt = ui.NewTester(func(c *ui.Context) {
		kit.Theme(c) // as Capy's window gives its views
		ui.Row(c).Fill().AlignItems(ui.Stretch).Children(func() {
			ui.Column(c).Grow(1).AlignItems(ui.End).Children(func() {
				ui.Row(c).Height(52).AlignItems(ui.Center).Children(func() {
					f.a.Toolbar(c, f.session)
				})
			})
			f.a.Shortcuts(c, f.session)
			if pane := f.a.Inspector(c, f.session); pane != nil {
				ui.Column(c).Width(pane.Width).AlignItems(ui.Stretch).Children(pane.View)
			}
		})
	}, 720, 700)
	f.settle(t)
	return f
}

// recorded is an ended session of two turns, recapped.
func recorded(t *testing.T, lib *sqlite.DB) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	start := time.Now().Add(-time.Hour)
	if err := lib.CreateSession(ctx, store.Session{ID: id, Mode: store.ModeCaptions, Source: store.SourceMic, LangA: "en-US", LangB: "zh-CN", StartedAt: start}); err != nil {
		t.Fatal(err)
	}
	for i, text := range []string{"Okay, let’s run through the launch one more time.", "Pricing still shows last quarter’s numbers."} {
		if err := lib.AppendSegment(ctx, store.Segment{ID: uuid.NewString(), SessionID: id, Seq: i, Lang: "en-US", Text: text,
			StartMs: int64(i) * 31_000, EndMs: int64(i)*31_000 + 5_000}); err != nil {
			t.Fatal(err)
		}
	}
	if err := lib.EndSession(ctx, id, start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := lib.SetRecap(ctx, id, store.Recap{Status: store.RecapReady, Title: rehearsal, Text: "# " + rehearsal}); err != nil {
		t.Fatal(err)
	}
	return id
}

// settle runs what the inspector posted, and draws, until nothing more
// comes.
func (f *inspectorFixture) settle(t *testing.T) {
	t.Helper()
	f.tt.Frame()
	for ran := true; ran; { // a frame may start reads that post more
		ran = false
		for quiet := false; !quiet; {
			select {
			case fn := <-f.h.q:
				fn()
				ran = true
			case <-time.After(150 * time.Millisecond):
				quiet = true
			}
		}
		f.tt.Frame()
	}
}

func (f *inspectorFixture) click(t *testing.T, label string) {
	t.Helper()
	if err := f.tt.Click(label); err != nil {
		t.Fatalf("%v; texts %q", err, f.tt.Texts())
	}
	f.settle(t)
}

// ask types question in the composer and presses Return.
func (f *inspectorFixture) ask(t *testing.T, question string) {
	t.Helper()
	f.click(t, "Question")
	f.tt.Type(question)
	f.tt.Key(0, ui.KeyEnter)
	f.settle(t)
}

func (f *inspectorFixture) has(t *testing.T, texts ...string) {
	t.Helper()
	for _, s := range texts {
		if !f.tt.HasText(s) {
			t.Errorf("the inspector lacks %q; texts %q", s, f.tt.Texts())
		}
	}
}

func (f *inspectorFixture) lacks(t *testing.T, texts ...string) {
	t.Helper()
	for _, s := range texts {
		if f.tt.HasText(s) {
			t.Errorf("the inspector shows %q", s)
		}
	}
}

var readIt = []fake.Call{{Tool: "get_session", Args: map[string]any{"id": "{session}", "part": "transcript"}}}

func TestAskShowsOnlyWithAnAgentFoundAndAgentOn(t *testing.T) {
	missing := claudeLike(fake.Script{}, false)
	f := newInspector(t, missing, gateway.Options{})
	f.has(t, "Ask", "Ask about this session")

	// No agent found: no Ask button, and no inspector, open or not.
	f.a.found = []gateway.Available{{Info: missing.info}}
	f.tt.Frame()
	f.lacks(t, "Ask", "Ask about this session")

	// Agent turned off.
	f.a.found = f.gw.Agents(context.Background())
	f.a.o.Gate.(*gate).on = false
	f.tt.Frame()
	f.lacks(t, "Ask", "Ask about this session")
}

func TestTheAskButtonAndCmdJOpenAndCloseTheInspector(t *testing.T) {
	f := newInspector(t, claudeLike(fake.Script{}, false), gateway.Options{})
	f.click(t, "Ask")
	if f.prefs.Get().InspectorOpen || f.tt.HasText("Ask about this session") {
		t.Fatal("the Ask button did not close the inspector")
	}
	f.tt.Key(ui.Cmd, ui.KeyJ)
	f.settle(t)
	if !f.prefs.Get().InspectorOpen || !f.tt.HasText("Ask about this session") {
		t.Fatal("⌘J did not open the inspector")
	}
	if !f.tt.Focused("Question") {
		t.Fatal("the question does not have the focus as the inspector opens")
	}
}

func TestTheEmptyStateFollowsTheScope(t *testing.T) {
	f := newInspector(t, claudeLike(fake.Script{}, false), gateway.Options{})
	f.has(t, "This session", "Ask about this session", "Claude Code reads this session’s transcript to answer.",
		"Summarize", "Action items", "Decisions", "Was I mentioned?", "Draft a follow-up email")
	snapshot(t, f.tt, "q1.1-empty-session")
	f.click(t, "Scope")
	if err := f.tt.ChooseMenuItem("All Sessions"); err != nil {
		t.Fatal(err)
	}
	f.settle(t)
	f.has(t, "All sessions", "Ask across your sessions", "Claude Code searches every session in Capy to answer.",
		"What did I commit to this week?", "Find where we discussed…", "Catch me up on yesterday")
	snapshot(t, f.tt, "q1.2-empty-all")
	// One that ends in "…" fills the field for the user to finish.
	f.click(t, "Find where we discussed…")
	if len(f.agent.Queries()) != 0 || f.a.panel.draft != "Find where we discussed " {
		t.Fatalf("draft %q, %d sent", f.a.panel.draft, len(f.agent.Queries()))
	}
	// With no session in view, This Session cannot be chosen.
	f.session = nil
	f.tt.Frame()
	f.click(t, "Scope")
	if err := f.tt.ChooseMenuItem("This Session"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("This Session with no session: %v", err)
	}
}

func TestAQuestionIsAnsweredInTheThread(t *testing.T) {
	f := newInspector(t, claudeLike(fake.Script{Calls: readIt, Answer: "Three, all due before Friday’s launch:\n1. **Maya**: update the pricing slide [00:31]\n2. Leo: record a backup demo"}, false), gateway.Options{})
	f.ask(t, "What are the action items?")
	f.has(t, "What are the action items?", "Read Launch rehearsal", "Three, all due before Friday’s launch:",
		"Maya", ": update the pricing slide ", "00:31", "Continue in Claude Code")
	if qs := f.agent.Queries(); len(qs) != 1 || !strings.Contains(qs[0].Prompt, f.session.ID) {
		t.Fatalf("queries %+v", qs)
	}
	if f.a.panel.draft != "" {
		t.Fatalf("the field kept %q after sending", f.a.panel.draft)
	}
	snapshot(t, f.tt, "q1.4-answer")

	// A citation opens the session's Transcript at the turn.
	f.click(t, "00:31") // the citation's link
	if len(f.h.shown) != 1 || f.h.shown[0] != f.session.ID+"@31s" {
		t.Fatalf("shown %v, want the Transcript at 00:31", f.h.shown)
	}
	// A follow-up resumes the thread.
	f.agent.Script = fake.Script{Answer: "By Thursday."}
	f.ask(t, "By when?")
	f.has(t, "By Thursday.")
	if qs := f.agent.Queries(); len(qs) != 2 || qs[1].ConversationID == "" {
		t.Fatalf("the follow-up did not resume the conversation: %+v", qs)
	}
}

func TestShiftReturnStartsALineReturnSends(t *testing.T) {
	f := newInspector(t, claudeLike(fake.Script{Answer: "ok"}, false), gateway.Options{})
	f.click(t, "Question")
	f.tt.Type("first")
	f.tt.Key(ui.Shift, ui.KeyEnter)
	f.tt.Type("second")
	f.settle(t)
	if f.a.panel.draft != "first\nsecond" || len(f.agent.Queries()) != 0 {
		t.Fatalf("draft %q, %d sent", f.a.panel.draft, len(f.agent.Queries()))
	}
	f.tt.Key(0, ui.KeyEnter)
	f.settle(t)
	if qs := f.agent.Queries(); len(qs) != 1 || !strings.HasSuffix(qs[0].Prompt, "first\nsecond") {
		t.Fatalf("queries %+v", qs)
	}
}

func TestAnAgentsEditShowsAChipThatUndoes(t *testing.T) {
	rename := []fake.Call{{Tool: "rename_session", Args: map[string]any{"id": "{session}", "title": "Friday launch rehearsal"}}}
	f := newInspector(t, claudeLike(fake.Script{Calls: rename, Answer: "I renamed it."}, false), gateway.Options{})
	f.ask(t, "Rename this by topic")
	f.has(t, "Renamed session", "Renamed to “Friday launch rehearsal”", "Undo")
	snapshot(t, f.tt, "q1.5-edit")
	f.click(t, "Undo Renamed to “Friday launch rehearsal”")
	f.has(t, "Undone")
	if f.h.changes != 1 {
		t.Fatalf("Undo requested %d library refreshes, want 1", f.h.changes)
	}
	if s, _ := f.lib.GetSession(context.Background(), f.session.ID); s.Title != rehearsal {
		t.Fatalf("title %q after Undo", s.Title)
	}
}

func TestProblemsSayWhatToDo(t *testing.T) {
	t.Run("signed out", func(t *testing.T) {
		f := newInspector(t, claudeLike(fake.Script{Fail: "signed-out"}, false), gateway.Options{})
		f.ask(t, "Was I mentioned?")
		f.has(t, "Was I mentioned?", "Sign in to Claude Code: run claude once in Terminal.")
		f.click(t, "Open Terminal")
		select {
		case app := <-f.opened:
			if app != "Terminal" {
				t.Fatalf("opened %s, want Terminal", app)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Open Terminal opened nothing")
		}
		snapshot(t, f.tt, "q1.7-signed-out")
	})
	t.Run("stalled, then Retry", func(t *testing.T) {
		f := newInspector(t, claudeLike(fake.Script{Answer: "Three, all due before Friday", Fail: "exit", FailAfter: 14}, false),
			gateway.Options{})
		f.ask(t, "What are the action items?")
		f.has(t, "Three, all due", "The answer stopped early.", "Claude Code stopped unexpectedly.", "Retry")
		snapshot(t, f.tt, "q1.7-stopped-early")
		f.agent.Script = fake.Script{Answer: "Three, all due before Friday."}
		f.click(t, "Retry")
		f.has(t, "Three, all due before Friday.")
		if n := len(f.agent.Queries()); n != 2 {
			t.Fatalf("%d runs, want the question asked again", n)
		}
	})
	t.Run("usage limit", func(t *testing.T) {
		f := newInspector(t, claudeLike(fake.Script{Fail: "usage-limit"}, false), gateway.Options{})
		f.ask(t, "Draft a follow-up email")
		f.has(t, "Claude Code says you’ve reached your usage limit.", "Your question is saved; send it again later.")
	})
}

func TestTheFirstAskWithAnAgentAsksConsentOnce(t *testing.T) {
	f := newInspector(t, claudeLike(fake.Script{Answer: "Three."}, true), gateway.Options{})
	f.ask(t, "What are the action items?")
	f.has(t, "Ask with Claude Code?", "Allow")
	if !strings.Contains(strings.Join(f.tt.Texts(), " "), "to Anthropic through your Claude account") {
		t.Fatalf("the consent names no recipient: %q", f.tt.Texts())
	}
	snapshot(t, f.tt, "q1.8-consent")
	f.click(t, "Cancel")
	if len(f.agent.Queries()) != 0 || f.a.panel.draft != "What are the action items?" {
		t.Fatalf("Cancel: draft %q, %d sent; want nothing sent and the question kept", f.a.panel.draft, len(f.agent.Queries()))
	}
	f.click(t, "Question")
	f.tt.Key(0, ui.KeyEnter)
	f.settle(t)
	f.click(t, "Allow")
	f.has(t, "Three.")
	f.agent.Script = fake.Script{Answer: "Friday."}
	f.ask(t, "When?")
	f.lacks(t, "Ask with Claude Code?")
	f.has(t, "Friday.")
}

func TestHistoryListsTheThreadsAndNewThreadStartsOne(t *testing.T) {
	f := newInspector(t, claudeLike(fake.Script{Answer: "Done."}, false), gateway.Options{})
	f.ask(t, "What are the action items?")
	f.click(t, "New Thread")
	f.has(t, "Ask about this session")
	f.ask(t, "Was I mentioned?")
	f.click(t, "History")
	menu := strings.Join(f.tt.Menu(), "|")
	if !strings.HasPrefix(menu, rehearsal+"|What are the action items? · ") || !strings.Contains(menu, "|Was I mentioned? · ") ||
		!strings.Contains(menu, "New Thread") || strings.Contains(menu, "Delete This Thread") {
		t.Fatalf("History %q", menu)
	}
	snapshot(t, f.tt, "q1.6-history")
	var first string
	for _, item := range f.tt.Menu() {
		if strings.HasPrefix(item, "What are the action items?") {
			first = item
		}
	}
	if err := f.tt.ChooseMenuItem(first); err != nil {
		t.Fatal(err)
	}
	f.settle(t)
	f.has(t, "What are the action items?")
	f.lacks(t, "Was I mentioned?")
}

func TestAQuestionLeftByQuittingCanBeAskedAgain(t *testing.T) {
	f := newInspector(t, claudeLike(fake.Script{Calls: readIt, Stall: true}, false), gateway.Options{})
	f.ask(t, "Catch me up")
	f.gw.Close() // Capy quits during the run
	f.settle(t)
	// Reopened later, the thread shows the question unanswered.
	f.a.data = newData()
	f.a.panel, f.a.live = newPanel(f.a, f.a.data, false), newPanel(f.a, f.a.data, true)
	f.settle(t)
	f.has(t, "Catch me up", "Not answered · ", "Ask Again")
}

func TestACustomRangeNamesItsDays(t *testing.T) {
	f := newInspector(t, claudeLike(fake.Script{}, false), gateway.Options{})
	f.click(t, "Scope")
	if err := f.tt.ChooseMenuItem("Custom Range…"); err != nil {
		t.Fatal(err)
	}
	f.settle(t)
	f.has(t, "Custom Range", "From", "To")
	f.click(t, "Choose")
	to := day(time.Now())
	f.has(t, to.AddDate(0, 0, -7).Format("Jan 2")+" – "+to.Format("Jan 2"), "Ask across your sessions")
	if f.a.panel.key() == "all" || f.a.panel.askScope().From.IsZero() {
		t.Fatalf("a custom range asks %+v", f.a.panel.askScope())
	}
}

// TestAnAnswerStreamsInAsItWillRead shows an answer being written as it
// will read, its half-written bold and citation without their marks,
// and links the citation once it closes.
func TestAnAnswerStreamsInAsItWillRead(t *testing.T) {
	f := newInspector(t, claudeLike(fake.Script{Calls: readIt, Stall: true}, false), gateway.Options{})
	f.ask(t, "What are the action items?")
	r := f.a.panel.runs[f.a.panel.open[f.a.panel.key()]]
	if r == nil {
		t.Fatal("no run going")
	}
	r.st.Answer = "Maya owns **the pricing [00:3"
	f.tt.Frame()
	f.tt.Advance(time.Second)              // the words come in
	f.has(t, "Maya owns the pricing 00:3") // a link being written
	f.lacks(t, "**")
	r.st.Answer += "1]** slide."
	f.tt.Frame()
	f.tt.Advance(time.Second)
	f.has(t, "Maya owns the pricing 00:31 slide.")
	f.click(t, "00:31") // the citation's link
	if len(f.h.shown) != 1 || f.h.shown[0] != f.session.ID+"@31s" {
		t.Fatalf("shown %v, want the Transcript at 00:31", f.h.shown)
	}
}

// TestTheThreadFollowsTheAnswerUntilTheUserScrollsUp keeps the end of an
// answer in view as it comes; scrolled up, the user stays where they read,
// and Jump to Latest takes them back to the end, which it follows again.
func TestTheThreadFollowsTheAnswerUntilTheUserScrollsUp(t *testing.T) {
	f := newInspector(t, claudeLike(fake.Script{Calls: readIt, Stall: true}, false), gateway.Options{})
	f.ask(t, "What are the action items?")
	r := f.a.panel.runs[f.a.panel.open[f.a.panel.key()]]
	if r == nil {
		t.Fatal("no run going")
	}
	field, _ := f.tt.Find("Question")
	inView := func(s string) bool { // a text scrolled away has an empty box at the edge
		at, ok := f.tt.Find(s)
		return ok && at.H > 0 && at.Y >= 0 && at.Y+at.H <= field.Y
	}
	write := func(from, to int) {
		for i := from; i < to; i++ {
			r.st.Answer += fmt.Sprintf("Item %d.\n\n", i)
		}
		f.tt.Frame()
		f.tt.Advance(2 * time.Second) // the words come in, and the list glides
	}
	write(0, 40)
	if !inView("Item 39.") || inView("Item 0.") || f.tt.HasText("Jump to Latest") {
		t.Fatalf("the end of the answer is not followed: Item 39 in view %v, Item 0 %v", inView("Item 39."), inView("Item 0."))
	}
	f.tt.Scroll(field.X+field.W/2, field.Y/2, 0, -400)
	f.tt.Frame()
	f.has(t, "Jump to Latest")
	snapshot(t, f.tt, "thread-jump-to-latest")
	write(40, 41)
	if inView("Item 40.") {
		t.Fatal("the thread scrolled down to the answer while the user read above it")
	}
	f.click(t, "Jump to Latest")
	f.tt.Advance(time.Second)
	write(41, 42)
	if !inView("Item 41.") || f.tt.HasText("Jump to Latest") {
		t.Fatalf("after Jump to Latest: Item 41 in view %v, the button shows %v", inView("Item 41."), f.tt.HasText("Jump to Latest"))
	}
}

func TestStopEndsTheAnswer(t *testing.T) {
	f := newInspector(t, claudeLike(fake.Script{Calls: readIt, Stall: true}, false), gateway.Options{})
	f.ask(t, "What are the action items?")
	f.has(t, "Stop")
	snapshot(t, f.tt, "q1.3-running")
	f.click(t, "Stop")
	f.has(t, "The answer stopped early.", "Retry")
}

// TestAFollowUpTypedDuringAnAnswerWaitsInItsThread sends each follow-up
// typed while an answer comes, in order, once it ends, in the thread it
// was typed in, whatever scope is in view by then.
func TestAFollowUpTypedDuringAnAnswerWaitsInItsThread(t *testing.T) {
	f := newInspector(t, claudeLike(fake.Script{Calls: readIt, Stall: true}, false), gateway.Options{})
	f.ask(t, "What are the action items?")
	f.ask(t, "By when?")
	f.ask(t, "Who else?")
	f.has(t, "By when?", "Who else?")
	if n := len(f.agent.Queries()); n != 1 {
		t.Fatalf("%d runs while the first answers, want 1", n)
	}
	thread := f.a.panel.open[f.a.panel.key()]
	f.click(t, "Scope")
	if err := f.tt.ChooseMenuItem("All Sessions"); err != nil {
		t.Fatal(err)
	}
	f.settle(t)
	f.agent.Script = fake.Script{Answer: "Soon."}
	f.a.panel.stop(thread)
	f.settle(t)
	f.settle(t)
	qs := f.agent.Queries()
	if len(qs) != 3 || !strings.HasSuffix(qs[1].Prompt, "By when?") || !strings.HasSuffix(qs[2].Prompt, "Who else?") {
		t.Fatalf("queries %+v, want both follow-ups in order", qs)
	}
	if qs[1].ConversationID == "" || qs[2].ConversationID != qs[1].ConversationID {
		t.Fatalf("the follow-ups did not resume the thread: %q, %q", qs[1].ConversationID, qs[2].ConversationID)
	}
	if got := f.a.panel.open["all"]; got != "" {
		t.Fatalf("All Sessions opened thread %q, want none", got)
	}
	f.has(t, "Ask across your sessions")
}

func TestNewThreadAndCmdNFocusTheQuestion(t *testing.T) {
	f := newInspector(t, claudeLike(fake.Script{Answer: "Done."}, false), gateway.Options{})
	f.ask(t, "What are the action items?")
	for _, start := range []func(){
		func() { f.click(t, "New Thread") },
		func() { f.tt.Key(ui.Cmd, ui.KeyN); f.settle(t) },
	} {
		f.click(t, "Done.") // the focus elsewhere
		start()
		if !f.tt.HasText("Ask about this session") || !f.tt.Focused("Question") {
			t.Fatalf("a new thread without the focus in its question; texts %q", f.tt.Texts())
		}
		f.ask(t, "Again?")
	}
}

func TestDeleteThisThreadErasesACrossSessionThread(t *testing.T) {
	f := newInspector(t, claudeLike(fake.Script{Answer: "Nothing yet."}, false), gateway.Options{})
	f.click(t, "Scope")
	if err := f.tt.ChooseMenuItem("All Sessions"); err != nil {
		t.Fatal(err)
	}
	f.settle(t)
	f.ask(t, "What did I commit to?")
	id := f.a.panel.open["all"]
	f.click(t, "History")
	if err := f.tt.ChooseMenuItem("Delete This Thread"); err != nil {
		t.Fatal(err)
	}
	f.settle(t)
	f.has(t, "Ask across your sessions")
	if _, err := f.a.o.Ask.Thread(context.Background(), id); err == nil {
		t.Fatal("the thread is still in the store")
	}
}

func TestContinueInOpensTheThreadInTerminal(t *testing.T) {
	f := newInspector(t, claudeLike(fake.Script{Answer: "Done."}, false), gateway.Options{})
	f.ask(t, "What are the action items?")
	f.click(t, "Continue in Claude Code")
	select {
	case path := <-f.scripts:
		if filepath.Base(path) != "continue.command" {
			t.Fatalf("opened %s", path)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Continue in opened nothing")
	}
}

func TestTheInspectorsWidthIsSavedOnceTheDragRests(t *testing.T) {
	f := newInspector(t, claudeLike(fake.Script{}, false), gateway.Options{})
	for _, w := range []float32{300, 350, 390} {
		f.a.resized(&f.a.inspectorSize, w)
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.prefs.Get().InspectorWidth != 390 {
		if time.Now().After(deadline) {
			t.Fatalf("width saved %v, want 390", f.prefs.Get().InspectorWidth)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestACrossSessionAnswerLinksItsSessions opens a session a title names,
// and a citation after it at its turn.
func TestACrossSessionAnswerLinksItsSessions(t *testing.T) {
	f := newInspector(t, claudeLike(fake.Script{Answer: "In Launch rehearsal you agreed [00:31]."}, false), gateway.Options{})
	f.click(t, "Scope")
	if err := f.tt.ChooseMenuItem("All Sessions"); err != nil {
		t.Fatal(err)
	}
	f.settle(t)
	f.ask(t, "What did I agree to?")
	f.click(t, rehearsal)
	f.click(t, "00:31") // the citation's link
	if want := []string{f.session.ID, f.session.ID + "@31s"}; strings.Join(f.h.shown, " ") != strings.Join(want, " ") {
		t.Fatalf("shown %v, want %v", f.h.shown, want)
	}
}

func TestAskAboutThisBatchAsksAboutItsSessions(t *testing.T) {
	f := newInspector(t, claudeLike(fake.Script{Answer: "Both want SSO."}, false), gateway.Options{})
	if err := f.prefs.Update(func(p *prefs.Prefs) { p.InspectorOpen = false }); err != nil {
		t.Fatal(err)
	}
	f.settle(t)
	b := addon.Batch{ID: uuid.NewString(), Name: "Customer interviews", SessionIDs: []string{f.session.ID, uuid.NewString()}}
	menu := ui.NewTester(func(c *ui.Context) {
		ui.Box(c).Size(200, 40).Label("Batch").ContextMenu(func(m *ui.Menu) { f.a.BatchMenu(m, b) })
	}, 300, 100)
	if err := menu.RightClick("Batch"); err != nil {
		t.Fatal(err)
	}
	if err := menu.ChooseMenuItem("Ask About This Batch"); err != nil {
		t.Fatalf("%v; menu %q", err, menu.Menu())
	}
	menu.Frame()
	f.settle(t)
	// The inspector opens on a new thread about the batch.
	if !f.a.inspectorOpen() {
		t.Fatal("the inspector did not open")
	}
	f.has(t, "Customer interviews")
	f.ask(t, "What do they all want?")
	qs := f.agent.Queries()
	if len(qs) != 1 || !strings.Contains(qs[0].Prompt, `"Customer interviews": the sessions with ids `+f.session.ID+", "+b.SessionIDs[1]) {
		t.Fatalf("queries %+v", qs)
	}
	list, err := f.a.o.Ask.Threads(context.Background(), "")
	if err != nil || len(list) != 1 || list[0].Scope != threads.BatchScope(b.ID) {
		t.Fatalf("threads %+v, %v", list, err)
	}
	// A batch without a session saved yet has nothing to ask about.
	b.SessionIDs = nil
	if err := menu.RightClick("Batch"); err != nil {
		t.Fatal(err)
	}
	if err := menu.ChooseMenuItem("Ask About This Batch"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("no sessions: %v", err)
	}
}

func TestAskSlotSendsAQuestionAboutTheSessionInANewThread(t *testing.T) {
	f := newInspector(t, claudeLike(fake.Script{Answer: "Update the pricing slide before the dry run."}, false), gateway.Options{})
	if err := f.prefs.Update(func(p *prefs.Prefs) { p.InspectorOpen = false }); err != nil {
		t.Fatal(err)
	}
	f.settle(t)
	if !f.a.CanAsk() {
		t.Fatal("CanAsk is false with an agent found and Agent on")
	}
	const question = "What does [00:31] mean for me, and what should I do?"
	f.a.Ask(*f.session, question, false)
	f.settle(t)
	if !f.a.inspectorOpen() {
		t.Fatal("the inspector did not open")
	}
	f.has(t, question, "Update the pricing slide before the dry run.")
	qs := f.agent.Queries()
	if len(qs) != 1 || !strings.Contains(qs[0].Prompt, question) || !strings.Contains(qs[0].Prompt, f.session.ID) {
		t.Fatalf("queries %+v: want the question, about the session", qs)
	}
	// With Agent off, there is nothing to ask.
	f.a.o.Gate = &gate{}
	if f.a.CanAsk() {
		t.Fatal("CanAsk is true with Agent off")
	}
	f.a.Ask(*f.session, question, false)
	f.settle(t)
	if n := len(f.agent.Queries()); n != 1 {
		t.Fatalf("%d queries: Agent off asked", n)
	}
}
