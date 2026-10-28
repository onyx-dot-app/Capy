// Package session runs one captions session: audio to Scribe Live, each
// completed turn to Translator and the store, and a Summarizer recap when
// it stops. It is the only package that joins the
// others.
package session

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/audio"
	"github.com/onyx-dot-app/Capy/internal/lang"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

// Timeouts and limits of a session.
const (
	finishTimeout      = 5 * time.Second  // wait for session.closed after the last frame
	translationTimeout = 10 * time.Second // drain in-flight translations at stop
	maxTranslations    = 3                // concurrent Translator calls
	maxReconnects      = 3
	stableConnection   = 30 * time.Second // a connection this old resets the reconnect attempts
	sendTimeout        = 5 * time.Second  // a send this slow means a dead connection
	maxDrafts          = 8                // pending turns remembered; older ones are stale
	frameMs            = 100              // audio in a frame of audio.FrameBytes
	bytesPerMs         = audio.SampleRate * 2 / 1000
	dialTimeout        = 10 * time.Second
)

// Status is where a session is in its life.
type Status string

// Statuses, in order.
const (
	StatusConnecting   Status = "connecting"
	StatusLive         Status = "live"
	StatusReconnecting Status = "reconnecting"
	StatusFinishing    Status = "finishing"
	StatusSummarizing  Status = "summarizing"
	StatusDone         Status = "done"
	StatusFailed       Status = "failed"
)

// Config chooses what a session does: LangA is the spoken language, or
// lang.Auto to detect it, LangB the language translations are in, which
// SetTarget changes, Translate whether turns are translated, which
// SetTranslate changes, and Highlights what Highlights marks, which
// SetHighlights changes.
type Config struct {
	Source     store.Source
	LangA      string
	LangB      string
	Translate  bool
	Highlights HighlightConfig
}

// Validate reports a configuration the APIs cannot serve.
func (c Config) Validate() error {
	if !lang.IsSpoken(c.LangA) {
		return fmt.Errorf("session: %q cannot be transcribed", c.LangA)
	}
	if !lang.IsTarget(c.LangB) {
		return fmt.Errorf("session: %q cannot be translated to", c.LangB)
	}
	if h := c.Highlights; h.Every < 0 || h.Threshold < 0 || h.Threshold > 1 {
		return errors.New("session: highlights need a positive interval and a threshold from 0 to 1")
	}
	return nil
}

// TranslationTarget is the language a turn spoken in from is translated to
// while translating to to, or "" when it is not translated: spoken in to,
// or in a language Translator does not know.
func TranslationTarget(from, to string) string {
	if from == to || !lang.IsTarget(from) || !lang.IsTarget(to) {
		return ""
	}
	return to
}

// Deps are the services a session uses.
type Deps struct {
	AI    *zoomai.Client
	Store store.Store
	Log   *slog.Logger // nil discards; never receives transcript text
	// Highlighted, if set, hears that Highlights saved highlights of a
	// session: final after its last check, or Find Highlights. It is
	// called on the checks' goroutine and must not block.
	Highlighted func(sessionID string, final bool)
}

// highlighted tells d.Highlighted, if set.
func (d Deps) highlighted(sessionID string, final bool) {
	if d.Highlighted != nil {
		d.Highlighted(sessionID, final)
	}
}

// Line is one completed turn as the UI shows it.
type Line struct {
	Seq             int
	StartMs         int64 // from the session start
	Lang            string
	Text            string
	Speaker         string // Scribe's label, "" when it gave none
	Translation     string
	TranslationLang string
	Translating     bool
	TranslateFailed bool
	Gap             bool // audio lost while reconnecting
}

// Snapshot is a copy of a session's state for the UI.
type Snapshot struct {
	SessionID string
	Config    Config // with the spoken language chosen last
	Status    Status
	StartedAt time.Time // zero until the session is live
	Lines     []Line
	Draft     string // the turn being recognized, not yet complete
	Err       error  // why the session failed or degraded, if it did
	// Highlights are the turns Highlights marked, by Seq, dismissed ones
	// too; HighlightsErr why it stopped for the session, if it did.
	Highlights    []store.Highlight
	HighlightsErr error
}

// Run is a running session. Its methods are safe from any goroutine.
type Run struct {
	deps     Deps
	cfg      Config // as the session started; Snapshot.Config follows SetSpoken, SetTarget and SetTranslate
	src      audio.Source
	onChange func()
	log      *slog.Logger

	captureCtx    context.Context
	stopCapture   context.CancelFunc
	translateCtx  context.Context
	stopTranslate context.CancelFunc
	translations  sync.WaitGroup
	sem           chan struct{}
	done          chan struct{}
	switched      chan struct{} // signals SetSpoken to the run goroutine

	id      string
	started time.Time
	// offsetMs is where the current connection's audio clock starts in the
	// session: Scribe counts from 0 on each connection. Run goroutine only.
	offsetMs int64
	// sentBytes is the audio sent on the current connection. Run goroutine only.
	sentBytes int
	// connLang is the spoken language of the current connection, lang.Auto
	// when it detects. Run goroutine only, once started.
	connLang string
	// savedTarget is the session's LangB in the store. Run goroutine only.
	savedTarget string
	mu          sync.Mutex
	snap        Snapshot
	drafts      []draft // pending turns, oldest first
	stopOnce    sync.Once
	hl          *highlighter
}

// draft is the latest recognition of a turn not yet completed.
type draft struct{ itemID, text string }

// Start starts capturing from src, connects to Scribe Live and creates the
// session in the store. Capture starts first, so that permission prompts come
// before a connection idles: if ctx ends while one is open, Start fails.
// onChange is called, from any goroutine, after every change of the Snapshot.
func Start(ctx context.Context, deps Deps, cfg Config, src audio.Source, onChange func()) (*Run, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r := &Run{
		deps:     deps,
		cfg:      cfg,
		src:      src,
		onChange: onChange,
		log:      log,
		sem:      make(chan struct{}, maxTranslations),
		done:     make(chan struct{}),
		switched: make(chan struct{}, 1),
		id:       uuid.NewString(),

		savedTarget: cfg.LangB,
	}
	r.snap = Snapshot{SessionID: r.id, Config: cfg, Status: StatusConnecting}
	if r.onChange == nil {
		r.onChange = func() {}
	}
	r.captureCtx, r.stopCapture = context.WithCancel(context.Background())
	r.translateCtx, r.stopTranslate = context.WithCancel(context.Background())
	fail := func(err error) (*Run, error) {
		r.stopCapture()
		r.stopTranslate()
		return nil, err
	}

	abandon := context.AfterFunc(ctx, r.stopCapture)
	frames, err := src.Start(r.captureCtx)
	abandon()
	if err != nil {
		return fail(err)
	}
	live, err := r.dial(ctx)
	if err != nil {
		return fail(fmt.Errorf("session: connect to Scribe: %w", err))
	}
	r.started = time.Now()
	if err := deps.Store.CreateSession(ctx, store.Session{
		ID: r.id, Mode: store.ModeCaptions, Source: cfg.Source,
		LangA: cfg.LangA, LangB: cfg.LangB, StartedAt: r.started,
	}); err != nil {
		_ = live.Close()
		return fail(fmt.Errorf("session: save session: %w", err))
	}
	r.snap.Status, r.snap.StartedAt = StatusLive, r.started
	log.Info("session started", "session", r.id, "source", cfg.Source)
	r.hl = newHighlighter(r, cfg.Highlights, r.started)
	go r.hl.loop()
	go r.run(frames, live)
	return r, nil
}

// Snapshot returns a copy of the current state.
func (r *Run) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.snap
	s.Lines = append([]Line(nil), r.snap.Lines...)
	s.Highlights = append([]store.Highlight(nil), r.snap.Highlights...)
	return s
}

// SetSpoken changes the spoken language while the session captures: the
// turn being spoken completes in the language before, and the next ones are
// recognized in code. The translation stays. Once capture has stopped, it
// does nothing.
func (r *Run) SetSpoken(code string) error {
	if !lang.IsSpoken(code) {
		return fmt.Errorf("session: %q cannot be transcribed", code)
	}
	if r.captureCtx.Err() != nil {
		return nil
	}
	r.update(func(s *Snapshot) { s.Config.LangA = code })
	select {
	case r.switched <- struct{}{}:
	default: // one is pending: it reads the latest language
	}
	return nil
}

// SetTranslate turns translation on or off for the turns completed from
// now on. Turns completed before keep theirs, and translations under way
// complete.
func (r *Run) SetTranslate(on bool) {
	r.update(func(s *Snapshot) { s.Config.Translate = on })
}

// SetTarget changes the language translations are in for the turns
// completed from now on, as SetTranslate does. The session's saved target
// follows once a turn is translated into it. Once capture has stopped, it
// does nothing.
func (r *Run) SetTarget(code string) error {
	if !lang.IsTarget(code) {
		return fmt.Errorf("session: %q cannot be translated to", code)
	}
	if r.captureCtx.Err() != nil {
		return nil
	}
	r.update(func(s *Snapshot) { s.Config.LangB = code })
	return nil
}

// SetHighlights changes what Highlights marks from the next check: how
// often it checks, the kinds, how sure, who the user is, or whether it
// marks at all. Turned on, it marks the turns from then on.
func (r *Run) SetHighlights(cfg HighlightConfig) {
	r.update(func(s *Snapshot) { s.Config.Highlights = cfg })
	r.hl.set(cfg)
}

// Dismiss marks highlight id not important: it is no longer shown, and
// its turn is not marked again.
func (r *Run) Dismiss(ctx context.Context, id string) error {
	if err := r.deps.Store.DismissHighlight(ctx, id); err != nil {
		return err
	}
	r.update(func(s *Snapshot) {
		for i := range s.Highlights {
			if s.Highlights[i].ID == id {
				s.Highlights[i].Dismissed = true
			}
		}
	})
	return nil
}

// MergePolish shows the polish an agent saved on the session's
// highlights since: each highlight shown takes its polish from the store,
// and a debate not shown yet is added. It merges, rather than reloads, so
// that a check saving highlights meanwhile loses none.
func (r *Run) MergePolish(ctx context.Context) error {
	d, err := r.deps.Store.GetSession(ctx, r.id)
	if err != nil {
		return err
	}
	saved := map[string]store.Highlight{}
	for _, h := range d.Highlights {
		saved[h.ID] = h
	}
	r.update(func(s *Snapshot) {
		for i, h := range s.Highlights {
			if got, ok := saved[h.ID]; ok {
				s.Highlights[i].Polish, s.Highlights[i].PolishedAt = got.Polish, got.PolishedAt
				delete(saved, h.ID)
			}
		}
		for _, h := range saved {
			if h.Kind == store.KindDebate {
				s.Highlights = append(s.Highlights, h)
			}
		}
		slices.SortFunc(s.Highlights, func(a, b store.Highlight) int { return cmp.Compare(a.Seq, b.Seq) })
	})
	return nil
}

// spoken is the spoken language chosen last.
func (r *Run) spoken() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snap.Config.LangA
}

// dial opens a connection in the spoken language chosen last.
func (r *Run) dial(ctx context.Context) (*zoomai.LiveSession, error) {
	code := r.spoken()
	cfg := zoomai.LiveConfig{Language: code}
	if code == lang.Auto {
		cfg = zoomai.LiveConfig{}
		for _, l := range lang.Detectable() {
			cfg.Detect = append(cfg.Detect, l.Code)
		}
	}
	live, err := r.deps.AI.OpenLive(ctx, cfg)
	if err == nil {
		r.connLang = code
	}
	return live, err
}

// Stop ends capture. The session then flushes, waits for translations and
// summarizes in the background; Done is closed when it has.
func (r *Run) Stop() {
	r.stopOnce.Do(r.stopCapture)
}

// Done is closed once the session is saved and summarized (or failed).
func (r *Run) Done() <-chan struct{} { return r.done }

func (r *Run) update(fn func(s *Snapshot)) {
	r.mu.Lock()
	fn(&r.snap)
	r.mu.Unlock()
	r.onChange()
}

// run moves frames to Scribe and events to the transcript until capture
// stops, then finishes the session.
func (r *Run) run(frames <-chan []byte, live *zoomai.LiveSession) {
	defer close(r.done)
	stopHeartbeat := r.heartbeat()
	captureErr := r.stream(frames, live)
	// Capture may have ended on its own (a source error, a fatal service
	// error): release the source either way.
	r.stopCapture()

	r.update(func(s *Snapshot) { s.Status = StatusFinishing })
	r.waitTranslations()
	// The last check runs beside the recap, its turns translated.
	checked := make(chan struct{})
	go func() {
		r.hl.finish()
		r.deps.highlighted(r.id, true)
		close(checked)
	}()

	stopHeartbeat() // an ended session has none
	ctx := context.Background()
	if err := r.deps.Store.EndSession(ctx, r.id, time.Now()); err != nil {
		r.log.Error("end session", "session", r.id, "err", err)
	}
	r.update(func(s *Snapshot) {
		s.Status = StatusSummarizing
		if captureErr != nil && s.Err == nil {
			s.Err = captureErr
		}
	})
	err := Recap(ctx, r.deps, r.id)
	<-checked
	r.update(func(s *Snapshot) {
		if err != nil {
			s.Status = StatusFailed
			if s.Err == nil {
				s.Err = err
			}
			return
		}
		s.Status = StatusDone
	})
}

// heartbeatEvery is how often a recording session tells the library it
// still records, by which a live session is told from a crashed one.
var heartbeatEvery = 10 * time.Second

// heartbeat tells the library now and every heartbeatEvery that the
// session still records, until stop returns.
func (r *Run) heartbeat() (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(heartbeatEvery)
		defer tick.Stop()
		for {
			if err := r.deps.Store.Heartbeat(ctx, r.id, time.Now()); err != nil && ctx.Err() == nil {
				r.log.Warn("heartbeat", "session", r.id, "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// pumpEnd is why pump returned.
type pumpEnd int

const (
	endCapture pumpEnd = iota // capture ended: stopped, or a source or fatal service error
	endLost                   // the connection dropped while capturing
	endSwitch                 // the spoken language changed
)

// stream runs until capture ends. It returns why the session ended early,
// or nil when the user stopped it.
func (r *Run) stream(frames <-chan []byte, live *zoomai.LiveSession) error {
	attempts := 0
	connected := time.Now()
	for {
		why, err := r.pump(frames, live)
		var next *zoomai.LiveSession
		var wait time.Duration // the server's Retry-After, when it gave one
		switch why {
		case endCapture:
			return err
		case endSwitch:
			// The turn in progress completes in its language; the audio
			// captured meanwhile waits for the new connection.
			if err := r.finish(live, nil); err != nil {
				r.drainFrames(frames)
				return err
			}
			ctx, cancel := context.WithTimeout(r.captureCtx, dialTimeout)
			next, err = r.dial(ctx)
			cancel()
			var limited *zoomai.RateLimitedError
			switch {
			case err == nil && !overflowed(frames):
				// No audio was lost: the new connection's clock starts
				// where the audio sent on the one before ends.
				r.offsetMs += int64(r.sentBytes / bytesPerMs)
				r.sentBytes = 0
				live, connected = next, time.Now()
				r.update(func(s *Snapshot) { s.Status = StatusLive })
				continue
			case err == nil:
				// The capture buffer filled while switching, so audio was
				// dropped: mark the gap, and time the turns from now.
				r.addGap()
				live, connected = next, time.Now()
				r.offsetMs = max(connected.Sub(r.started).Milliseconds()-int64(len(frames))*frameMs, 0)
				r.sentBytes = 0
				continue
			case r.captureCtx.Err() != nil:
				r.drainFrames(frames)
				return nil
			case errors.Is(err, zoomai.ErrUnauthorized):
				r.drainFrames(frames)
				return fmt.Errorf("session: reconnect to Scribe: %w", err)
			case errors.As(err, &limited):
				wait = limited.RetryAfter
				fallthrough
			default:
				r.log.Warn("switch language", "err", err)
			}
		}
		if next == nil {
			// The connection dropped while capturing, or none opened in the
			// new language: mark the gap and reconnect. A connection that
			// lasted resets the attempts, so a long session survives
			// occasional drops.
			if time.Since(connected) > stableConnection {
				attempts = 0
			}
			r.update(func(s *Snapshot) { s.Status = StatusReconnecting })
			r.clearDrafts()
			r.addGap()
			next, err = r.reconnect(&attempts, wait)
			if err != nil || next == nil { // next is nil when stopped meanwhile
				r.drainFrames(frames)
				return err
			}
		}
		live, connected = next, time.Now()
		// The audio captured while reconnecting goes first: the new
		// connection's clock starts that much before now.
		r.offsetMs = max(connected.Sub(r.started).Milliseconds()-int64(len(frames))*frameMs, 0)
		r.sentBytes = 0
		r.update(func(s *Snapshot) { s.Status = StatusLive })
	}
}

// pump forwards frames to live and applies its events until capture ends
// (stop, or a source or fatal service error, returned as err), the
// connection drops while capture continues, or the spoken language changes.
func (r *Run) pump(frames <-chan []byte, live *zoomai.LiveSession) (pumpEnd, error) {
	events := live.Events()
	for {
		select {
		case <-r.switched:
			if r.spoken() != r.connLang {
				return endSwitch, nil
			}
		case frame, ok := <-frames:
			if !ok {
				return endCapture, r.finish(live, r.src.Err())
			}
			if r.captureCtx.Err() != nil {
				// Stopping: drop the tail. Sending it on the canceled capture
				// context would close the connection before the flush.
				continue
			}
			sendCtx, cancel := context.WithTimeout(context.Background(), sendTimeout)
			err := live.Send(sendCtx, frame)
			cancel()
			if err == nil {
				r.sentBytes += len(frame)
			}
			if err != nil {
				_ = live.Close()
				if fatal := r.drainEvents(events); fatal != nil {
					r.drainFrames(frames)
					return endCapture, fatal
				}
				return endLost, nil
			}
		case ev, ok := <-events:
			if !ok {
				return endLost, nil
			}
			if fatal := r.handle(ev); fatal != nil {
				r.drainFrames(frames)
				_ = live.Close()
				return endCapture, fatal
			}
		}
	}
}

// finish flushes the live session after the last frame and applies the
// events that arrive before it closes.
func (r *Run) finish(live *zoomai.LiveSession, captureErr error) error {
	ctx, cancel := context.WithTimeout(context.Background(), finishTimeout)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- live.Finish(ctx) }()
	for ev := range live.Events() {
		if fatal := r.handle(ev); fatal != nil && captureErr == nil {
			captureErr = fatal
		}
	}
	if err := <-finished; err != nil {
		r.log.Warn("finish live session", "err", err)
	}
	_ = live.Close()
	r.clearDrafts()
	return captureErr
}

// reconnect dials Scribe again, with exponential backoff and at least
// wait, the server's last Retry-After, until it succeeds or *attempts
// reaches maxReconnects. It returns nil, nil when capture stopped meanwhile.
func (r *Run) reconnect(attempts *int, wait time.Duration) (*zoomai.LiveSession, error) {
	for *attempts < maxReconnects {
		*attempts++
		select {
		case <-time.After(max(time.Second<<(*attempts-1), wait)):
		case <-r.captureCtx.Done():
			return nil, nil
		}
		ctx, cancel := context.WithTimeout(r.captureCtx, dialTimeout)
		live, err := r.dial(ctx)
		cancel()
		switch {
		case err == nil:
			return live, nil
		case r.captureCtx.Err() != nil:
			return nil, nil
		case errors.Is(err, zoomai.ErrUnauthorized):
			return nil, fmt.Errorf("session: reconnect to Scribe: %w", err)
		}
		var limited *zoomai.RateLimitedError
		if errors.As(err, &limited) {
			wait = limited.RetryAfter
		}
		r.log.Warn("reconnect", "attempt", *attempts, "err", err)
	}
	return nil, errors.New("session: lost the connection to Scribe")
}

// overflowed reports whether a buffered capture channel is full: frames
// captured beyond it were dropped.
func overflowed(frames <-chan []byte) bool {
	return cap(frames) > 0 && len(frames) == cap(frames)
}

func (r *Run) drainFrames(frames <-chan []byte) {
	r.stopCapture()
	for range frames {
	}
}

// drainEvents applies the events left on a closed connection and returns
// the first fatal error among them: Scribe ended the session, so it must
// not be reconnected.
func (r *Run) drainEvents(events <-chan zoomai.LiveEvent) error {
	var fatal error
	for ev := range events {
		if err := r.handle(ev); err != nil && fatal == nil {
			fatal = err
		}
	}
	return fatal
}

// clearDrafts forgets the turns a lost or finished connection will never
// complete, so they do not come back as the current draft.
func (r *Run) clearDrafts() {
	r.mu.Lock()
	r.drafts = nil
	r.mu.Unlock()
	r.publishDraft()
}

// handle applies one live event. It returns an error for a fatal one.
func (r *Run) handle(ev zoomai.LiveEvent) error {
	switch ev := ev.(type) {
	case zoomai.Draft:
		r.mu.Lock()
		r.drafts = append(slices.DeleteFunc(r.drafts, func(d draft) bool { return d.itemID == ev.ItemID }),
			draft{itemID: ev.ItemID, text: ev.Text})
		if len(r.drafts) > maxDrafts {
			r.drafts = slices.Delete(r.drafts, 0, len(r.drafts)-maxDrafts)
		}
		r.mu.Unlock()
		r.publishDraft()
	case zoomai.Final:
		r.mu.Lock()
		r.drafts = slices.DeleteFunc(r.drafts, func(d draft) bool { return d.itemID == ev.ItemID })
		r.mu.Unlock()
		r.publishDraft()
		if ev.Text != "" {
			r.addTurn(ev)
		}
	case zoomai.ServerError:
		r.log.Warn("scribe error", "code", ev.Code, "fatal", ev.Fatal)
		if ev.Fatal {
			return fmt.Errorf("session: Scribe ended the session (%s)", ev.Code)
		}
	case zoomai.Closed:
		r.log.Info("scribe closed", "reason", ev.Reason)
	}
	return nil
}

// publishDraft shows the most recently updated draft.
func (r *Run) publishDraft() {
	r.mu.Lock()
	var text string
	if len(r.drafts) > 0 {
		text = r.drafts[len(r.drafts)-1].text
	}
	r.mu.Unlock()
	r.update(func(s *Snapshot) { s.Draft = text })
}

func (r *Run) addTurn(ev zoomai.Final) {
	r.mu.Lock()
	seq := len(r.snap.Lines)
	translate, target := r.snap.Config.Translate, r.snap.Config.LangB
	r.mu.Unlock()
	from := r.turnLang(ev.Language)
	to := ""
	if translate {
		to = TranslationTarget(from, target)
	}
	if to != "" && to != r.savedTarget {
		if err := r.deps.Store.SetTarget(context.Background(), r.id, to); err != nil {
			r.log.Error("set target", "seq", seq, "err", err)
		} else {
			r.savedTarget = to
		}
	}

	start := r.offsetMs + ev.StartMs
	seg := store.Segment{
		ID: uuid.NewString(), SessionID: r.id, Seq: seq, Lang: from, Text: ev.Text, Speaker: ev.Speaker,
		StartMs: start, EndMs: r.offsetMs + max(ev.EndMs, ev.StartMs),
		TranslationLang: to, // its translation is awaited
	}
	if err := r.deps.Store.AppendSegment(context.Background(), seg); err != nil {
		r.log.Error("append segment", "seq", seq, "err", err)
	}
	line := Line{
		Seq: seq, StartMs: start, Lang: from, Text: ev.Text, Speaker: ev.Speaker,
		TranslationLang: to, Translating: to != "",
	}
	r.update(func(s *Snapshot) { s.Lines = append(s.Lines, line) })
	r.hl.added(line)
	if to == "" {
		return
	}
	r.translations.Add(1)
	go r.translate(seg, to)
}

// turnLang is the language of a turn Scribe completed: the connection's,
// else the catalog's code of the one it detected, else Undetermined.
func (r *Run) turnLang(detected string) string {
	if r.connLang != lang.Auto {
		return r.connLang
	}
	if code, ok := lang.FromDetected(detected); ok {
		return code
	}
	return lang.Undetermined
}

func (r *Run) addGap() {
	r.mu.Lock()
	seq := len(r.snap.Lines)
	r.mu.Unlock()
	seg := store.Segment{ID: uuid.NewString(), SessionID: r.id, Seq: seq, Lang: store.GapLang}
	if err := r.deps.Store.AppendSegment(context.Background(), seg); err != nil {
		r.log.Error("append gap", "seq", seq, "err", err)
	}
	r.update(func(s *Snapshot) { s.Lines = append(s.Lines, Line{Seq: seq, Gap: true}) })
}

func (r *Run) translate(seg store.Segment, to string) {
	defer r.translations.Done()
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-r.translateCtx.Done():
		r.setTranslation(seg.Seq, "", errors.New("canceled"))
		return
	}
	text, err := TranslateText(r.translateCtx, r.deps.AI, seg.Text, seg.Lang, to)
	if err == nil {
		err = r.deps.Store.SetTranslation(context.Background(), seg.ID, to, text)
	}
	if err != nil {
		r.log.Warn("translate", "seq", seg.Seq, "err", err)
	}
	r.setTranslation(seg.Seq, text, err)
}

func (r *Run) setTranslation(seq int, text string, err error) {
	r.update(func(s *Snapshot) {
		for i := range s.Lines {
			if s.Lines[i].Seq == seq {
				s.Lines[i].Translating = false
				s.Lines[i].Translation = text
				s.Lines[i].TranslateFailed = err != nil
				if errors.Is(err, zoomai.ErrUnauthorized) && s.Err == nil {
					s.Err = err
				}
			}
		}
	})
}

// waitTranslations waits for in-flight translations, then cancels the rest.
func (r *Run) waitTranslations() {
	done := make(chan struct{})
	go func() {
		r.translations.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(translationTimeout):
		r.stopTranslate()
		<-done
	}
	r.stopTranslate()
}

// TranslateText translates text, through English when neither language is.
func TranslateText(ctx context.Context, ai *zoomai.Client, text, from, to string) (string, error) {
	for _, step := range lang.TranslationSteps(from, to) {
		out, err := ai.Translate(ctx, text, step[0], step[1])
		if err != nil {
			return "", err
		}
		text = out
	}
	return text, nil
}
