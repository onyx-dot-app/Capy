package zoomai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const testTimeout = 5 * time.Second

// liveServer runs handler for each accepted WebSocket and returns a client
// pointed at it.
func liveServer(t *testing.T, handler func(ctx context.Context, conn *websocket.Conn, r *http.Request)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/aiservices/scribe/live" {
			http.NotFound(w, r)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		handler(r.Context(), conn, r)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient("test-key", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func openLive(t *testing.T, c *Client) *LiveSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	s, err := c.OpenLive(ctx, LiveConfig{Language: "en-US"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// collect drains events until the channel closes or the timeout elapses.
func collect(t *testing.T, s *LiveSession) []LiveEvent {
	t.Helper()
	var got []LiveEvent
	timeout := time.After(testTimeout)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-timeout:
			t.Fatalf("events channel not closed; got %v", got)
		}
	}
}

// holdOpen keeps a server handler alive while still servicing the peer's
// control frames (a handler that stops reading never answers a close).
func holdOpen(ctx context.Context, conn *websocket.Conn) {
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
	}
}

func writeJSON(t *testing.T, ctx context.Context, conn *websocket.Conn, v string) {
	t.Helper()
	if err := conn.Write(ctx, websocket.MessageText, []byte(v)); err != nil {
		t.Errorf("server write: %v", err)
	}
}

func TestOpenLiveHandshakeAndAudio(t *testing.T) {
	type seen struct {
		auth   string
		update string
		frame  []byte
		typ    websocket.MessageType
	}
	ch := make(chan seen, 1)
	c := liveServer(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		var s seen
		s.auth = r.Header.Get("x-api-key") + "|" + r.Header.Get("Authorization")
		_, b, err := conn.Read(ctx)
		if err != nil {
			return
		}
		s.update = string(b)
		s.typ, s.frame, _ = conn.Read(ctx)
		ch <- s
		holdOpen(ctx, conn)
	})
	sess := openLive(t, c)
	frame := []byte{0, 1, 2, 255, 254, 0}
	if err := sess.Send(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	var got seen
	select {
	case got = <-ch:
	case <-time.After(testTimeout):
		t.Fatal("server saw nothing")
	}
	if got.auth != "test-key|" { // the key, and no Bearer
		t.Errorf("x-api-key|Authorization = %q", got.auth)
	}
	const want = `{"type":"session.update","language":"en-US","audio":{"format":"pcm16"},` +
		`"config":{"language":"en-US","interim_results":true,"diarization":true},` +
		`"turn_detection":{"silence_duration_ms":800,"max_segment_duration_ms":30000,"force_partial_interval_ms":1000}}`
	if got.update != want {
		t.Errorf("session.update =\n%s\nwant\n%s", got.update, want)
	}
	if got.typ != websocket.MessageBinary || !reflect.DeepEqual(got.frame, frame) {
		t.Errorf("frame = %v %v, want binary %v", got.typ, got.frame, frame)
	}
}

// TestOpenLiveDetectsLanguages pins the session.update of detection: no
// language at all, as Scribe then transcribes everything in one.
func TestOpenLiveDetectsLanguages(t *testing.T) {
	updates := make(chan string, 1)
	c := liveServer(t, func(ctx context.Context, conn *websocket.Conn, r *http.Request) {
		if _, b, err := conn.Read(ctx); err == nil {
			updates <- string(b)
		}
		holdOpen(ctx, conn)
	})
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	s, err := c.OpenLive(ctx, LiveConfig{Detect: []string{"en-US", "zh-CN"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const want = `{"type":"session.update","audio":{"format":"pcm16"},` +
		`"config":{"language_detection":{"mode":"continuous","candidate_languages":["en-US","zh-CN"]},` +
		`"interim_results":true,"diarization":true},` +
		`"turn_detection":{"silence_duration_ms":800,"max_segment_duration_ms":30000,"force_partial_interval_ms":1000}}`
	select {
	case got := <-updates:
		if got != want {
			t.Errorf("session.update =\n%s\nwant\n%s", got, want)
		}
	case <-time.After(testTimeout):
		t.Fatal("server saw no session.update")
	}
}

func TestLiveEventMapping(t *testing.T) {
	tests := []struct {
		name string
		msgs []string
		want []LiveEvent
	}{
		{
			name: "delta snapshots replace, completion finalizes",
			msgs: []string{
				`{"type":"transcription.delta","item_id":"a","delta":"hel"}`,
				`{"type":"transcription.delta","item_id":"a","delta":"hello wor"}`,
				`{"type":"transcription.completed","item_id":"a","transcript":"  hello world ","audio_start_ms":100.4,"audio_end_ms":1900.6}`,
			},
			want: []LiveEvent{
				Draft{"a", "hel"}, Draft{"a", "hello wor"},
				Final{ItemID: "a", Text: "hello world", StartMs: 100, EndMs: 1901},
			},
		},
		{
			// As Scribe sends them with detection and diarization on.
			name: "detected language and speaker kept, oversized ones dropped",
			msgs: []string{
				`{"type":"transcription.completed","item_id":"a","transcript":"大家好","language":"zh","speaker":"Speaker 2"}`,
				`{"type":"transcription.completed","item_id":"b","transcript":"hi","language":"` + strings.Repeat("x", 36) +
					`","speaker":"` + strings.Repeat("s", 65) + `"}`,
			},
			want: []LiveEvent{
				Final{ItemID: "a", Text: "大家好", Language: "zh", Speaker: "Speaker 2"},
				Final{ItemID: "b", Text: "hi"},
			},
		},
		{
			name: "duplicate completion and late delta dropped",
			msgs: []string{
				`{"type":"transcription.completed","item_id":"a","transcript":"one"}`,
				`{"type":"transcription.delta","item_id":"a","delta":"late"}`,
				`{"type":"transcription.completed","item_id":"a","transcript":"one again"}`,
				`{"type":"transcription.delta","item_id":"b","delta":"two"}`,
			},
			want: []LiveEvent{Final{ItemID: "a", Text: "one"}, Draft{"b", "two"}},
		},
		{
			name: "empty completion still emitted",
			msgs: []string{
				`{"type":"transcription.delta","item_id":"a","delta":"um"}`,
				`{"type":"transcription.completed","item_id":"a","transcript":""}`,
			},
			want: []LiveEvent{Draft{"a", "um"}, Final{ItemID: "a"}},
		},
		{
			name: "events without item_id use empty ID and can repeat completion",
			msgs: []string{
				`{"type":"transcription.delta","delta":"x"}`,
				`{"type":"transcription.completed","transcript":"x done"}`,
				`{"type":"transcription.delta","delta":"y"}`,
				`{"type":"transcription.completed","transcript":"y done"}`,
			},
			want: []LiveEvent{
				Draft{"", "x"}, Final{Text: "x done"}, Draft{"", "y"}, Final{Text: "y done"},
			},
		},
		{
			name: "end offset never precedes start; negatives clamp",
			msgs: []string{
				`{"type":"transcription.completed","item_id":"a","transcript":"t","audio_start_ms":500,"audio_end_ms":100}`,
				`{"type":"transcription.completed","item_id":"b","transcript":"t","audio_start_ms":-5}`,
			},
			want: []LiveEvent{
				Final{ItemID: "a", Text: "t", StartMs: 500, EndMs: 500},
				Final{ItemID: "b", Text: "t"},
			},
		},
		{
			name: "malformed and unknown messages ignored",
			msgs: []string{
				`not json`,
				`{"type":"session.created","session_id":"s"}`,
				`{"type":"transcription.delta","item_id":"","delta":"bad id"}`,
				`{"type":"transcription.delta","item_id":"a"}`,
				`{"type":"transcription.delta","item_id":"ok","delta":"fine"}`,
			},
			want: []LiveEvent{Draft{"ok", "fine"}},
		},
		{
			name: "non-fatal error continues",
			msgs: []string{
				`{"type":"error","error":{"code":"bad_audio","message":"oops","fatal":false}}`,
				`{"type":"transcription.delta","item_id":"a","delta":"still here"}`,
			},
			want: []LiveEvent{ServerError{Code: "bad_audio", Message: "oops"}, Draft{"a", "still here"}},
		},
		{
			name: "fatal error ends stream; later messages unseen",
			msgs: []string{
				`{"type":"error","error":{"code":4001,"message":"boom","fatal":true}}`,
				`{"type":"transcription.delta","item_id":"a","delta":"never"}`,
			},
			want: []LiveEvent{ServerError{Code: "4001", Message: "boom", Fatal: true}},
		},
		{
			name: "session.closed is last",
			msgs: []string{
				`{"type":"session.closed","reason":"done"}`,
				`{"type":"transcription.delta","item_id":"a","delta":"never"}`,
			},
			want: []LiveEvent{Closed{Reason: "done"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := liveServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
				if _, _, err := conn.Read(ctx); err != nil { // session.update
					return
				}
				for _, m := range tt.msgs {
					writeJSON(t, ctx, conn, m)
				}
				// Hold the connection open: the client, not a server hangup,
				// must be what ends the stream.
				holdOpen(ctx, conn)
			})
			sess := openLive(t, c)
			var got []LiveEvent
			for range tt.want {
				select {
				case ev := <-sess.Events():
					got = append(got, ev)
				case <-time.After(testTimeout):
					t.Fatalf("timed out; got %v", got)
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("events = %#v\nwant     %#v", got, tt.want)
			}
			// Terminal cases must close the channel on their own; others
			// must not emit anything further.
			terminal := tt.want[len(tt.want)-1]
			switch ev := terminal.(type) {
			case Closed:
				expectClosed(t, sess)
			case ServerError:
				if ev.Fatal {
					expectClosed(t, sess)
				}
			}
		})
	}
}

func expectClosed(t *testing.T, s *LiveSession) {
	t.Helper()
	select {
	case ev, ok := <-s.Events():
		if ok {
			t.Fatalf("unexpected event %#v", ev)
		}
	case <-time.After(testTimeout):
		t.Fatal("events channel not closed")
	}
}

func TestFinishFlushesAndWaitsForSessionClosed(t *testing.T) {
	flush := make(chan string, 1)
	c := liveServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		_, b, err := conn.Read(ctx)
		if err != nil {
			return
		}
		flush <- string(b)
		// Tail transcript arrives only after the flush request.
		writeJSON(t, ctx, conn, `{"type":"transcription.completed","item_id":"z","transcript":"tail"}`)
		writeJSON(t, ctx, conn, `{"type":"session.closed","reason":"client_closed"}`)
		holdOpen(ctx, conn)
	})
	sess := openLive(t, c)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- sess.Finish(ctx) }()
	got := collect(t, sess)
	if err := <-finished; err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if msg := <-flush; msg != `{"type":"session.close"}` {
		t.Errorf("flush message = %s", msg)
	}
	want := []LiveEvent{Final{ItemID: "z", Text: "tail"}, Closed{Reason: "client_closed"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("events = %#v, want %#v", got, want)
	}
}

func TestFinishReturnsCtxErrorWhenServerNeverCloses(t *testing.T) {
	c := liveServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		holdOpen(ctx, conn)
	})
	sess := openLive(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := sess.Finish(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Finish = %v, want deadline exceeded", err)
	}
	expectClosed(t, sess) // connection was torn down anyway
}

func TestCloseIsIdempotentAndUnblocksUndrainedSession(t *testing.T) {
	c := liveServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		// Far more events than the buffer holds, with nobody reading.
		for i := 0; i < 4*eventBuffer; i++ {
			if conn.Write(ctx, websocket.MessageText, []byte(`{"type":"transcription.delta","delta":"x"}`)) != nil {
				return
			}
		}
		holdOpen(ctx, conn)
	})
	sess := openLive(t, c)
	time.Sleep(100 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		sess.Close()
		sess.Close()
	}()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("Close blocked on an undrained session")
	}
	if err := sess.Send(context.Background(), []byte{1, 2}); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("Send after Close = %v, want ErrSessionClosed", err)
	}
}

func TestServerHangupClosesEvents(t *testing.T) {
	c := liveServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		conn.Read(ctx)
		conn.CloseNow()
	})
	sess := openLive(t, c)
	expectClosed(t, sess)
}

func TestOpenLiveUpgradeRejected(t *testing.T) {
	tests := []struct {
		status int
		check  func(error) bool
	}{
		{http.StatusUnauthorized, func(err error) bool { return errors.Is(err, ErrUnauthorized) }},
		{http.StatusForbidden, func(err error) bool { return errors.Is(err, ErrUnauthorized) }},
		{http.StatusTooManyRequests, func(err error) bool {
			var rl *RateLimitedError
			return errors.As(err, &rl) && rl.RetryAfter == 2*time.Second
		}},
		{http.StatusBadGateway, func(err error) bool {
			var ae *APIError
			return errors.As(err, &ae) && ae.Status == http.StatusBadGateway
		}},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()
			c, err := NewClient("k", WithBaseURL(srv.URL))
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.OpenLive(context.Background(), LiveConfig{Language: "en-US"})
			if err == nil || !tt.check(err) {
				t.Errorf("OpenLive error = %v", err)
			}
		})
	}
}

func TestOpenLiveRejectsEmptyLanguage(t *testing.T) {
	c, _ := NewClient("k")
	if _, err := c.OpenLive(context.Background(), LiveConfig{}); err == nil {
		t.Error("want error for neither a language nor languages to detect")
	}
}

func TestCompletedSetEvictsOldest(t *testing.T) {
	s := newCompletedSet(3)
	for _, id := range []string{"a", "b", "c", "d"} {
		s.add(id)
	}
	if s.has("a") || !s.has("b") || !s.has("d") || len(s.set) != 3 {
		t.Errorf("set = %v", s.set)
	}
}

func shortKeepalive(t *testing.T) {
	interval, timeout := pingInterval, pingTimeout
	pingInterval, pingTimeout = 20*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { pingInterval, pingTimeout = interval, timeout })
}

func TestSilentlyDeadConnectionEndsEvents(t *testing.T) {
	shortKeepalive(t)
	c := liveServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		_, _, _ = conn.Read(ctx) // the session.update, then nothing: pings go unanswered
		<-ctx.Done()
	})
	s := openLive(t, c)
	select {
	case _, ok := <-s.Events():
		if ok {
			t.Fatal("unexpected event from a silent server")
		}
	case <-time.After(testTimeout):
		t.Fatal("a connection whose pings go unanswered was never dropped")
	}
}

func TestAnsweredPingsKeepTheSession(t *testing.T) {
	shortKeepalive(t)
	c := liveServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		for { // reading answers pings
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	})
	s := openLive(t, c)
	select {
	case <-s.Events():
		t.Fatal("a healthy connection was dropped")
	case <-time.After(300 * time.Millisecond): // several ping intervals
	}
}

func TestDraftsKeepFlowingAfterManyAbandonedItems(t *testing.T) {
	c := liveServer(t, func(ctx context.Context, conn *websocket.Conn, _ *http.Request) {
		_, _, _ = conn.Read(ctx)
		for i := range maxDrafts + 5 { // items that never complete
			_ = conn.Write(ctx, websocket.MessageText, fmt.Appendf(nil, `{"type":"transcription.delta","item_id":"old-%d","delta":"x"}`, i))
		}
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"transcription.delta","item_id":"now","delta":"current words"}`))
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.closed"}`))
	})
	var last Draft
	for _, ev := range collect(t, openLive(t, c)) {
		if d, ok := ev.(Draft); ok {
			last = d
		}
	}
	if last.ItemID != "now" || last.Text != "current words" {
		t.Fatalf("last draft %+v: drafts stopped after %d abandoned items", last, maxDrafts)
	}
}
