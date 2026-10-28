package session

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/onyx-dot-app/Capy/internal/profile"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

var allKinds = store.HighlightKinds

func TestNameHeardIsAWholeWordOrACJKName(t *testing.T) {
	names := []string{"Alex", "Capy team", "小明"}
	for text, want := range map[string]bool{
		"Alex, can you take it?":          true,
		"thanks, ALEX.":                   true,
		"over to the Capy team now":       true,
		"Alexander will present":          false,
		"the Capy teams agreed":           false,
		"ping alexa about it":             false,
		"小明你来更新一下":                        true,
		"大家好":                             false,
		"an Alex-style layout":            true, // a word, which Decisions then judges
		"":                                false,
		"Alex":                            true,
		"I asked Alex2 for it":            false,
		"Ask Álex":                        false,
		"no names here, alright, alexis?": false,
	} {
		if got := nameHeard(text, names); got != want {
			t.Errorf("nameHeard(%q) = %v, want %v", text, got, want)
		}
	}
	if nameHeard("Alex, are you there?", []string{"", "  "}) {
		t.Error("blank names heard")
	}
}

// meeting is n turns of a meeting, one every step, each "turn <seq>".
func meeting(n int, step time.Duration) []Line {
	lines := make([]Line, n)
	for i := range lines {
		lines[i] = Line{Seq: i, StartMs: int64(i) * step.Milliseconds(), Lang: "en-US", Text: fmt.Sprintf("turn %d", i), Speaker: "Speaker 1"}
	}
	return lines
}

var alex = profile.Profile{Name: "Alex Rivera", Aliases: []string{"Alex", "Capy team"}}

func TestBuildRequestsSplitsLeadInFromNewTurns(t *testing.T) {
	all := meeting(10, 30*time.Second)
	all[6] = Line{Seq: 6, StartMs: 180_000, Lang: "gap", Gap: true}
	all[8].Lang, all[8].Text, all[8].Translation, all[8].TranslationLang, all[8].Speaker = "ja-JP", "こんにちは", "Hello", "en-US", "Speaker 2"
	cfg := HighlightConfig{On: true, Kinds: allKinds, Profile: alex}
	reqs := buildRequests(all, all[5:], cfg, "en-US", "session-1", 0)
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	r := reqs[0]
	want := "You are helping one person follow a live meeting they are recording.\n" +
		"The listener: Alex Rivera. Also called: Alex, Capy team.\n" +
		"Each turn is [id time speaker] text. Speaker labels are not names.\n\n" +
		"LEAD-IN (context only — do not choose these):\n" +
		"[t1 00:30 S1] turn 1\n[t2 01:00 S1] turn 2\n[t3 01:30 S1] turn 3\n[t4 02:00 S1] turn 4\n\n" +
		"NEW:\n" +
		"[t5 02:30 S1] turn 5\n[t7 03:30 S1] turn 7\n[t8 04:00 S2] Hello\n[t9 04:30 S1] turn 9"
	if r.req.Input != want {
		t.Fatalf("input:\n%s\nwant:\n%s", r.req.Input, want)
	}
	if r.req.Model != zoomai.DecisionsModel || r.req.ReferenceID != "session-1" || len(r.req.Questions) != 7 {
		t.Fatalf("request %+v", r.req)
	}
	for i, q := range r.req.Questions {
		if q.Name != string(allKinds[i]) || !slices.Equal(q.Choices, []string{"none", "t5", "t7", "t8", "t9"}) ||
			!strings.HasSuffix(q.Instructions, "If you are not sure, choose none.") || strings.Contains(q.Instructions, "Alex") {
			t.Errorf("question %d: %+v", i, q)
		}
	}
	// Without translation, each turn is as said.
	if r := buildRequests(all, all[8:9], cfg, "", "s", 0); !strings.HasSuffix(r[0].req.Input, "[t8 04:00 S2] こんにちは") {
		t.Errorf("untranslated input ends %q", r[0].req.Input[len(r[0].req.Input)-40:])
	}
}

func TestBuildRequestsAsksOnlyWhatItCan(t *testing.T) {
	all := meeting(3, time.Second)
	noName := HighlightConfig{On: true, Kinds: allKinds, Profile: profile.Profile{Aliases: []string{"Alex"}}}
	r := buildRequests(all, all, noName, "", "s", 0)
	if len(r) != 1 || !strings.Contains(r[0].req.Input, "The listener's name is not known.") || strings.Contains(r[0].req.Input, "LEAD-IN") {
		t.Fatalf("no name: %+v", r)
	}
	var names []string
	for _, q := range r[0].req.Questions {
		names = append(names, q.Name)
	}
	if !slices.Equal(names, []string{"heads_up", "decision", "debate", "action", "announcement"}) {
		t.Fatalf("without a name asks %q: the user's kinds need one", names)
	}
	decisions := HighlightConfig{On: true, Kinds: []store.HighlightKind{store.KindDecision}, Profile: alex}
	if r := buildRequests(all, all, decisions, "", "s", 0); len(r[0].req.Questions) != 1 || r[0].req.Questions[0].Name != "decision" {
		t.Fatalf("one kind on: %+v", r[0].req.Questions)
	}
	if r := buildRequests(all, all, HighlightConfig{On: true, Profile: alex}, "", "s", 0); r != nil {
		t.Fatalf("no kind on: %+v", r)
	}
	gaps := []Line{{Seq: 0, Gap: true, Lang: "gap"}}
	if r := buildRequests(gaps, gaps, decisions, "", "s", 0); r != nil {
		t.Fatalf("only a gap: %+v", r)
	}
}

func TestBuildRequestsSplitsALongBacklog(t *testing.T) {
	cfg := HighlightConfig{On: true, Kinds: allKinds, Profile: alex}
	all := meeting(450, time.Second)
	reqs := buildRequests(all, all, cfg, "", "s", 0)
	var sizes []int
	next := 0
	for _, r := range reqs {
		sizes = append(sizes, len(r.news))
		if r.news[0].Seq != next || len(r.req.Questions[0].Choices) != len(r.news)+1 {
			t.Fatalf("request from t%d with %d choices, want from t%d", r.news[0].Seq, len(r.req.Questions[0].Choices), next)
		}
		next += len(r.news)
		// Each has the lead-in of the turns before it.
		if prev := r.news[0].Seq - 1; prev >= 0 && !strings.Contains(r.req.Input, fmt.Sprintf("[t%d ", prev)) {
			t.Errorf("request from t%d lacks its lead-in", r.news[0].Seq)
		}
	}
	if !slices.Equal(sizes, []int{200, 200, 50}) {
		t.Fatalf("split into %v, want 200, 200, 50", sizes)
	}
	// Long turns split it by size first.
	long := meeting(40, time.Second)
	for i := range long {
		long[i].Text = strings.Repeat("word ", 300)
	}
	for _, r := range buildRequests(long, long, cfg, "", "s", 0) {
		if len(r.req.Input) > zoomai.MaxDecideInput {
			t.Fatalf("a request of %d bytes", len(r.req.Input))
		}
	}
}

// answer is a choice answer of kind, the turns' probabilities by seq,
// none taking the rest.
func answer(kind store.HighlightKind, p map[int]float64) zoomai.Answer {
	a := zoomai.Answer{Name: string(kind), Probabilities: map[string]float64{}}
	rest := 1.0
	for seq, v := range p {
		a.Probabilities[turnID(seq)] = v
		rest -= v
	}
	a.Probabilities[noTurn] = rest
	return a
}

func picks(answers ...zoomai.Answer) zoomai.DecideResponse {
	return zoomai.DecideResponse{Answers: answers}
}

func TestPickMarksGuardsAgainstNoise(t *testing.T) {
	all := meeting(20, 10*time.Second)
	r := offered(all[10:])
	cfg := HighlightConfig{On: true, Kinds: allKinds, Profile: alex, Threshold: ThresholdFewer}
	pick := func(existing []store.Highlight, answers ...zoomai.Answer) []mark {
		return pickMarks(picks(answers...), r, cfg, existing, all, maxMarksOfOther)
	}
	cases := []struct {
		name     string
		existing []store.Highlight
		answers  []zoomai.Answer
		want     []mark
	}{
		{"sure of one turn", nil, []zoomai.Answer{answer(store.KindDecision, map[int]float64{12: 0.9})},
			[]mark{{12, store.KindDecision, 0.9}}},
		{"sure something happened, not where", nil, []zoomai.Answer{answer(store.KindDecision, map[int]float64{12: 0.48, 13: 0.48})}, nil},
		{"under θ", nil, []zoomai.Answer{answer(store.KindDecision, map[int]float64{12: 0.84})}, nil},
		{"at θ", nil, []zoomai.Answer{answer(store.KindDecision, map[int]float64{12: 0.85})}, []mark{{12, store.KindDecision, 0.85}}},
		{"the user's kinds need θ+0.05", nil, []zoomai.Answer{
			answer(store.KindAskedYou, map[int]float64{11: 0.89}), answer(store.KindMentionedYou, map[int]float64{14: 0.9}),
		}, []mark{{14, store.KindMentionedYou, 0.9}}},
		{"a turn not sent", nil, []zoomai.Answer{answer(store.KindDecision, map[int]float64{3: 0.99})}, nil},
		{"a refusal", nil, []zoomai.Answer{{Name: string(store.KindDecision), Refused: true}}, nil},
		{"a question not asked", nil, []zoomai.Answer{{Name: "question", Probabilities: map[string]float64{"t12": 1}}}, nil},
		{"the first answer of a kind", nil, []zoomai.Answer{
			answer(store.KindDecision, map[int]float64{12: 0.9}), answer(store.KindDecision, map[int]float64{15: 0.99}),
		}, []mark{{12, store.KindDecision, 0.9}}},
		{"one kind per turn, the user's first", nil, []zoomai.Answer{
			answer(store.KindDecision, map[int]float64{12: 0.99}), answer(store.KindAskedYou, map[int]float64{12: 0.9}),
			answer(store.KindHeadsUp, map[int]float64{12: 0.95}),
		}, []mark{{12, store.KindAskedYou, 0.9}}},
		{"two of the other kinds, the surest", nil, []zoomai.Answer{
			answer(store.KindDecision, map[int]float64{11: 0.86}), answer(store.KindAction, map[int]float64{13: 0.95}),
			answer(store.KindAnnouncement, map[int]float64{15: 0.9}), answer(store.KindAskedYou, map[int]float64{17: 0.91}),
			answer(store.KindMentionedYou, map[int]float64{18: 0.92}),
		}, []mark{{13, store.KindAction, 0.95}, {15, store.KindAnnouncement, 0.9}, {17, store.KindAskedYou, 0.91}, {18, store.KindMentionedYou, 0.92}}},
		{"the same moment again", []store.Highlight{{Seq: 9, Kind: store.KindDecision}}, []zoomai.Answer{
			answer(store.KindDecision, map[int]float64{11: 0.95}),
		}, nil},
		{"another kind at that moment", []store.Highlight{{Seq: 9, Kind: store.KindDecision}}, []zoomai.Answer{
			answer(store.KindAction, map[int]float64{11: 0.95}),
		}, []mark{{11, store.KindAction, 0.95}}},
		{"the same kind three turns on", []store.Highlight{{Seq: 9, Kind: store.KindDecision}}, []zoomai.Answer{
			answer(store.KindDecision, map[int]float64{12: 0.95}),
		}, []mark{{12, store.KindDecision, 0.95}}},
		{"a turn marked before, dismissed", []store.Highlight{{Seq: 12, Kind: store.KindHeadsUp, Dismissed: true}}, []zoomai.Answer{
			answer(store.KindDecision, map[int]float64{12: 0.99}),
		}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pick(c.existing, c.answers...); !slices.Equal(got, c.want) {
				t.Fatalf("marks %v, want %v", got, c.want)
			}
		})
	}
	// The same kind within ±2 turns but more than 90 s apart is another moment.
	far := meeting(20, time.Minute)
	got := pickMarks(picks(answer(store.KindDecision, map[int]float64{11: 0.95})), offered(far[10:]), cfg,
		[]store.Highlight{{Seq: 9, Kind: store.KindDecision}}, far, maxMarksOfOther)
	if len(got) != 1 {
		t.Fatalf("2 min later: %v", got)
	}
	// More marks θ lower; the room left in a check bounds the other kinds.
	more := cfg
	more.Threshold = ThresholdMore
	if got := pickMarks(picks(answer(store.KindDecision, map[int]float64{12: 0.72})), r, more, nil, all, 0); len(got) != 0 {
		t.Fatalf("no room left: %v", got)
	}
	if got := pickMarks(picks(answer(store.KindDecision, map[int]float64{12: 0.72})), r, more, nil, all, 1); len(got) != 1 {
		t.Fatalf("More at 0.72: %v", got)
	}
}

func TestNextCheck(t *testing.T) {
	last := time.Date(2026, 10, 21, 11, 0, 0, 0, time.UTC)
	every := 5 * time.Minute
	at := func(d time.Duration) time.Time { return last.Add(d) }
	cases := []struct {
		name   string
		nameAt time.Time
		news   bool
		want   time.Time
		ok     bool
	}{
		{"silence costs nothing", time.Time{}, false, time.Time{}, false},
		{"the interval after the last", time.Time{}, true, at(every), true},
		{"a name, 15 s after it", at(time.Minute), true, at(time.Minute + 15*time.Second), true},
		{"a name, 30 s after the last at the soonest", at(5 * time.Second), true, at(30 * time.Second), true},
		{"a name late in the interval", at(every - 5*time.Second), true, at(every), true},
	}
	for _, c := range cases {
		got, ok := nextCheck(last, c.nameAt, every, c.news)
		if ok != c.ok || !got.Equal(c.want) {
			t.Errorf("%s: %v %v, want %v %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestEvalRequestsCheckEachIntervalOfTheTranscript(t *testing.T) {
	lines := meeting(4, time.Second)
	for i, ms := range []int64{0, 299_000, 300_000, 720_000} {
		lines[i].StartMs = ms
	}
	cfg := HighlightConfig{On: true, Every: 5 * time.Minute, Kinds: allKinds, Profile: alex}
	var got [][]int
	for _, r := range EvalRequests(lines, cfg, "") {
		var seqs []int
		for _, l := range r.news {
			seqs = append(seqs, l.Seq)
		}
		got = append(got, seqs)
	}
	if fmt.Sprint(got) != "[[0 1] [2] [3]]" {
		t.Fatalf("checks of %v, want one per 5 minutes said in, silence skipped", got)
	}
}

func TestListenForHearsAFirstNameToo(t *testing.T) {
	p := profile.Profile{Name: "Alex Rivera", Aliases: []string{"Capy team", "李 明", "J"}}
	if got := listenFor(p); !slices.Equal(got, []string{"Alex Rivera", "Capy team", "李 明", "J", "Alex", "Capy"}) {
		t.Fatalf("listenFor = %q", got)
	}
	if !nameHeard("can you take that, Alex?", listenFor(profile.Profile{Name: "Alex Rivera"})) {
		t.Fatal("the account's full name alone does not hear its first name")
	}
}

func TestBuildRequestsOffersATurnJudgedBeforeToTheUsersKindsOnly(t *testing.T) {
	all := meeting(6, 10*time.Second)
	cfg := HighlightConfig{On: true, Kinds: allKinds, Profile: alex}
	reqs := buildRequests(all, all[2:], cfg, "", "s", 4)
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	for _, q := range reqs[0].req.Questions {
		want := []string{"none", "t4", "t5"}
		if store.HighlightKind(q.Name).ForYou() {
			want = []string{"none", "t2", "t3", "t4", "t5"}
		}
		if !slices.Equal(q.Choices, want) {
			t.Errorf("%s offers %q, want %q", q.Name, q.Choices, want)
		}
	}
	// A decision Decisions finds in a turn judged before is not marked.
	got := pickMarks(picks(answer(store.KindDecision, map[int]float64{2: 0.99})), reqs[0], cfg, nil, all, maxMarksOfOther)
	if len(got) != 0 {
		t.Fatalf("marked %v on a turn not offered to decisions", got)
	}
	// With nothing new to the other kinds, only the user's are asked.
	if r := buildRequests(all, all[2:4], cfg, "", "s", 4); len(r) != 1 || len(r[0].req.Questions) != 2 {
		t.Fatalf("only judged turns: %+v", r)
	}
}

func TestATurnLongerThanARequestIsCut(t *testing.T) {
	all := meeting(1, time.Second)
	all[0].Text = strings.Repeat("长", 20_000) // 60 KB
	r := buildRequests(all, all, HighlightConfig{On: true, Kinds: allKinds, Profile: alex}, "", "s", 0)
	if len(r) != 1 || len(r[0].req.Input) > zoomai.MaxDecideInput || !strings.HasSuffix(r[0].req.Input, "…") {
		t.Fatalf("a long turn: %d requests, input %d bytes", len(r), len(r[0].req.Input))
	}
}

// offered is a request that offers every kind the turns news.
func offered(news []Line) request {
	choices := []string{noTurn}
	for _, l := range news {
		choices = append(choices, turnID(l.Seq))
	}
	r := request{news: news}
	for _, k := range allKinds {
		r.req.Questions = append(r.req.Questions, zoomai.ChoiceQuestion{Name: string(k), Choices: choices})
	}
	return r
}
