package ui

import (
	"reflect"
	"testing"

	"github.com/onyx-dot-app/Capy/internal/agent/ask"
)

// TestLinkAnswerLinksCitationsWithTheirOpenings links a session answer's
// citations to its turns, their openings the tooltips, and leaves the rest
// of its Markdown, and brackets that are no turn's time, as they are.
func TestLinkAnswerLinksCitationsWithTheirOpenings(t *testing.T) {
	turns := map[string]map[int64]string{"s1": {31_000: `Pricing "v2" is up`}}
	got := linkAnswer("Three, due **before Friday**:\n\n1. Maya: update pricing [00:31]\n- Leo [1:02:03]\n* see [notes] and [99:99]", "s1", nil, turns)
	want := "Three, due **before Friday**:\n\n" +
		`1. Maya: update pricing [00:31](capy-turn:s1/31000 "Pricing \"v2\" is up")` + "\n" +
		`- Leo [1:02:03](capy-turn:s1/3723000 "Turn at 1:02:03")` + "\n" +
		"* see [notes] and [99:99]"
	if got.src != want {
		t.Fatalf("linked\n%s\nwant\n%s", got.src, want)
	}
	if !reflect.DeepEqual(got.cited, []string{"s1"}) {
		t.Fatalf("cited %v", got.cited)
	}
}

// TestACrossSessionCitationCitesTheTitleBeforeIt links titles, the
// longer of two that overlap, and gives each citation the session named
// last before it; one before any title cites nothing.
func TestACrossSessionCitationCitesTheTitleBeforeIt(t *testing.T) {
	sessions := []ask.Session{{ID: "s1", Title: "Weekly sync"}, {ID: "s2", Title: "Weekly sync with [Ana]"}, {ID: "s3", Title: "Pricing review"}}
	got := linkAnswer("First [00:01]. In Weekly sync with [Ana] you agreed [00:10].\nPricing review: ship [02:00]", "", sessions, nil)
	want := `First [00:01]. In [Weekly sync with \[Ana\]](capy-session:s2 "Show Session") you agreed [00:10](capy-turn:s2/10000 "Turn at 00:10").` + "\n" +
		`[Pricing review](capy-session:s3 "Show Session"): ship [02:00](capy-turn:s3/120000 "Turn at 02:00")`
	if got.src != want {
		t.Fatalf("linked\n%s\nwant\n%s", got.src, want)
	}
	if !reflect.DeepEqual(got.cited, []string{"s2", "s3"}) {
		t.Fatalf("cited %v", got.cited)
	}
}

// TestLinkAnswerLeavesCodeAlone links nothing in code spans or fenced
// code, where a link would show as its syntax.
func TestLinkAnswerLeavesCodeAlone(t *testing.T) {
	sessions := []ask.Session{{ID: "s1", Title: "Weekly sync"}}
	text := "Run `Weekly sync [00:31]` then Weekly sync\n```\nWeekly sync [00:31]\n```\n"
	got := linkAnswer(text, "", sessions, nil)
	want := "Run `Weekly sync [00:31]` then [Weekly sync](capy-session:s1 \"Show Session\")\n```\nWeekly sync [00:31]\n```\n"
	if got.src != want {
		t.Fatalf("linked\n%s\nwant\n%s", got.src, want)
	}
}

func TestPlainDropsTheMarkdown(t *testing.T) {
	got := plain("Three, due **before Friday**:\n\n1. Maya: update [pricing](https://example.com) [00:31]\n2. Leo\n- Ana\n\n| A | B |\n|---|---|\n| 1 | 2 |")
	if want := "Three, due before Friday:\n1. Maya: update pricing [00:31]\n2. Leo\n• Ana\nA\tB\n1\t2"; got != want {
		t.Fatalf("plain %q, want %q", got, want)
	}
}

func TestOpenLinkOpensTurnsAndSessions(t *testing.T) {
	h := &askHost{}
	p := &panel{a: &AddOn{host: h}}
	p.openLink(nil, "capy-turn:a%2Fb/31000")
	p.openLink(nil, "capy-session:s2")
	p.openLink(nil, "capy-turn:s1/not-a-time") // ignored
	if want := []string{"a/b@31s", "s2"}; !reflect.DeepEqual(h.shown, want) {
		t.Fatalf("shown %v, want %v", h.shown, want)
	}
}
