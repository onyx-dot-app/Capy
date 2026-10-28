package session

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/onyx-dot-app/Capy/internal/store"
)

func TestChunksSplitAtLinesWithinLimitWithoutLoss(t *testing.T) {
	text := strings.Repeat("short line\n", 30) + strings.Repeat("长", 50) + "\n" + "tail without newline"
	for _, limit := range []int{25, 64, 100, 1000} {
		chunks := Chunks(text, limit)
		if got := strings.Join(chunks, ""); got != text {
			t.Fatalf("limit %d: chunks do not reassemble the text", limit)
		}
		for _, c := range chunks {
			if len(c) > limit {
				t.Fatalf("limit %d: chunk of %d bytes", limit, len(c))
			}
			if !utf8.ValidString(c) {
				t.Fatalf("limit %d: chunk splits a rune: %q", limit, c)
			}
		}
	}
	// Whole lines that fit are never split.
	for _, c := range Chunks(strings.Repeat("0123456789\n", 10), 25) {
		if !strings.HasSuffix(c, "\n") {
			t.Fatalf("chunk %q ends mid-line", c)
		}
	}
}

func TestTitleIsFirstLineWithoutMarkdown(t *testing.T) {
	tests := map[string]string{
		"# Weekly sync\n\n- item":     "Weekly sync",
		"\n\n**Budget review**\nmore": "Budget review",
		"- first bullet":              "first bullet",
		"":                            "",
		strings.Repeat("词", 100):      strings.Repeat("词", maxTitleRunes-1) + "…",
		"## 中文 (简体)\n\n# 项目进度\n- 下周发布": "中文 (简体)",
	}
	for in, want := range tests {
		if got := Title(in); got != want {
			t.Errorf("Title(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTranscriptUsesTheRecapLanguage(t *testing.T) {
	d := store.SessionDetail{
		SessionSummary: store.SessionSummary{Mode: store.ModeCaptions, LangA: "zh-CN", LangB: "en-US"},
		Segments: []store.Segment{
			{Lang: "zh-CN", Text: "你好", Translation: "Hello", TranslationLang: "en-US"},
			{Lang: store.GapLang},
			{Lang: "zh-CN", Text: "再见", Translation: "", TranslationLang: ""}, // translation failed
		},
	}
	if got, want := Transcript(d, "en-US"), "Hello\n再见\n"; got != want {
		t.Errorf("en-US transcript %q, want %q (untranslated turns stay in the original)", got, want)
	}
	if got, want := Transcript(d, "zh-CN"), "你好\n再见\n"; got != want {
		t.Errorf("zh-CN transcript %q, want %q", got, want)
	}
}

func TestRecapLanguage(t *testing.T) {
	spoken := func(codes ...string) []store.Segment {
		var segs []store.Segment
		for _, c := range codes {
			segs = append(segs, store.Segment{Lang: c, Text: "x"})
		}
		return segs
	}
	tests := []struct {
		name, a, b string
		translated bool
		segs       []store.Segment
		want       string
	}{
		{"the translation's", "en-US", "ja-JP", true, spoken("en-US"), "ja-JP"},
		{"never translated: spoken most", "en-US", "ja-JP", false, spoken("en-US", "es-ES", "es-ES"), "es-ES"},
		// Summarizer does not write Korean or Traditional Chinese.
		{"spoken, when Summarizer cannot write the translation's", "en-US", "ko-KR", true, spoken("en-US"), "en-US"},
		{"spoken most, gaps and unknown languages aside", "auto", "zh-TW", true,
			append(spoken("en-US", "ja-JP", "ja-JP", "und", "und", "und"), store.Segment{Lang: store.GapLang}), "ja-JP"},
		{"nothing said: the language it started in", "ja-JP", "zh-TW", false, nil, "ja-JP"},
		{"nothing said in auto: English", "auto", "zh-CN", false, nil, "en-US"},
	}
	for _, tt := range tests {
		d := store.SessionDetail{SessionSummary: store.SessionSummary{LangA: tt.a, LangB: tt.b, Translated: tt.translated}, Segments: tt.segs}
		if got := RecapLanguage(d); got != tt.want {
			t.Errorf("%s: RecapLanguage = %s, want %s", tt.name, got, tt.want)
		}
	}
}

func TestUntranslatedIsATranslationAwaited(t *testing.T) {
	tests := []struct {
		name string
		seg  store.Segment
		want bool
	}{
		{"failed", store.Segment{Lang: "en-US", Text: "Hi", TranslationLang: "ja-JP"}, true},
		{"translated", store.Segment{Lang: "en-US", Text: "Hi", TranslationLang: "ja-JP", Translation: "やあ"}, false},
		{"said with translation off", store.Segment{Lang: "en-US", Text: "Hi"}, false},
		{"gap", store.Segment{Lang: store.GapLang}, false},
	}
	for _, tt := range tests {
		if got := Untranslated(tt.seg); got != tt.want {
			t.Errorf("%s: Untranslated = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestTranslationTarget(t *testing.T) {
	tests := []struct{ from, to, want string }{
		{"en-US", "zh-CN", "zh-CN"},
		{"zh-CN", "zh-CN", ""}, // spoken in the translation's language
		{"und", "zh-CN", ""},   // Translator needs to know the language
	}
	for _, tt := range tests {
		if got := TranslationTarget(tt.from, tt.to); got != tt.want {
			t.Errorf("TranslationTarget(%s, %s) = %q, want %q", tt.from, tt.to, got, tt.want)
		}
	}
}

func TestTranscriptNamesSpeakersOnlyWhenSeveralSpoke(t *testing.T) {
	seg := func(speaker, text string) store.Segment {
		return store.Segment{Lang: "en-US", Text: text, Speaker: speaker}
	}
	one := store.SessionDetail{Segments: []store.Segment{seg("Speaker 1", "Hi"), seg("Speaker 1", "Bye")}}
	if got, want := Transcript(one, "en-US"), "Hi\nBye\n"; got != want {
		t.Errorf("one speaker: %q, want %q", got, want)
	}
	two := store.SessionDetail{Segments: []store.Segment{seg("Speaker 1", "Hi"), seg("", "Hm"), seg("Speaker 2", "Bye")}}
	if got, want := Transcript(two, "en-US"), "Speaker 1: Hi\nHm\nSpeaker 2: Bye\n"; got != want {
		t.Errorf("two speakers: %q, want %q", got, want)
	}
}

func TestSpokenLanguagesInOrderFirstHeard(t *testing.T) {
	d := store.SessionDetail{
		SessionSummary: store.SessionSummary{LangA: "auto"},
		Segments: []store.Segment{
			{Lang: "zh-CN", Text: "x"}, {Lang: store.GapLang}, {Lang: "und", Text: "?"}, {Lang: "en-US", Text: "y"}, {Lang: "zh-CN", Text: "z"},
		},
	}
	if got := SpokenLanguages(d); !slices.Equal(got, []string{"zh-CN", "en-US"}) {
		t.Errorf("SpokenLanguages = %v", got)
	}
	d.Segments = nil
	if got := SpokenLanguages(d); !slices.Equal(got, []string{"auto"}) {
		t.Errorf("SpokenLanguages before any turn = %v", got)
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		cfg   Config
		valid bool
	}{
		{Config{LangA: "en-US", LangB: "ko-KR"}, true},
		{Config{LangA: "ko-KR", LangB: "en-US"}, false}, // Scribe can't transcribe Korean
		{Config{LangA: "zh-TW", LangB: "en-US"}, false}, // nor Traditional Chinese speech
		{Config{LangA: "en-US", LangB: "en-US"}, true},  // translating to the spoken language translates nothing
		{Config{LangA: "en-US", LangB: "xx-XX"}, false},
		{Config{LangA: "auto", LangB: "zh-CN"}, true},
		{Config{LangA: "en-US", LangB: "auto"}, false}, // auto is no target
	}
	for _, tt := range tests {
		if err := tt.cfg.Validate(); (err == nil) != tt.valid {
			t.Errorf("%+v: err %v, want valid=%v", tt.cfg, err, tt.valid)
		}
	}
}

func TestMarkdownKeepsTranscriptOrderAndGaps(t *testing.T) {
	d := store.SessionDetail{
		SessionSummary: store.SessionSummary{Mode: store.ModeCaptions, LangA: "en-US", LangB: "zh-CN", Title: "Demo", Translated: true},
		Recap:          "# Recap\n- done",
		Segments: []store.Segment{
			{Lang: "en-US", Text: "First", Translation: "第一", TranslationLang: "zh-CN"},
			{Lang: store.GapLang},
			{Lang: "en-US", Text: "Second"},
		},
	}
	md := Markdown(d)
	if !strings.Contains(md, "English · 中文 (简体)") {
		t.Fatalf("markdown lacks the languages:\n%s", md)
	}
	first, gap, second := strings.Index(md, "- First\n  - *第一*"), strings.Index(md, "connection lost"), strings.Index(md, "- Second")
	if !strings.HasPrefix(md, "# Demo\n") || first < 0 || gap < first || second < gap {
		t.Fatalf("markdown out of order or missing parts:\n%s", md)
	}
	if !strings.Contains(md, "## Recap\n\n# Recap\n- done") {
		t.Fatalf("markdown lacks the recap:\n%s", md)
	}
}

func TestMarkdownAppendsSectionsAfterTheTranscript(t *testing.T) {
	d := store.SessionDetail{
		SessionSummary: store.SessionSummary{Mode: store.ModeCaptions, LangA: "en-US", LangB: "zh-CN", Title: "Demo"},
		Segments:       []store.Segment{{Lang: "en-US", Text: "Last turn."}},
	}
	md := Markdown(d, "## Ask\n\nWhat was decided?\n\n", "## Notes\n\nNone.")
	turn, ask, notes := strings.Index(md, "- Last turn."), strings.Index(md, "\n## Ask\n\nWhat was decided?\n"), strings.Index(md, "\n## Notes\n\nNone.\n")
	if turn < 0 || ask < turn || notes < ask {
		t.Fatalf("sections missing or out of order:\n%s", md)
	}
	if strings.HasSuffix(md, "\n\n") {
		t.Fatalf("a section's trailing blank lines leak into the document:\n%q", md)
	}
}

func TestMarkdownListsHighlightsAfterTheRecap(t *testing.T) {
	d := store.SessionDetail{
		SessionSummary: store.SessionSummary{Mode: store.ModeCaptions, LangA: "en-US", LangB: "zh-CN", Title: "Demo"},
		Recap:          "Settled pricing.",
		Segments: []store.Segment{
			{Seq: 0, Lang: "en-US", Text: "Alex, can you take it?", StartMs: 61_000},
			{Seq: 1, Lang: "en-US", Text: "The review could block us.", StartMs: 3_725_000},
			{Seq: 2, Lang: "en-US", Text: "Annual goes to forty-nine.", StartMs: 3_730_000},
		},
		Highlights: []store.Highlight{
			{Seq: 0, Kind: store.KindAskedYou, Dismissed: true},
			{Seq: 1, Kind: store.KindHeadsUp},
			{Seq: 2, Kind: store.KindDecision},
		},
	}
	md := Markdown(d)
	want := "## Recap\n\nSettled pricing.\n\n## Highlights\n\n" +
		"- **Heads-up** [1:02:05] The review could block us.\n" +
		"- **Decision** [1:02:10] Annual goes to forty-nine.\n\n## Transcript\n"
	if !strings.Contains(md, want) {
		t.Fatalf("markdown lacks the highlights after the recap, the dismissed one left out:\n%s", md)
	}
	d.Highlights = []store.Highlight{{Seq: 0, Kind: store.KindAskedYou, Dismissed: true}}
	if strings.Contains(Markdown(d), "## Highlights") {
		t.Fatal("a session whose only highlight was dismissed lists highlights")
	}
}

func TestMarkdownListsPolishedHighlightsByTheirSentence(t *testing.T) {
	polish := func(text string) *store.Polish {
		return &store.Polish{ByAgent: "Claude Code", Text: text, Confidence: 0.9}
	}
	folded := polish("Annual pricing goes to $49.")
	folded.FoldedInto = "first"
	corrected := polish("You were asked to own the pricing copy.")
	corrected.Kind = store.KindAskedYou
	d := store.SessionDetail{
		SessionSummary: store.SessionSummary{Mode: store.ModeCaptions, LangA: "en-US", LangB: "zh-CN", Title: "Demo"},
		Recap:          "Settled pricing.",
		Segments: []store.Segment{
			{Seq: 0, Lang: "en-US", Text: "Can someone own the copy?", StartMs: 61_000},
			{Seq: 1, Lang: "en-US", Text: "Ship Monday or wait?", StartMs: 70_000},
			{Seq: 2, Lang: "en-US", Text: "Annual goes to forty-nine.", StartMs: 80_000},
			{Seq: 3, Lang: "en-US", Text: "So forty-nine, yes.", StartMs: 90_000},
		},
		Highlights: []store.Highlight{
			{ID: "action", Seq: 0, Kind: store.KindAction, Polish: corrected},
			{Seq: 1, Kind: store.KindDebate, Polish: &store.Polish{ByAgent: "Claude Code", Confidence: 0.9,
				Debate: &store.Debate{Topic: "Ship Monday or wait a week", Status: store.DebateDecided}}},
			{ID: "first", Seq: 2, Kind: store.KindDecision, Polish: polish("Annual pricing goes to $49.")},
			{Seq: 3, Kind: store.KindDecision, Polish: folded},
		},
	}
	want := "## Highlights\n\n" +
		"- **Asked you** [01:01] You were asked to own the pricing copy.\n" +
		"- **Debate** [01:10] Ship Monday or wait a week (decided)\n" +
		"- **Decision** [01:20] Annual pricing goes to $49.\n\n"
	if md := Markdown(d); !strings.Contains(md, want) {
		t.Fatalf("markdown lacks the polished highlights, the restatement left out:\n%s", md)
	}
	// An agent's sentence is text, never Markdown that loads or links.
	d.Highlights[0].Polish.Text = "See ![x](https://e.example/?q=pricing) <b>now</b>"
	if md := Markdown(d); !strings.Contains(md, `[01:01] See \!\[x\]\(https://e.example/?q=pricing\) \<b\>now\</b\>`+"\n") {
		t.Fatalf("the agent's sentence is not escaped:\n%s", md)
	}
	// Its first dismissed, the restatement is listed in its place.
	d.Highlights[2].Dismissed = true
	if md := Markdown(d); !strings.Contains(md, "- **Decision** [01:30] Annual pricing goes to $49.\n") {
		t.Fatalf("dismissing the first hid its restatement:\n%s", md)
	}
}
