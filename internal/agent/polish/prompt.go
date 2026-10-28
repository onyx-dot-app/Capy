package polish

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/onyx-dot-app/Capy/internal/lang"
	"github.com/onyx-dot-app/Capy/internal/profile"
	"github.com/onyx-dot-app/Capy/internal/store"
)

// Bounds of a pass's prompts.
const (
	maxPrompt = 32 << 10 // bytes a prompt, the fixed instructions included
	maxTurn   = 2 << 10  // bytes of a turn's text
	before    = 3        // turns of context before a highlight
	after     = 2        // and after it
)

// Input is what a pass reads: the session as the store has it, the
// listener, and whether it is the final pass.
type Input struct {
	Session store.SessionDetail
	Profile profile.Profile
	Final   bool
}

// batch is one prompt of a pass: the turns it sent, by seq, and the
// speakers among them, which its ops may cite.
type batch struct {
	prompt   string
	sent     map[int]bool
	speakers []string
}

// targets are the highlights a pass polishes, debates included, in seq
// order: those not polished yet, or every one for the final pass; never a
// dismissed one.
func targets(in Input) []store.Highlight {
	var out []store.Highlight
	for _, h := range in.Session.Highlights {
		if !h.Dismissed && (in.Final || h.Polish == nil) {
			out = append(out, h)
		}
	}
	return out
}

// prompts are the prompts of a pass: each carries the highlights to
// polish, with the turns around them, in seq order, as many as fit
// maxPrompt. None when there is nothing to polish.
func prompts(in Input) []batch {
	todo := targets(in)
	if len(todo) == 0 {
		return nil
	}
	code := language(in.Session)
	turns := spoken(in.Session.Segments)
	head := instructions + preamble(in, code)
	due := map[string]bool{}
	for _, h := range todo {
		due[h.ID] = true
	}
	var out []batch
	cur := map[int]bool{}
	flush := func() {
		if len(cur) > 0 {
			out = append(out, render(in, head, turns, cur, code, due))
			cur = map[int]bool{}
		}
	}
	for _, h := range todo {
		win := window(turns, h.Seq)
		next := clone(cur)
		for _, seq := range win {
			next[seq] = true
		}
		if len(cur) > 0 && len(render(in, head, turns, next, code, due).prompt) > maxPrompt {
			flush()
			next = map[int]bool{}
			for _, seq := range win {
				next[seq] = true
			}
		}
		cur = next
	}
	flush()
	return out
}

func clone(m map[int]bool) map[int]bool {
	out := make(map[int]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// spoken are the turns of segs, the gaps left out.
func spoken(segs []store.Segment) []store.Segment {
	return slices.DeleteFunc(slices.Clone(segs), func(s store.Segment) bool { return s.Lang == store.GapLang })
}

// window is the seqs of the turns around the turn of seq: before it and
// after it, it included.
func window(turns []store.Segment, seq int) []int {
	i := slices.IndexFunc(turns, func(s store.Segment) bool { return s.Seq >= seq })
	if i < 0 {
		return nil
	}
	var out []int
	for _, s := range turns[max(i-before, 0):min(i+after+1, len(turns))] {
		out = append(out, s.Seq)
	}
	return out
}

// render is the prompt of the turns of sent: the head, the highlights on
// those turns, those due marked to polish, and every debate, which a
// prompt may update; then those turns in order, in language code.
func render(in Input, head string, turns []store.Segment, sent map[int]bool, code string, due map[string]bool) batch {
	var b strings.Builder
	b.WriteString(head)
	seqs := map[string]int{}
	for _, h := range in.Session.Highlights {
		seqs[h.ID] = h.Seq
	}
	b.WriteString("<highlights>\n")
	for _, h := range in.Session.Highlights {
		if sent[h.Seq] || h.Kind == store.KindDebate {
			b.WriteString(highlightLine(h, due[h.ID], seqs))
		}
	}
	b.WriteString("</highlights>\n<excerpts>\n")
	var speakers []string
	prev := -1
	for i, s := range turns {
		if !sent[s.Seq] {
			continue
		}
		if prev >= 0 && i != prev+1 {
			b.WriteString("…\n")
		}
		prev = i
		speaker := speakerOf(s)
		if !slices.Contains(speakers, speaker) {
			speakers = append(speakers, speaker)
		}
		fmt.Fprintf(&b, "[%s %s %s] %s\n", turnID(s.Seq), stamp(s.StartMs), speaker, cut(oneLine(textIn(s, code)), maxTurn))
	}
	b.WriteString("</excerpts>\n")
	return batch{prompt: b.String(), sent: sent, speakers: speakers}
}

// language is the language a pass writes in and sends turns in, as the
// recap is written and Highlights checks: the translation's, when the
// session translates into a language Summarizer writes, else "", each
// turn as said.
func language(d store.SessionDetail) string {
	if d.Translated && lang.Summarizable(d.LangB) {
		return d.LangB
	}
	return ""
}

// languageName is how the prompt names language code.
func languageName(code string) string {
	if l, ok := lang.Lookup(code); ok && code != "" {
		return l.Name
	}
	return "the language most of the excerpts are in"
}

// textIn is s in language code: its translation when it was said in
// another.
func textIn(s store.Segment, code string) string {
	if code != "" && s.Lang != code && s.TranslationLang == code && s.Translation != "" {
		return s.Translation
	}
	return s.Text
}

func speakerOf(s store.Segment) string {
	if sp := oneLine(s.Speaker); sp != "" {
		return sp
	}
	return "Unknown"
}

// turnID is how the prompt names the turn of seq: "t12".
func turnID(seq int) string { return "t" + strconv.Itoa(seq) }

// seqOf is the seq of a turn ID, false for one that is not.
func seqOf(id string) (int, bool) {
	n, ok := strings.CutPrefix(strings.TrimSpace(id), "t")
	if !ok {
		return 0, false
	}
	seq, err := strconv.Atoi(n)
	return seq, err == nil && seq >= 0 && strconv.Itoa(seq) == n
}

func stamp(ms int64) string {
	s := ms / 1000
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

// oneLine is s on one line, without angle brackets, so that meeting text
// cannot close the prompt's tags.
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '<' || r == '>' {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// cut is s cut to at most n bytes, on a character's edge.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// preamble is the part of every prompt of a pass after its instructions:
// the language and the listener.
func preamble(in Input, code string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<language>%s</language>\n", languageName(code))
	p := in.Profile
	switch {
	case p.Name == "":
		b.WriteString("<listener>not known</listener>\n")
	case len(p.Aliases) > 0:
		fmt.Fprintf(&b, "<listener>%s (also: %s)</listener>\n", oneLine(p.Name), oneLine(strings.Join(p.Aliases, ", ")))
	default:
		fmt.Fprintf(&b, "<listener>%s</listener>\n", oneLine(p.Name))
	}
	return b.String()
}

// highlightLine is how the prompt lists h: its turn, its kind, and
// whether to polish it, its polish so far, or that the user dismissed it.
func highlightLine(h store.Highlight, todo bool, seqs map[string]int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", turnID(h.Seq), h.ShownKind())
	p := h.Polish
	switch {
	case h.Dismissed && p != nil && p.Debate != nil:
		fmt.Fprintf(&b, " · %q · DISMISSED BY USER", oneLine(p.Debate.Topic))
	case h.Dismissed:
		b.WriteString(" · DISMISSED BY USER")
	case p != nil && p.Debate != nil:
		if todo {
			b.WriteString(" · to polish again")
		}
		fmt.Fprintf(&b, " · %q · %s", oneLine(p.Debate.Topic), p.Debate.Status)
		if seq, ok := seqs[p.Debate.Decision]; ok {
			b.WriteString(" → " + turnID(seq))
		}
	case todo && p == nil:
		b.WriteString(" · to polish")
	case p != nil:
		if todo {
			b.WriteString(" · to polish again")
		}
		fmt.Fprintf(&b, " · polished: %q", oneLine(p.Text))
		for _, f := range []struct{ name, v string }{
			{"owner", p.Owner}, {"due", p.Due}, {"decided_by", p.DecidedBy}, {"condition", p.Condition},
		} {
			if f.v != "" {
				fmt.Fprintf(&b, " %s=%q", f.name, oneLine(f.v))
			}
		}
		if seq, ok := seqs[p.FoldedInto]; ok {
			b.WriteString(" · folded into " + turnID(seq))
		}
	}
	b.WriteString("\n")
	return b.String()
}
