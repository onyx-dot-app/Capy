package polish

import (
	"cmp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/store"
)

// Bounds of what an answer may change.
const (
	maxOps = 50 // ops applied a pass; the rest are dropped
	// maxNewDebates is how many debates an answer may add: each takes a
	// turn from Highlights.
	maxNewDebates = 3
	// defaultConfidence is an op's that gives none: implied.
	defaultConfidence = 0.7
)

// outcome counts what a pass did with an answer's ops, by op: what the
// log says of it, never their text.
type outcome struct {
	kept, dropped map[string]int
}

func (o outcome) keep(name string) { o.kept[name]++ }
func (o outcome) drop(name string) { o.dropped[name]++ }

// applier applies one answer's ops to a session's highlights, as batch b
// sent them.
type applier struct {
	in    Input
	b     batch
	agent string
	now   time.Time
	names names

	bySeq    map[int]store.Highlight
	byID     map[string]store.Highlight
	polished map[string]store.Polish // this pass's, by highlight ID
	debates  map[int]store.Highlight // this pass's, by turn
	added    int                     // debates the pass adds, not updates
}

// apply keeps the ops the guards pass, at most maxOps of them, as the
// polish to save; ops is in the answer's order, a later op winning over
// an earlier one of the same highlight.
func apply(ops []op, in Input, b batch, agent string, now time.Time) (store.Polishing, outcome) {
	a := &applier{in: in, b: b, agent: agent, now: now, names: namesOf(in, b),
		bySeq: map[int]store.Highlight{}, byID: map[string]store.Highlight{},
		polished: map[string]store.Polish{}, debates: map[int]store.Highlight{}}
	for _, h := range in.Session.Highlights {
		a.bySeq[h.Seq], a.byID[h.ID] = h, h
	}
	out := outcome{kept: map[string]int{}, dropped: map[string]int{}}
	for i, o := range ops {
		name := o.Op
		if name != "polish" && name != "fold" && name != "debate" {
			name = "unknown"
		}
		ok := false
		switch {
		case i >= maxOps:
		case o.Op == "polish":
			ok = a.polish(o)
		case o.Op == "fold":
			ok = a.fold(o)
		case o.Op == "debate":
			ok = a.debate(o)
		}
		if ok {
			out.keep(name)
		} else {
			out.drop(name)
		}
	}
	a.settleFolds(out)
	p := store.Polishing{SessionID: in.Session.ID, At: now, Polish: a.polished}
	for _, d := range a.debates {
		p.Debates = append(p.Debates, d)
	}
	slices.SortFunc(p.Debates, func(x, y store.Highlight) int { return cmp.Compare(x.Seq, y.Seq) })
	return p, out
}

// sentHighlight is the highlight on the turn of id, when the batch sent
// that turn and the highlight may be polished: not a debate, not
// dismissed.
func (a *applier) sentHighlight(id string) (store.Highlight, bool) {
	seq, ok := seqOf(id)
	if !ok || !a.b.sent[seq] {
		return store.Highlight{}, false
	}
	h, ok := a.bySeq[seq]
	return h, ok && h.Kind != store.KindDebate && !h.Dismissed
}

// current is h's polish as this pass leaves it so far, nil for none.
func (a *applier) current(h store.Highlight) *store.Polish {
	if p, ok := a.polished[h.ID]; ok {
		return &p
	}
	return h.Polish
}

// shownKind is h's kind as this pass leaves it so far.
func (a *applier) shownKind(h store.Highlight) store.HighlightKind {
	if p := a.current(h); p != nil && p.Kind != "" {
		return p.Kind
	}
	return h.Kind
}

func (a *applier) polish(o op) bool {
	h, ok := a.sentHighlight(o.Turn)
	if !ok {
		return false
	}
	text := oneLine(o.Text)
	if text == "" || utf8.RuneCountInString(text) > store.MaxPolishText {
		return false
	}
	p := store.Polish{ByAgent: a.agent, Text: text, Confidence: confidence(o.Confidence)}
	// A debate has sides a polish op cannot give: a debate op adds one.
	if k := store.HighlightKind(o.Kind); k != h.Kind && k != store.KindDebate && slices.Contains(store.HighlightKinds, k) {
		p.Kind = k
	}
	switch cmp.Or(p.Kind, h.Kind) {
	case store.KindAskedYou, store.KindAction:
		p.Owner, p.Due = a.names.field(o.Owner, false), field(o.Due)
	case store.KindDecision:
		p.DecidedBy, p.Condition = a.names.field(o.DecidedBy, true), field(o.Condition)
	}
	if prev := a.current(h); prev != nil {
		p.FoldedInto = prev.FoldedInto
	}
	if p.Validate() != nil {
		return false
	}
	a.polished[h.ID] = p
	return true
}

// fold folds a polished highlight into an earlier one of the same kind,
// one that is not itself folded, nor one folded into itself: no chains.
func (a *applier) fold(o op) bool {
	h, ok := a.sentHighlight(o.Turn)
	if !ok {
		return false
	}
	into, ok := a.sentHighlight(o.Into)
	if !ok || into.Seq >= h.Seq || a.shownKind(into) != a.shownKind(h) {
		return false
	}
	cur := a.current(h)
	if cur == nil {
		return false
	}
	if p := a.current(into); p != nil && p.FoldedInto != "" {
		return false
	}
	for _, other := range a.in.Session.Highlights {
		if p := a.current(other); p != nil && p.FoldedInto == h.ID {
			return false
		}
	}
	p := *cur
	p.FoldedInto = into.ID
	a.polished[h.ID] = p
	return true
}

// debate adds a debate on a turn the batch sent that has no highlight,
// or updates a listed debate, named by its own turn. A new one goes only
// on a turn Decisions has judged, before its latest mark, so that it never
// takes a turn from a check to come; and never where a dismissed debate
// was, at its turn or its sides'.
func (a *applier) debate(o op) bool {
	seq, ok := seqOf(o.Anchor)
	if !ok {
		return false
	}
	existing, had := a.bySeq[seq]
	if had && (existing.Kind != store.KindDebate || existing.Dismissed) {
		return false
	}
	if !had && (!a.b.sent[seq] || seq >= a.lastMark() || a.dismissedDebate(seq)) {
		return false
	}
	if _, again := a.debates[seq]; !had && !again && a.added >= maxNewDebates {
		return false
	}
	d := store.Debate{Topic: oneLine(o.Topic), Status: store.DebateStatus(o.Status)}
	if d.Topic == "" || utf8.RuneCountInString(d.Topic) > store.MaxDebateTopic {
		return false
	}
	for _, s := range o.Sides {
		if sd, ok := a.side(s); ok {
			d.Sides = append(d.Sides, sd)
		}
	}
	switch d.Status {
	case store.DebateOpen, store.DebateConsensus, store.DebateDecided, store.DebateParked:
	default:
		d.Status = store.DebateOpen
		if had && existing.Polish != nil && existing.Polish.Debate != nil {
			d.Status = existing.Polish.Debate.Status
		}
	}
	if dseq, ok := seqOf(o.Decision); ok {
		if g, ok := a.bySeq[dseq]; ok && !g.Dismissed && g.Kind != store.KindDebate && a.shownKind(g) == store.KindDecision {
			d.Decision = g.ID
		}
	}
	conf := confidence(o.Confidence)
	h := store.Highlight{ID: uuid.NewString(), SessionID: a.in.Session.ID, Seq: seq, Kind: store.KindDebate,
		Confidence: conf, CreatedAt: a.now, Polish: &store.Polish{ByAgent: a.agent, Confidence: conf, Debate: &d}}
	if had {
		h.ID, h.CreatedAt = existing.ID, existing.CreatedAt
	}
	if !had && slices.ContainsFunc(d.Sides, func(s store.Side) bool { return slices.ContainsFunc(s.Seqs, a.dismissedDebate) }) {
		return false
	}
	if h.Validate() != nil {
		return false
	}
	if _, again := a.debates[seq]; !had && !again {
		a.added++
	}
	a.debates[seq] = h
	return true
}

// settleFolds undoes a fold that ops after it left wrong: into a
// highlight that now shows as another kind, or that is folded itself.
func (a *applier) settleFolds(out outcome) {
	for _, h := range a.in.Session.Highlights {
		p := a.current(h)
		if p == nil || p.FoldedInto == "" {
			continue
		}
		into, ok := a.byID[p.FoldedInto]
		if ok && a.shownKind(into) == a.shownKind(h) {
			if ip := a.current(into); ip == nil || ip.FoldedInto == "" {
				continue
			}
		}
		fixed := *p
		fixed.FoldedInto = ""
		a.polished[h.ID] = fixed
		out.drop("fold")
	}
}

// lastMark is the seq of the latest highlight, dismissed or not: the
// turns before it were all judged by Decisions. A debate polish added is
// always before one Decisions made, so it moves nothing.
func (a *applier) lastMark() int {
	last := -1
	for _, h := range a.in.Session.Highlights {
		last = max(last, h.Seq)
	}
	return last
}

// dismissedDebate reports whether seq is the turn of a debate the user
// dismissed, or of one of its sides.
func (a *applier) dismissedDebate(seq int) bool {
	for _, h := range a.in.Session.Highlights {
		if !h.Dismissed || h.Kind != store.KindDebate {
			continue
		}
		if h.Seq == seq {
			return true
		}
		if h.Polish != nil && h.Polish.Debate != nil {
			for _, s := range h.Polish.Debate.Sides {
				if slices.Contains(s.Seqs, seq) {
					return true
				}
			}
		}
	}
	return false
}

// side is s as a debate keeps it: its position, the speakers the batch
// may name, and the turns it sent; false for a side with no position.
func (a *applier) side(s side) (store.Side, bool) {
	out := store.Side{Position: oneLine(s.Position)}
	if out.Position == "" || utf8.RuneCountInString(out.Position) > store.MaxPolishField {
		return out, false
	}
	for _, who := range s.Speakers {
		if n := a.names.field(who, false); n != "" && len(n) <= store.MaxSpeakerLen && !slices.Contains(out.Speakers, n) &&
			len(out.Speakers) < store.MaxSideSpeakers {
			out.Speakers = append(out.Speakers, n)
		}
	}
	for _, t := range s.Turns {
		if seq, ok := seqOf(t); ok && a.b.sent[seq] && !slices.Contains(out.Seqs, seq) {
			out.Seqs = append(out.Seqs, seq)
		}
	}
	return out, true
}

// confidence is an op's, 0.7 when it gives none, within 0 and 1.
func confidence(c *float64) float64 {
	if c == nil {
		return defaultConfidence
	}
	return min(max(*c, 0), 1)
}

// field is a polish field's value: one line, "" when too long.
func field(s string) string {
	s = oneLine(s)
	if utf8.RuneCountInString(s) > store.MaxPolishField {
		return ""
	}
	return s
}

// names are the names an op may give an owner, a decider or a side: the
// speakers the batch sent, the listener's, and names its turns say.
type names struct {
	known []string // folded
	said  string   // the batch's turns, folded
}

func namesOf(in Input, b batch) names {
	var n names
	for _, s := range append(append([]string(nil), b.speakers...), in.Profile.Names()...) {
		n.known = append(n.known, strings.ToLower(oneLine(s)))
	}
	code := language(in.Session)
	var said strings.Builder
	for _, s := range in.Session.Segments {
		if b.sent[s.Seq] {
			said.WriteString(strings.ToLower(oneLine(s.Text)) + "\n")
			said.WriteString(strings.ToLower(oneLine(textIn(s, code))) + "\n")
		}
	}
	n.said = said.String()
	return n
}

// field is name as an op may give it, "" when the batch gives no reason
// to believe it: a speaker sent, the listener, a name the turns say, or
// "group" for a decider.
func (n names) field(name string, group bool) string {
	name = field(name)
	folded := strings.ToLower(name)
	switch {
	case name == "":
		return ""
	case group && folded == "group":
		return "group"
	case slices.Contains(n.known, folded), saidAsWords(n.said, folded):
		return name
	}
	return ""
}

// saidAsWords reports whether name is in text as whole words: not a
// part of a longer word, as "Al" is of "Alex".
func saidAsWords(text, name string) bool {
	for i := 0; ; {
		at := strings.Index(text[i:], name)
		if at < 0 {
			return false
		}
		at += i
		end := at + len(name)
		prev, _ := utf8.DecodeLastRuneInString(text[:at])
		next, _ := utf8.DecodeRuneInString(text[end:])
		if (at == 0 || !wordRune(prev)) && (end == len(text) || !wordRune(next)) {
			return true
		}
		i = at + 1
	}
}

func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
