package store

import (
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// HighlightKind is what a highlighted turn is to the user.
type HighlightKind string

// The kinds of highlight, as Decisions is asked about them.
const (
	// KindAskedYou is a question, request or task aimed at the user.
	KindAskedYou HighlightKind = "asked_you"
	// KindMentionedYou is the user talked about, without being asked
	// anything.
	KindMentionedYou HighlightKind = "mentioned_you"
	// KindDecision is something the group settled.
	KindDecision HighlightKind = "decision"
	// KindAction is a task someone else takes or is given.
	KindAction HighlightKind = "action"
	// KindAnnouncement is news that changes things for the people there.
	KindAnnouncement HighlightKind = "announcement"
	// KindHeadsUp is a risk, blocker or threatened deadline.
	KindHeadsUp HighlightKind = "heads_up"
	// KindDebate is people disagreeing on what to do or what is true. An
	// agent's polish gives it its question and sides, and may add one
	// Decisions missed.
	KindDebate HighlightKind = "debate"
)

// HighlightKinds are the kinds, in the order one wins over another on a
// turn: the user's first.
var HighlightKinds = []HighlightKind{KindAskedYou, KindMentionedYou, KindHeadsUp, KindDecision, KindDebate, KindAction, KindAnnouncement}

// ForYou reports whether k is about the user.
func (k HighlightKind) ForYou() bool { return k == KindAskedYou || k == KindMentionedYou }

// Label is k as a highlight's note names it: "Asked you".
func (k HighlightKind) Label() string {
	switch k {
	case KindAskedYou:
		return "Asked you"
	case KindMentionedYou:
		return "Mentioned you"
	case KindDecision:
		return "Decision"
	case KindAction:
		return "Action"
	case KindAnnouncement:
		return "Announcement"
	case KindHeadsUp:
		return "Heads-up"
	case KindDebate:
		return "Debate"
	}
	return string(k)
}

// Definition is what a highlight of kind k is, in a sentence, as an
// agent that polishes highlights is told. Decisions' questions keep their
// own wording, which its quality eval tuned.
func (k HighlightKind) Definition() string {
	switch k {
	case KindAskedYou:
		return "A question, request or task aimed at the listener."
	case KindMentionedYou:
		return "The listener talked about, in a way they would want to know, without being asked anything."
	case KindDecision:
		return "A decision the group made or confirmed: a choice settled, an option dropped, a plan locked. Proposals and opinions are not decisions."
	case KindAction:
		return "A concrete task someone other than the listener commits to or is given, with an owner."
	case KindAnnouncement:
		return "News that changes things for the people in the meeting: a launch, an approval, a policy, a date, an organisational change."
	case KindHeadsUp:
		return "A serious risk, blocker or threatened deadline the group should act on."
	case KindDebate:
		return "People disagreeing on what to do or what is true: two or more positions held by different people."
	}
	return ""
}

// known reports whether k is a kind a highlight may have.
func (k HighlightKind) known() bool { return slices.Contains(HighlightKinds, k) }

// Highlight marks a turn of a session that matters: Decisions was sure it
// is of its kind. It points at the turn and holds no text of its own.
type Highlight struct {
	ID        string
	SessionID string
	// Seq is the turn's, a segment's Seq.
	Seq  int
	Kind HighlightKind
	// Confidence is how sure Decisions was, from 0 to 1.
	Confidence float64
	// Dismissed is a highlight the user found not important: it is kept,
	// so the turn is not marked again, but not shown.
	Dismissed bool
	CreatedAt time.Time
	// Polish is what the user's agent wrote of it, nil for none; a
	// debate's, once polished, always has its Debate. PolishedAt is when,
	// zero for none.
	Polish     *Polish
	PolishedAt time.Time
}

// ShownKind is the kind h shows as: its polish's correction, else Kind.
// Kind stays Decisions' for the highlighter's guards.
func (h Highlight) ShownKind() HighlightKind {
	if h.Polish != nil && h.Polish.Kind != "" {
		return h.Polish.Kind
	}
	return h.Kind
}

// Sentence is what h's polish says of it: its sentence, or a debate's
// topic; "" when it is not polished.
func (h Highlight) Sentence() string {
	switch {
	case h.Polish == nil:
		return ""
	case h.Polish.Debate != nil:
		return h.Polish.Debate.Topic
	}
	return h.Polish.Text
}

// FoldedAmong reports whether h restates another of hs, a session's
// highlights, which lists show in its place: only while that one is shown,
// so that dismissing it never hides h.
func (h Highlight) FoldedAmong(hs []Highlight) bool {
	if h.Polish == nil || h.Polish.FoldedInto == "" {
		return false
	}
	for _, o := range hs {
		if o.ID == h.Polish.FoldedInto {
			return !o.Dismissed
		}
	}
	return false
}

// TentativeBelow is the agent's confidence below which a polished
// highlight shows as tentative: muted, never hidden.
const TentativeBelow = 0.7

// Limits of a polish, in characters.
const (
	MaxPolishText  = 160 // a sentence
	MaxPolishField = 120 // owner, due, decider, condition, a side's position
	MaxDebateTopic = 80
	MaxAgentName   = 64
	MinDebateSides = 2
	MaxDebateSides = 4
	// MaxSideSpeakers is how many speakers a debate's side names.
	MaxSideSpeakers = 8
)

// Polish is what the user's agent wrote of a highlight: a sentence, its
// fields, its kind corrected, how sure it is, and a debate's sides.
type Polish struct {
	// ByAgent is the agent's name: "Claude Code".
	ByAgent string `json:"by_agent"`
	// Text is the sentence, on one line; "" only for a debate, whose
	// Topic says it.
	Text string `json:"text,omitempty"`
	// Owner and Due are a task's: Asked you, Action.
	Owner string `json:"owner,omitempty"`
	Due   string `json:"due,omitempty"`
	// DecidedBy and Condition are a decision's: a speaker, the listener or
	// "group", and what it hangs on.
	DecidedBy string `json:"decided_by,omitempty"`
	Condition string `json:"condition,omitempty"`
	// Kind is the agent's correction of the highlight's kind, "" for none.
	Kind HighlightKind `json:"kind,omitempty"`
	// Confidence is how sure the agent is, from 0 to 1.
	Confidence float64 `json:"confidence"`
	// FoldedInto is the ID of an earlier highlight this one restates, ""
	// for none.
	FoldedInto string `json:"folded_into,omitempty"`
	// Debate is a debate's, nil for any other kind.
	Debate *Debate `json:"debate,omitempty"`
}

// Tentative reports whether the agent was not sure enough to show p as
// it shows others.
func (p Polish) Tentative() bool { return p.Confidence < TentativeBelow }

// DebateStatus is where a debate stands.
type DebateStatus string

// The statuses of a debate.
const (
	DebateOpen      DebateStatus = "open"
	DebateConsensus DebateStatus = "consensus"
	DebateDecided   DebateStatus = "decided"
	DebateParked    DebateStatus = "parked"
)

// Debate is a question the meeting argued: its sides, where it stands,
// and the decision it produced.
type Debate struct {
	Topic  string       `json:"topic"`
	Sides  []Side       `json:"sides"`
	Status DebateStatus `json:"status"`
	// Decision is the ID of the Decision highlight it produced, "" for
	// none.
	Decision string `json:"decision,omitempty"`
}

// Side is one position of a debate, the speakers who held it, and the
// turns they said it in, by seq.
type Side struct {
	Position string   `json:"position"`
	Speakers []string `json:"speakers,omitempty"`
	Seqs     []int    `json:"seqs,omitempty"`
}

// Polishing is one pass's polish of a session's highlights, saved at At.
type Polishing struct {
	SessionID string
	At        time.Time
	// Polish is by highlight ID; a debate's is in Debates.
	Polish map[string]Polish
	// Debates are KindDebate highlights, each added on its turn or, when
	// that turn has a debate, updating it.
	Debates []Highlight
}

// Validate checks p against the contract.
func (p Polishing) Validate() error {
	if err := ValidateID("session id", p.SessionID); err != nil {
		return err
	}
	if p.At.IsZero() {
		return invalid("polished at", "is required")
	}
	for id, pol := range p.Polish {
		if err := ValidateID("highlight id", id); err != nil {
			return err
		}
		if pol.Debate != nil {
			return invalid("polish", "of a debate goes in debates")
		}
		if err := pol.Validate(); err != nil {
			return err
		}
	}
	for _, d := range p.Debates {
		if d.SessionID != p.SessionID {
			return invalid("debates", "must be of the session")
		}
		if d.Kind != KindDebate || d.Polish == nil {
			return invalid("debates", "must be of kind debate, polished")
		}
		if err := d.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks p against the contract.
func (p Polish) Validate() error {
	if err := polishText("agent", p.ByAgent, MaxAgentName, true); err != nil {
		return err
	}
	if err := polishText("text", p.Text, MaxPolishText, p.Debate == nil); err != nil {
		return err
	}
	for _, f := range []struct{ name, v string }{
		{"owner", p.Owner}, {"due", p.Due}, {"decided by", p.DecidedBy}, {"condition", p.Condition},
	} {
		if err := polishText(f.name, f.v, MaxPolishField, false); err != nil {
			return err
		}
	}
	if p.Kind != "" && (p.Kind == KindDebate || !p.Kind.known()) {
		return invalid("polish kind", "is unknown")
	}
	if math.IsNaN(p.Confidence) || p.Confidence < 0 || p.Confidence > 1 {
		return invalid("polish confidence", "must be from 0 to 1")
	}
	if p.FoldedInto != "" {
		if err := ValidateID("folded into", p.FoldedInto); err != nil {
			return err
		}
	}
	if p.Debate != nil {
		return p.Debate.validate()
	}
	return nil
}

func (d Debate) validate() error {
	if err := polishText("topic", d.Topic, MaxDebateTopic, true); err != nil {
		return err
	}
	if len(d.Sides) < MinDebateSides || len(d.Sides) > MaxDebateSides {
		return invalid("sides", "must be from 2 to 4")
	}
	for _, s := range d.Sides {
		if err := polishText("position", s.Position, MaxPolishField, true); err != nil {
			return err
		}
		if len(s.Speakers) > MaxSideSpeakers {
			return invalid("side speakers", "are too many")
		}
		for _, who := range s.Speakers {
			if err := polishText("speaker", who, MaxSpeakerLen, true); err != nil {
				return err
			}
		}
		for _, seq := range s.Seqs {
			if seq < 0 {
				return invalid("side seq", "must not be negative")
			}
		}
	}
	switch d.Status {
	case DebateOpen, DebateConsensus, DebateDecided, DebateParked:
	default:
		return invalid("debate status", "is unknown")
	}
	if d.Decision != "" {
		return ValidateID("debate decision", d.Decision)
	}
	return nil
}

// polishText checks a field of a polish: one line of at most max
// characters, of valid UTF-8, and present when required.
func polishText(field, v string, max int, required bool) error {
	switch {
	case required && strings.TrimSpace(v) == "":
		return invalid(field, "is required")
	case !utf8.ValidString(v):
		return invalid(field, "is not valid UTF-8")
	case utf8.RuneCountInString(v) > max:
		return invalid(field, "is too long")
	case strings.ContainsAny(v, "\r\n"):
		return invalid(field, "must be one line")
	}
	return nil
}

// Validate checks h against the contract.
func (h Highlight) Validate() error {
	if err := ValidateID("highlight id", h.ID); err != nil {
		return err
	}
	if err := ValidateID("session id", h.SessionID); err != nil {
		return err
	}
	if h.Seq < 0 {
		return invalid("seq", "must not be negative")
	}
	if !h.Kind.known() {
		return invalid("kind", "is unknown")
	}
	if math.IsNaN(h.Confidence) || h.Confidence < 0 || h.Confidence > 1 {
		return invalid("confidence", "must be from 0 to 1")
	}
	if h.CreatedAt.IsZero() {
		return invalid("created at", "is required")
	}
	if h.Polish != nil && (h.Kind == KindDebate) != (h.Polish.Debate != nil) {
		return invalid("debate", "is a debate's polish, and only a debate's")
	}
	if h.Polish != nil {
		return h.Polish.Validate()
	}
	return nil
}
