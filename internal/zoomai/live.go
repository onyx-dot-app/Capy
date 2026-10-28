package zoomai

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	eventBuffer     = 64
	maxEventBytes   = 128 * 1024
	maxItemIDLen    = 256
	maxLanguageLen  = 35 // the longest BCP-47 tag Capy keeps
	maxSpeakerLen   = 64
	maxDraftLen     = 20_000
	maxDrafts       = 32
	maxCompletedIDs = 4096
	closeGrace      = time.Second
)

// Keepalive: a ping unanswered for pingTimeout means the connection died
// silently (Wi-Fi change, sleep), which reads alone would never notice.
// Variables so tests can shorten them.
var (
	pingInterval = 15 * time.Second
	pingTimeout  = 10 * time.Second
)

// ErrSessionClosed is returned by Send after the session has ended.
var ErrSessionClosed = errors.New("zoomai: live session closed")

// LiveEvent is one event from a live session: Draft, Final, Closed or
// ServerError.
type LiveEvent interface{ isLiveEvent() }

// Draft is an interim transcript. Text is the full revised snapshot for
// ItemID and replaces any earlier draft for it.
type Draft struct{ ItemID, Text string }

// Final is a completed segment. Text may be empty; consumers use the event to
// clear the item's draft and decide whether to keep an empty segment.
// Language is the language Scribe detected, as it names it ("en", "zh"),
// and only when it detects one. Speaker is Scribe's label of who spoke
// ("Speaker 1"), numbered anew on each connection; either may be empty.
type Final struct {
	ItemID, Text      string
	StartMs, EndMs    int64
	Language, Speaker string
}

// Closed reports the server ended the session. It is the last event.
type Closed struct{ Reason string }

// ServerError is an error event from the server. After a fatal one the
// session ends.
type ServerError struct {
	Code, Message string
	Fatal         bool
}

func (Draft) isLiveEvent()       {}
func (Final) isLiveEvent()       {}
func (Closed) isLiveEvent()      {}
func (ServerError) isLiveEvent() {}

// LiveSession is an open Scribe Live stream. Send may run concurrently with
// event consumption, and Close/Finish are safe to call concurrently with Send.
type LiveSession struct {
	conn   *websocket.Conn
	events chan LiveEvent
	stop   chan struct{} // closed by Close; unblocks the reader
	done   chan struct{} // closed when the reader has exited
	once   sync.Once

	drafts    map[string]struct{} // item IDs with a live draft; reader goroutine only
	completed completedSet
}

// LiveConfig chooses what a live session recognizes. Scribe always tells
// speakers apart.
type LiveConfig struct {
	// Language is the BCP-47 code spoken. Empty, Scribe detects the
	// language of each turn among Detect instead.
	Language string
	// Detect are the BCP-47 codes detection chooses from, without Language.
	Detect []string
}

// OpenLive dials Scribe Live and sends the session.update. ctx bounds only
// the connection setup; the session lives until Close, Finish or a server end.
func (c *Client) OpenLive(ctx context.Context, cfg LiveConfig) (*LiveSession, error) {
	if cfg.Language == "" && len(cfg.Detect) == 0 {
		return nil, errors.New("zoomai: a language or languages to detect are required")
	}
	u := c.baseURL.JoinPath(livePath)
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	conn, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPClient: c.httpClient,
		HTTPHeader: http.Header{apiKeyHeader: {c.apiKey}},
	})
	if err != nil {
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusUnauthorized, http.StatusForbidden:
				return nil, ErrUnauthorized
			case http.StatusTooManyRequests:
				return nil, &RateLimitedError{RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
			}
			return nil, &APIError{Status: resp.StatusCode}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errors.New("zoomai: live connect failed")
	}
	conn.SetReadLimit(maxEventBytes)

	update, err := json.Marshal(newSessionUpdate(cfg))
	if err == nil {
		err = conn.Write(ctx, websocket.MessageText, update)
	}
	if err != nil {
		_ = conn.CloseNow()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, errors.New("zoomai: live session setup failed")
	}

	s := &LiveSession{
		conn:      conn,
		events:    make(chan LiveEvent, eventBuffer),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		drafts:    make(map[string]struct{}),
		completed: newCompletedSet(maxCompletedIDs),
	}
	go s.readLoop()
	go s.keepalive()
	return s, nil
}

// keepalive pings the server until the session ends, and drops the
// connection when a ping goes unanswered, which ends Events.
func (s *LiveSession) keepalive() {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-s.done:
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
		err := s.conn.Ping(ctx)
		cancel()
		if err != nil {
			_ = s.conn.CloseNow()
			return
		}
	}
}

type sessionUpdate struct {
	Type     string `json:"type"`
	Language string `json:"language,omitempty"`
	Audio    struct {
		Format string `json:"format"`
	} `json:"audio"`
	Config struct {
		Language          string             `json:"language,omitempty"`
		LanguageDetection *languageDetection `json:"language_detection,omitempty"`
		InterimResults    bool               `json:"interim_results"`
		Diarization       bool               `json:"diarization"`
	} `json:"config"`
	TurnDetection struct {
		SilenceDurationMs      int `json:"silence_duration_ms"`
		MaxSegmentDurationMs   int `json:"max_segment_duration_ms"`
		ForcePartialIntervalMs int `json:"force_partial_interval_ms"`
	} `json:"turn_detection"`
}

// languageDetection has Scribe detect each turn's language: "continuous"
// follows a conversation that changes language.
type languageDetection struct {
	Mode       string   `json:"mode"`
	Candidates []string `json:"candidate_languages"`
}

func newSessionUpdate(cfg LiveConfig) sessionUpdate {
	var m sessionUpdate
	m.Type = "session.update"
	m.Audio.Format = "pcm16"
	if cfg.Language != "" {
		m.Language = cfg.Language
		m.Config.Language = cfg.Language
	} else {
		m.Config.LanguageDetection = &languageDetection{Mode: "continuous", Candidates: cfg.Detect}
	}
	m.Config.InterimResults = true
	m.Config.Diarization = true
	m.TurnDetection.SilenceDurationMs = 800
	m.TurnDetection.MaxSegmentDurationMs = 30000
	m.TurnDetection.ForcePartialIntervalMs = 1000
	return m
}

// Send writes one binary PCM16 (16 kHz mono little-endian) frame.
func (s *LiveSession) Send(ctx context.Context, pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}
	if err := s.conn.Write(ctx, websocket.MessageBinary, pcm); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		select {
		case <-s.done:
			return ErrSessionClosed
		default:
		}
		return errors.New("zoomai: live send failed")
	}
	return nil
}

// Events returns the session's event stream. It is closed when the session
// ends. Consumers must keep draining it, including during Finish.
func (s *LiveSession) Events() <-chan LiveEvent { return s.events }

// Finish asks the server to end the session (the "session.close" message),
// waits for it to flush remaining transcripts and send session.closed, then
// closes the connection. If ctx ends first, the connection is closed anyway
// and ctx's error is returned.
func (s *LiveSession) Finish(ctx context.Context) error {
	defer s.Close()
	select {
	case <-s.done:
		return nil
	default:
	}
	if err := s.conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.close"}`)); err != nil {
		select {
		case <-s.done:
			return nil
		default:
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return errors.New("zoomai: live finish failed")
	}
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close ends the session immediately, discarding undelivered events. It is
// idempotent. The returned error is always nil: a failed close handshake
// leaves nothing for the caller to act on.
func (s *LiveSession) Close() error {
	s.once.Do(func() {
		close(s.stop)
		// Offer a graceful close, but do not let an unresponsive server hold
		// the caller for the library's full handshake timeout.
		graceful := make(chan struct{})
		go func() {
			defer close(graceful)
			_ = s.conn.Close(websocket.StatusNormalClosure, "")
		}()
		select {
		case <-graceful:
		case <-time.After(closeGrace):
			_ = s.conn.CloseNow()
		}
	})
	<-s.done
	return nil
}

// emit delivers ev unless the session is being closed.
func (s *LiveSession) emit(ev LiveEvent) bool {
	select {
	case s.events <- ev:
		return true
	case <-s.stop:
		return false
	}
}

func (s *LiveSession) readLoop() {
	defer close(s.done)
	defer close(s.events)
	defer func() { _ = s.conn.CloseNow() }()

	for {
		typ, data, err := s.conn.Read(context.Background())
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		if !s.handle(data) {
			return
		}
	}
}

// liveMessage is the union of server messages Capy consumes.
type liveMessage struct {
	Type       string   `json:"type"`
	ItemID     *string  `json:"item_id"`
	Delta      *string  `json:"delta"`
	Transcript *string  `json:"transcript"`
	StartMs    *float64 `json:"audio_start_ms"`
	EndMs      *float64 `json:"audio_end_ms"`
	Language   string   `json:"language"`
	Speaker    string   `json:"speaker"`
	Reason     string   `json:"reason"`
	Error      *struct {
		Code    flexString `json:"code"`
		Message string     `json:"message"`
		Fatal   bool       `json:"fatal"`
	} `json:"error"`
}

// flexString decodes a JSON string or number into a string.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexString(n.String())
	return nil
}

// handle processes one server message and reports whether the loop continues.
func (s *LiveSession) handle(data []byte) bool {
	var m liveMessage
	if json.Unmarshal(data, &m) != nil {
		return true
	}
	// Events without an ID share one draft ("").
	itemID := ""
	if m.ItemID != nil {
		itemID = *m.ItemID
		if itemID == "" || len(itemID) > maxItemIDLen {
			return true
		}
	}

	switch m.Type {
	case "transcription.delta":
		if m.Delta == nil || len(*m.Delta) > maxDraftLen || s.completed.has(itemID) {
			return true
		}
		if _, ok := s.drafts[itemID]; !ok && len(s.drafts) >= maxDrafts {
			// The table is full of items that never completed: they are
			// stale, and the new item is the one being spoken.
			clear(s.drafts)
		}
		s.drafts[itemID] = struct{}{}
		return s.emit(Draft{ItemID: itemID, Text: *m.Delta})
	case "transcription.completed":
		if m.Transcript == nil || len(*m.Transcript) > maxDraftLen || s.completed.has(itemID) {
			return true
		}
		delete(s.drafts, itemID)
		if itemID != "" {
			s.completed.add(itemID)
		}
		start := millis(m.StartMs, 0)
		end := max(millis(m.EndMs, start), start)
		return s.emit(Final{
			ItemID: itemID, Text: strings.TrimSpace(*m.Transcript), StartMs: start, EndMs: end,
			Language: bounded(m.Language, maxLanguageLen), Speaker: bounded(m.Speaker, maxSpeakerLen),
		})
	case "error":
		ev := ServerError{}
		if m.Error != nil {
			ev = ServerError{Code: string(m.Error.Code), Message: m.Error.Message, Fatal: m.Error.Fatal}
		}
		if !s.emit(ev) {
			return false
		}
		return !ev.Fatal
	case "session.closed":
		s.emit(Closed{Reason: m.Reason})
		return false
	}
	return true
}

// bounded is s trimmed, or "" when it is longer than limit bytes.
func bounded(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) > limit {
		return ""
	}
	return s
}

// millis converts a JSON number to non-negative whole milliseconds.
func millis(v *float64, fallback int64) int64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return fallback
	}
	return max(int64(math.Round(*v)), 0)
}

// completedSet remembers the most recent completed item IDs.
type completedSet struct {
	limit int
	set   map[string]struct{}
	order []string
}

func newCompletedSet(limit int) completedSet {
	return completedSet{limit: limit, set: make(map[string]struct{})}
}

func (c *completedSet) has(id string) bool {
	_, ok := c.set[id]
	return ok
}

func (c *completedSet) add(id string) {
	c.set[id] = struct{}{}
	c.order = append(c.order, id)
	if len(c.order) > c.limit {
		delete(c.set, c.order[0])
		c.order = c.order[1:]
	}
}
