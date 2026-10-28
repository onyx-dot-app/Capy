package session

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/lang"
	"github.com/onyx-dot-app/Capy/internal/profile"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

// How often Highlights checks, and how sure Decisions must be: How many
// in Settings is Fewer or More.
const (
	DefaultHighlightEvery = 5 * time.Minute
	ThresholdFewer        = 0.85
	ThresholdMore         = 0.70
	// ForYouMargin is how much surer Decisions must be of a kind for the
	// user than θ, as they are the only ones that may draw attention.
	ForYouMargin = 0.05
)

// Limits of a check.
const (
	leadInMs        = 2 * 60 * 1000 // context before the first new turn
	maxNewTurns     = 200           // choices of a request, under the API's 255
	maxMarksOfOther = 2             // marks a check makes of the kinds not for the user
	repeatTurns     = 2             // a kind again this close to its last mark is the same moment
	repeatMs        = 90 * 1000
	maxFailedChecks = 3
)

// Timing of the checks; variables, so tests run them sooner.
var (
	// nameCheckDelay is how long after a turn naming the user its check
	// waits, to include the reply.
	nameCheckDelay = 15 * time.Second
	// minCheckGap is how soon after the last check an early one may start.
	minCheckGap = 30 * time.Second
	// finalCheckBudget is how long the last check, at Stop, may take.
	finalCheckBudget = 10 * time.Second
)

// HighlightConfig is what Highlights does in a session: whether it
// checks, how often, for which kinds and how sure, and who the user is.
type HighlightConfig struct {
	On bool
	// Every is how long after one check the next may start; zero is
	// DefaultHighlightEvery.
	Every time.Duration
	// Kinds are the kinds asked about. The user's own are asked only when
	// the profile has a name.
	Kinds []store.HighlightKind
	// Threshold is θ, how sure Decisions must be of a turn; zero is
	// ThresholdFewer.
	Threshold float64
	Profile   profile.Profile
}

func (c HighlightConfig) every() time.Duration { return cmp.Or(c.Every, DefaultHighlightEvery) }

// Theta is θ, how sure Decisions must be of a turn to mark it.
func (c HighlightConfig) Theta() float64 { return cmp.Or(c.Threshold, ThresholdFewer) }

// asked are the kinds a check asks about, in the order of HighlightKinds.
func (c HighlightConfig) asked() []store.HighlightKind {
	var out []store.HighlightKind
	for _, k := range store.HighlightKinds {
		if slices.Contains(c.Kinds, k) && (!k.ForYou() || c.Profile.Name != "") {
			out = append(out, k)
		}
	}
	return out
}

// instructions are the questions asked of each kind. Each asks for a clear
// case and leans to none: a missed moment costs less than a wrong mark.
// The user's names go in the input, never here.
var instructions = map[store.HighlightKind]string{
	store.KindAskedYou: "Which NEW turn clearly asks, requests or assigns something to the listener (by name, other name or clear role)? " +
		"Most turns do not. If you are not sure, choose none.",
	store.KindMentionedYou: "Which NEW turn clearly talks about the listener, or a name listed as theirs, in a way they would want to know about, " +
		"without asking them anything? Passing mentions do not count. If you are not sure, choose none.",
	store.KindDecision: "Which NEW turn states a decision the group made or confirmed (a choice settled, an option dropped, a plan locked)? " +
		"Proposals, opinions and questions are not decisions. If you are not sure, choose none.",
	store.KindDebate: "Which NEW turn has someone clearly disagree with a position another person took on what to do or what is true " +
		"(an objection, a counter-proposal), leaving an open question between them? Questions, small caveats and agreement do not count. " +
		"If you are not sure, choose none.",
	store.KindAction: "Which NEW turn has someone other than the listener clearly commit to, or be given, a concrete task with an owner? " +
		"Vague intentions do not count. If you are not sure, choose none.",
	store.KindAnnouncement: "Which NEW turn announces news that changes things for the people in the meeting (a launch, approval, policy, date, " +
		"organisational change)? Small talk and routine status are not announcements. If you are not sure, choose none.",
	store.KindHeadsUp: "Which NEW turn raises a serious risk, blocker or threatened deadline that the group should act on? " +
		"Ordinary caveats do not count. If you are not sure, choose none.",
}

// noTurn is the choice of no turn.
const noTurn = "none"

func turnID(seq int) string { return "t" + strconv.Itoa(seq) }

// nameHeard reports whether text names one of names: as a whole word,
// case aside, or anywhere for a name written without spaces between
// words, as Chinese and Japanese are.
func nameHeard(text string, names []string) bool {
	text = strings.ToLower(text)
	for _, name := range names {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if strings.ContainsFunc(name, unspaced) {
			if strings.Contains(text, name) {
				return true
			}
			continue
		}
		for i := 0; ; {
			j := strings.Index(text[i:], name)
			if j < 0 {
				break
			}
			at, end := i+j, i+j+len(name)
			before, _ := utf8.DecodeLastRuneInString(text[:at])
			after, _ := utf8.DecodeRuneInString(text[end:])
			if (at == 0 || !wordRune(before)) && (end == len(text) || !wordRune(after)) {
				return true
			}
			i = at + 1
		}
	}
	return false
}

func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// listenFor are the names a turn naming the user is heard by: the
// profile's, and the first word of a name of several, as "Alex" of "Alex
// Rivera", since people call each other by their first names.
func listenFor(p profile.Profile) []string {
	names := p.Names()
	for _, n := range p.Names() {
		if first, _, ok := strings.Cut(strings.TrimSpace(n), " "); ok && utf8.RuneCountInString(first) > 1 &&
			!strings.ContainsFunc(first, unspaced) && !slices.Contains(names, first) {
			names = append(names, first)
		}
	}
	return names
}

// unspaced reports a letter of a script written without spaces between
// words.
func unspaced(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul, unicode.Thai)
}

// checkLanguage is the language a check sends turns in, as a recap is
// written: the translation's, to, when the session translates and
// Summarizer writes it, else "", each turn as said.
func checkLanguage(translate bool, to string) string {
	if translate && lang.Summarizable(to) {
		return to
	}
	return ""
}

// turnText is l in language code: its translation when it was spoken in
// another, on one line.
func turnText(l Line, code string) string {
	text := l.Text
	if code != "" && l.Lang != code && l.TranslationLang == code && l.Translation != "" {
		text = l.Translation
	}
	return strings.Join(strings.Fields(text), " ")
}

// maxTurnBytes bounds a turn in a check's input, so that one, however
// long, fits a request with the preamble.
const maxTurnBytes = 8 << 10

// turnLine is how a check's input shows l: "[t12 03:41 S2] text", a turn
// longer than maxTurnBytes cut.
func turnLine(l Line, code string) string {
	speaker := "?"
	if n, ok := strings.CutPrefix(l.Speaker, "Speaker "); ok {
		speaker = "S" + n
	} else if l.Speaker != "" {
		speaker = strings.Join(strings.Fields(l.Speaker), "_")
	}
	text := turnText(l, code)
	if len(text) > maxTurnBytes {
		text = strings.ToValidUTF8(text[:maxTurnBytes], "") + "…"
	}
	return "[" + turnID(l.Seq) + " " + stamp(l.StartMs) + " " + speaker + "] " + text + "\n"
}

// stamp is ms as 03:41, or 1:03:41 past the hour.
func stamp(ms int64) string {
	s := ms / 1000
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

// preamble starts every check's input with who the user is: the same
// each time, so the API can cache it.
func preamble(p profile.Profile) string {
	var b strings.Builder
	b.WriteString("You are helping one person follow a live meeting they are recording.\n")
	if p.Name == "" {
		b.WriteString("The listener's name is not known.")
	} else {
		b.WriteString("The listener: " + p.Name + ".")
		if len(p.Aliases) > 0 {
			b.WriteString(" Also called: " + strings.Join(p.Aliases, ", ") + ".")
		}
	}
	b.WriteString("\nEach turn is [id time speaker] text. Speaker labels are not names.\n\n")
	return b.String()
}

// request is one request of a check: the turns it asks about.
type request struct {
	req zoomai.DecideRequest
	// news are the turns it may choose, by seq.
	news []Line
}

// buildRequests are the requests that ask about news, consecutive turns:
// each at most maxNewTurns of them and zoomai.MaxDecideInput long, after
// the turns of the two minutes before it, from all, which it may not
// choose. Gaps are left out.
//
// A turn before fresh was judged by a check already: it is new again only
// to the questions of the user's kinds, as a name turn judged again is.
func buildRequests(all, news []Line, cfg HighlightConfig, code, ref string, fresh int) []request {
	asked := cfg.asked()
	news = slices.DeleteFunc(slices.Clone(news), func(l Line) bool { return l.Gap })
	if len(asked) == 0 || len(news) == 0 {
		return nil
	}
	head := preamble(cfg.Profile)
	var out []request
	for len(news) > 0 {
		first := news[0]
		var leadIn []string
		for _, l := range all {
			if !l.Gap && l.Seq < first.Seq && l.StartMs >= first.StartMs-leadInMs {
				leadIn = append(leadIn, turnLine(l, code))
			}
		}
		size := func(n int) int {
			total := len(head) + len("LEAD-IN (context only — do not choose these):\n\nNEW:\n")
			for _, s := range leadIn {
				total += len(s)
			}
			for _, l := range news[:n] {
				total += len(turnLine(l, code))
			}
			return total
		}
		// The lead-in gives way to the first new turn, then new turns stop
		// where the request would be too long.
		for len(leadIn) > 0 && size(1) > zoomai.MaxDecideInput {
			leadIn = leadIn[1:]
		}
		n := 1
		for n < len(news) && n < maxNewTurns && size(n+1) <= zoomai.MaxDecideInput {
			n++
		}
		var b strings.Builder
		b.WriteString(head)
		if len(leadIn) > 0 {
			b.WriteString("LEAD-IN (context only — do not choose these):\n")
			b.WriteString(strings.Join(leadIn, ""))
			b.WriteString("\n")
		}
		b.WriteString("NEW:\n")
		forYou, others := []string{noTurn}, []string{noTurn}
		for _, l := range news[:n] {
			b.WriteString(turnLine(l, code))
			forYou = append(forYou, turnID(l.Seq))
			if l.Seq >= fresh {
				others = append(others, turnID(l.Seq))
			}
		}
		r := request{news: news[:n], req: zoomai.DecideRequest{
			Model: zoomai.DecisionsModel, ReferenceID: ref, Input: strings.TrimSuffix(b.String(), "\n"),
		}}
		for _, k := range asked {
			choices := others
			if k.ForYou() {
				choices = forYou
			}
			if len(choices) > 1 {
				r.req.Questions = append(r.req.Questions, zoomai.ChoiceQuestion{Name: string(k), Instructions: instructions[k], Choices: choices})
			}
		}
		if len(r.req.Questions) > 0 {
			out = append(out, r)
		}
		news = news[n:]
	}
	return out
}

// mark is a turn a check found of a kind.
type mark struct {
	seq  int
	kind store.HighlightKind
	p    float64
}

// pickMarks reads a request's answers into marks, through the guards
// against noise, each of which only removes: a turn is marked only when
// Decisions is sure of that turn (θ, higher for the user's kinds), one
// per kind, not again for a moment marked already, one kind per turn,
// never a turn marked before, dismissed or not, and room at most of the
// kinds not for the user, the surest. all gives the turns' times.
func pickMarks(resp zoomai.DecideResponse, r request, cfg HighlightConfig, existing []store.Highlight, all []Line, room int) []mark {
	// A question may choose only the turns it was offered.
	choosable := map[string]map[string]int{}
	for _, q := range r.req.Questions {
		choosable[q.Name] = map[string]int{}
		for _, l := range r.news {
			if slices.Contains(q.Choices, turnID(l.Seq)) {
				choosable[q.Name][turnID(l.Seq)] = l.Seq
			}
		}
	}
	startMs := func(seq int) int64 {
		if i, ok := slices.BinarySearchFunc(all, seq, func(l Line, s int) int { return cmp.Compare(l.Seq, s) }); ok {
			return all[i].StartMs
		}
		return math.MinInt64 / 2
	}
	asked := cfg.asked()
	answered := map[string]bool{}
	var marks []mark
	for _, a := range resp.Answers {
		kind := store.HighlightKind(a.Name)
		if a.Refused || answered[a.Name] || !slices.Contains(asked, kind) {
			continue
		}
		answered[a.Name] = true
		best := mark{seq: -1, kind: kind}
		for value, p := range a.Probabilities {
			seq, ok := choosable[a.Name][value]
			if ok && p >= 0 && p <= 1 && (p > best.p || p == best.p && seq < best.seq) {
				best.seq, best.p = seq, p
			}
		}
		theta := cfg.Theta()
		if kind.ForYou() {
			theta += ForYouMargin
		}
		if best.seq < 0 || best.p < theta {
			continue
		}
		repeat := slices.ContainsFunc(existing, func(h store.Highlight) bool {
			return h.Seq == best.seq || h.Kind == kind && abs(h.Seq-best.seq) <= repeatTurns &&
				abs64(startMs(h.Seq)-startMs(best.seq)) <= repeatMs
		})
		if !repeat {
			marks = append(marks, best)
		}
	}
	// One kind per turn: the first of HighlightKinds.
	priority := func(k store.HighlightKind) int { return slices.Index(store.HighlightKinds, k) }
	slices.SortFunc(marks, func(a, b mark) int {
		return cmp.Or(cmp.Compare(a.seq, b.seq), cmp.Compare(priority(a.kind), priority(b.kind)))
	})
	marks = slices.CompactFunc(marks, func(a, b mark) bool { return a.seq == b.seq })
	// Of the kinds not for the user, the surest room.
	var others []mark
	for _, m := range marks {
		if !m.kind.ForYou() {
			others = append(others, m)
		}
	}
	slices.SortStableFunc(others, func(a, b mark) int { return cmp.Compare(b.p, a.p) })
	if len(others) > max(room, 0) {
		dropped := others[max(room, 0):]
		marks = slices.DeleteFunc(marks, func(m mark) bool { return slices.Contains(dropped, m) })
	}
	return marks
}

func abs(n int) int { return max(n, -n) }

func abs64(n int64) int64 { return max(n, -n) }

// nextCheck is when the next check is due, after the last one started:
// the interval after it, or sooner for a turn that named the user, at
// nameAt, nameCheckDelay after it and minCheckGap after the last. There
// is none while no turn is new: silence costs nothing.
func nextCheck(last, nameAt time.Time, every time.Duration, news bool) (time.Time, bool) {
	if !news {
		return time.Time{}, false
	}
	due := last.Add(every)
	if !nameAt.IsZero() {
		early := nameAt.Add(nameCheckDelay)
		if gap := last.Add(minCheckGap); gap.After(early) {
			early = gap
		}
		if early.Before(due) {
			due = early
		}
	}
	return due, true
}

// errHighlightsFailing stops Highlights after maxFailedChecks checks in a
// row failed.
var errHighlightsFailing = errors.New("session: highlights checks keep failing")

// highlighter checks a running session's turns with Decisions, one check
// at a time, and saves the highlights. Its loop runs until Stop, then
// makes a last check.
type highlighter struct {
	r      *Run
	decide func(context.Context, zoomai.DecideRequest) (zoomai.DecideResponse, error)
	now    func() time.Time

	ctx    context.Context // the checks', canceled finalCheckBudget after stop
	cancel context.CancelFunc
	wake   chan struct{}
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once

	mu  sync.Mutex
	cfg HighlightConfig
	// judged is the seq of the first turn the next check sends as new,
	// and fresh that of the first no check judged yet: a name turn judged
	// again comes between them, new to the user's kinds only.
	judged, fresh int
	// carriedEnd is where the check ended that judged its unconfirmed
	// name turn again: a turn before it gets no third chance.
	carriedEnd int
	last       time.Time // when the last check started
	nameAt     time.Time // when a turn not yet checked named the user; zero for none
	failed     int       // checks in a row that failed
	// gen counts the times Highlights was turned on: a check that began
	// before one does not move judged.
	gen int
}

func newHighlighter(r *Run, cfg HighlightConfig, started time.Time) *highlighter {
	ctx, cancel := context.WithCancel(context.Background())
	return &highlighter{
		r: r, decide: r.deps.AI.Decide, now: time.Now, ctx: ctx, cancel: cancel,
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
		cfg: cfg, last: started,
	}
}

func (h *highlighter) signal() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// added notes a completed turn: one naming the user brings the next
// check forward. Nothing is drawn until Decisions confirms it.
func (h *highlighter) added(l Line) {
	h.mu.Lock()
	if h.cfg.On && h.nameAt.IsZero() && slices.ContainsFunc(h.cfg.asked(), store.HighlightKind.ForYou) &&
		nameHeard(l.Text, listenFor(h.cfg.Profile)) {
		h.nameAt = h.now()
	}
	h.mu.Unlock()
	h.signal()
}

// set applies cfg from the next check. Turned on, it judges the turns
// from now on.
func (h *highlighter) set(cfg HighlightConfig) {
	h.mu.Lock()
	if cfg.On && !h.cfg.On {
		n := len(h.r.Snapshot().Lines)
		h.judged, h.fresh, h.last, h.nameAt, h.gen = n, n, h.now(), time.Time{}, h.gen+1
	}
	h.cfg = cfg
	h.mu.Unlock()
	h.signal()
}

// finish makes the last check, within finalCheckBudget, and returns once
// it is made.
func (h *highlighter) finish() {
	h.once.Do(func() {
		time.AfterFunc(finalCheckBudget, h.cancel)
		close(h.stop)
	})
	<-h.done
}

func (h *highlighter) loop() {
	defer close(h.done)
	defer h.cancel()
	for {
		lines := h.r.Snapshot().Lines
		h.mu.Lock()
		due, ok := nextCheck(h.last, h.nameAt, h.cfg.every(), h.cfg.On && h.hasNews(lines))
		h.mu.Unlock()
		var timer *time.Timer
		var fire <-chan time.Time
		if ok {
			timer = time.NewTimer(max(due.Sub(h.now()), 0))
			fire = timer.C
		}
		select {
		case <-h.wake:
		case <-fire:
			if !h.check() {
				<-h.stop
				return
			}
		case <-h.stop:
			h.mu.Lock()
			on := h.cfg.On
			h.mu.Unlock()
			if on {
				h.check()
			}
			return
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// hasNews reports whether a turn no check judged yet awaits one: a name
// turn to judge again waits for what is said after it. Call with mu held.
func (h *highlighter) hasNews(lines []Line) bool {
	return slices.ContainsFunc(lines[min(h.fresh, len(lines)):], func(l Line) bool { return !l.Gap })
}

// check asks Decisions about the turns completed since the last check and
// saves the highlights. It returns false once Highlights stopped for the
// session: the key cannot use Decisions, or checks keep failing.
func (h *highlighter) check() bool {
	h.mu.Lock()
	snap := h.r.Snapshot() // under mu: a name turn added meanwhile keeps its nameAt
	cfg, from, fresh, carriedEnd, nameAt, gen := h.cfg, h.judged, h.fresh, h.carriedEnd, h.nameAt, h.gen
	h.last, h.nameAt = h.now(), time.Time{}
	h.mu.Unlock()
	if from >= len(snap.Lines) {
		return true
	}
	all, end := snap.Lines, len(snap.Lines)
	existing := slices.Clone(snap.Highlights)
	var marks []mark
	answered, room := end, maxMarksOfOther // answered: the first turn of the request that failed, if one did
	var err error
	for _, r := range buildRequests(all, all[from:], cfg, checkLanguage(snap.Config.Translate, snap.Config.LangB), snap.SessionID, fresh) {
		var resp zoomai.DecideResponse
		started := h.now()
		if resp, err = h.decide(h.ctx, r.req); err != nil {
			answered = r.news[0].Seq
			h.r.log.Warn("highlights check", "session", snap.SessionID, "turns", len(r.news), "err", err)
			break
		}
		got := pickMarks(resp, r, cfg, existing, all, room)
		for _, m := range got {
			existing = append(existing, store.Highlight{Seq: m.seq, Kind: m.kind})
			if !m.kind.ForYou() {
				room--
			}
		}
		marks = append(marks, got...)
		h.r.log.Info("highlights check", "session", snap.SessionID, "turns", len(r.news), "request", resp.RequestID,
			"took", h.now().Sub(started).Round(time.Millisecond), "marks", marksLog(got))
	}
	h.save(snap.SessionID, marks)

	// A turn that named the user and was not marked is judged again once,
	// by the user's kinds, in the next check, with what was said after it.
	judged := answered
	if slices.ContainsFunc(cfg.asked(), store.HighlightKind.ForYou) {
		for _, l := range all[max(from, carriedEnd):max(answered, from)] {
			if !l.Gap && !slices.ContainsFunc(marks, func(m mark) bool { return m.seq == l.Seq }) &&
				nameHeard(l.Text, listenFor(cfg.Profile)) {
				judged, carriedEnd = l.Seq, answered
				break
			}
		}
	}

	stopped := error(nil)
	h.mu.Lock()
	if h.gen == gen { // else turned on anew meanwhile: it judges from then on
		h.judged, h.fresh, h.carriedEnd = max(judged, from), max(answered, fresh), carriedEnd
	}
	switch {
	case err == nil:
		h.failed = 0
	case errors.Is(err, zoomai.ErrUnauthorized):
		stopped = err
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
	default:
		if h.nameAt.IsZero() {
			h.nameAt = nameAt // its early check is tried again
		}
		if h.failed++; h.failed >= maxFailedChecks {
			h.r.log.Warn("highlights stopped", "session", snap.SessionID, "failed", h.failed)
			stopped = errHighlightsFailing
		}
	}
	h.mu.Unlock()
	if stopped != nil {
		h.r.update(func(s *Snapshot) { s.HighlightsErr = stopped })
		return false
	}
	return true
}

// save stores marks as highlights and shows them.
func (h *highlighter) save(sessionID string, marks []mark) {
	if len(marks) == 0 {
		return
	}
	now := h.now()
	hs := make([]store.Highlight, len(marks))
	for i, m := range marks {
		hs[i] = store.Highlight{ID: uuid.NewString(), SessionID: sessionID, Seq: m.seq, Kind: m.kind, Confidence: m.p, CreatedAt: now}
	}
	slices.SortFunc(hs, func(a, b store.Highlight) int { return cmp.Compare(a.Seq, b.Seq) })
	hs, err := addFree(context.Background(), h.r.deps.Store, sessionID, hs)
	if err != nil {
		h.r.log.Error("save highlights", "session", sessionID, "err", err)
		return
	}
	if len(hs) == 0 {
		return
	}
	h.r.update(func(s *Snapshot) {
		s.Highlights = append(s.Highlights, hs...)
		slices.SortFunc(s.Highlights, func(a, b store.Highlight) int { return cmp.Compare(a.Seq, b.Seq) })
	})
	h.r.deps.highlighted(sessionID, false)
}

// addFree saves hs, but those on a turn that has a highlight by now: one
// an agent's polish saved as a debate while the check ran, which the
// snapshot the check read could not show. It returns those it saved.
func addFree(ctx context.Context, st store.Store, sessionID string, hs []store.Highlight) ([]store.Highlight, error) {
	err := st.AddHighlights(ctx, hs)
	if !errors.Is(err, store.ErrConflict) {
		return hs, err
	}
	d, gerr := st.GetSession(ctx, sessionID)
	if gerr != nil {
		return nil, errors.Join(err, gerr)
	}
	taken := map[int]bool{}
	for _, x := range d.Highlights {
		taken[x.Seq] = true
	}
	free := slices.DeleteFunc(slices.Clone(hs), func(x store.Highlight) bool { return taken[x.Seq] })
	switch {
	case len(free) == len(hs):
		return nil, err // no turn was taken: another conflict
	case len(free) == 0:
		return nil, nil
	}
	return free, st.AddHighlights(ctx, free)
}

// marksLog is marks as the log may show them: kinds and how sure, never
// text.
func marksLog(marks []mark) []string {
	out := make([]string, len(marks))
	for i, m := range marks {
		out[i] = string(m.kind) + ":" + strconv.FormatFloat(m.p, 'f', 2, 64)
	}
	return out
}

// EvalRequest is a request of a finished transcript's checks, as the
// quality eval replays them: the request, and the turns it may choose.
type EvalRequest struct {
	Req  zoomai.DecideRequest
	news []Line
}

// EvalRequests are the requests a recording of lines would have made,
// checking every interval of cfg by when the turns were said, in order.
// Name lines' early checks are left out: the eval judges what is marked,
// not how soon.
func EvalRequests(lines []Line, cfg HighlightConfig, ref string) []EvalRequest {
	var out []EvalRequest
	for _, r := range replayRequests(lines, cfg, "", ref) {
		out = append(out, EvalRequest{Req: r.req, news: r.news})
	}
	return out
}

// replayRequests are the requests a recording of lines would have made in
// language code, checking every interval of cfg by when the turns were
// said, in order, without name lines' early checks.
func replayRequests(lines []Line, cfg HighlightConfig, code, ref string) []request {
	var out []request
	every := cfg.every().Milliseconds()
	from := 0
	for from < len(lines) {
		end := from
		for end < len(lines) && lines[end].StartMs < (lines[from].StartMs/every+1)*every {
			end++
		}
		out = append(out, buildRequests(lines, lines[from:end], cfg, code, ref, 0)...)
		from = end
	}
	return out
}

// EvalMarks are the highlights resp marks for r, through the same guards
// as a recording's, after existing: their Seq, Kind and Confidence.
func EvalMarks(resp zoomai.DecideResponse, r EvalRequest, cfg HighlightConfig, existing []store.Highlight, lines []Line) []store.Highlight {
	var out []store.Highlight
	for _, m := range pickMarks(resp, request{req: r.Req, news: r.news}, cfg, existing, lines, maxMarksOfOther) {
		out = append(out, store.Highlight{Seq: m.seq, Kind: m.kind, Confidence: m.p})
	}
	return out
}

// FindHighlights finds the highlights of a saved session, a recording or
// a file: it replays the checks a recording of its turns would have made,
// as the quality eval scores them, and saves what they mark. cfg.On is
// not read. A turn marked or dismissed before is never marked again, so
// finding them again only adds. A request that fails ends it, the
// highlights found before it saved. It returns how many it found.
func FindHighlights(ctx context.Context, deps Deps, sessionID string, cfg HighlightConfig) (int, error) {
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	d, err := deps.Store.GetSession(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	lines := make([]Line, len(d.Segments))
	for i, seg := range d.Segments {
		lines[i] = Line{Seq: seg.Seq, StartMs: seg.StartMs, Lang: seg.Lang, Text: seg.Text, Speaker: seg.Speaker,
			Translation: seg.Translation, TranslationLang: seg.TranslationLang, Gap: seg.Lang == store.GapLang}
	}
	existing := slices.Clone(d.Highlights)
	var found []store.Highlight
	for _, r := range replayRequests(lines, cfg, checkLanguage(d.Translated, d.LangB), sessionID) {
		started := time.Now()
		var resp zoomai.DecideResponse
		if resp, err = deps.AI.Decide(ctx, r.req); err != nil {
			log.Warn("find highlights", "session", sessionID, "turns", len(r.news), "err", err)
			break
		}
		marks := pickMarks(resp, r, cfg, existing, lines, maxMarksOfOther)
		for _, m := range marks {
			h := store.Highlight{ID: uuid.NewString(), SessionID: sessionID, Seq: m.seq, Kind: m.kind, Confidence: m.p, CreatedAt: time.Now()}
			existing, found = append(existing, h), append(found, h)
		}
		log.Info("find highlights", "session", sessionID, "turns", len(r.news), "request", resp.RequestID,
			"took", time.Since(started).Round(time.Millisecond), "marks", marksLog(marks))
	}
	if len(found) > 0 {
		saved, serr := addFree(context.WithoutCancel(ctx), deps.Store, sessionID, found)
		if serr != nil {
			return 0, errors.Join(err, serr)
		}
		if found = saved; len(found) > 0 {
			deps.highlighted(sessionID, true)
		}
	}
	return len(found), err
}
