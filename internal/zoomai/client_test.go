package zoomai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient("secret-key", WithBaseURL(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTranslateRequestAndResponse(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotPath, gotMethod string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotMethod = r.Header.Get("x-api-key")+"|"+r.Header.Get("Authorization"), r.URL.Path, r.Method
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		io.WriteString(w, `{"request_id":"r","result":{"translations":{"ja-JP":"こんにちは"}}}`)
	})
	got, err := c.Translate(context.Background(), "hello", "en-US", "ja-JP")
	if err != nil || got != "こんにちは" {
		t.Fatalf("Translate = %q, %v", got, err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v2/aiservices/translator/translate" || gotAuth != "secret-key|" {
		t.Errorf("request = %s %s auth=%q", gotMethod, gotPath, gotAuth)
	}
	want := `{"config":{"source_language":"en-US","target_languages":["ja-JP"]},"text":"hello"}`
	b, _ := json.Marshal(gotBody)
	if string(b) != want {
		t.Errorf("body = %s, want %s", b, want)
	}
}

func TestTranslateMissingTargetInResponse(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"result":{"translations":{"fr-FR":"bonjour"}}}`)
	})
	if _, err := c.Translate(context.Background(), "hello", "en-US", "ja-JP"); err == nil {
		t.Error("want error when response lacks the requested target")
	}
}

func TestSummarizeRequestAndResponse(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		io.WriteString(w, `{"request_id":"r","task":"recap","result":{"recap":"a recap"},"model":"m"}`)
	})
	got, err := c.Summarize(context.Background(), "A: hi\nB: yo", TaskRecap, "en-US")
	if err != nil || got != "a recap" {
		t.Fatalf("Summarize = %q, %v", got, err)
	}
	if gotPath != "/v2/aiservices/summarizer/summarize" {
		t.Errorf("path = %s", gotPath)
	}
	want := `{"config":{"language":"en-US","summary_type":"conversation","task":"recap"},"input":{"text":"A: hi\nB: yo"}}`
	b, _ := json.Marshal(gotBody)
	if string(b) != want {
		t.Errorf("body = %s, want %s", b, want)
	}
}

func TestInputValidationNeverHitsServer(t *testing.T) {
	var hits atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1) })
	ctx := context.Background()

	tests := []struct {
		name    string
		call    func() error
		tooLong bool
	}{
		{"translate 4001 runes", func() error {
			_, err := c.Translate(ctx, strings.Repeat("あ", 4001), "ja-JP", "en-US")
			return err
		}, true},
		{"summarize over 96KB", func() error {
			_, err := c.Summarize(ctx, strings.Repeat("a", 96*1024+1), TaskSummary, "en-US")
			return err
		}, true},
		{"translate empty text", func() error { _, err := c.Translate(ctx, "  ", "en-US", "ja-JP"); return err }, false},
		{"translate empty target", func() error { _, err := c.Translate(ctx, "hi", "en-US", ""); return err }, false},
		{"summarize empty language", func() error { _, err := c.Summarize(ctx, "hi", TaskSummary, ""); return err }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if err == nil {
				t.Fatal("want error")
			}
			if errors.Is(err, ErrTextTooLong) != tt.tooLong {
				t.Errorf("err = %v, tooLong want %v", err, tt.tooLong)
			}
		})
	}
	if hits.Load() != 0 {
		t.Errorf("server hit %d times", hits.Load())
	}
}

func TestSummarizeReadsTheRequestedTasksText(t *testing.T) {
	// A full_summary response, as the Summarizer API sends it.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"task":"full_summary","result":{"recap":"Short recap.","summary_text":"Main summary.","action_items":[{"id":"1","description":"Follow up."}],"full_summary":"# Notes\n- Ship it"}}`)
	})
	got, err := c.Summarize(context.Background(), "hi", TaskFullSummary, "en-US")
	if err != nil || got != "# Notes\n- Ship it" {
		t.Fatalf("Summarize = %q, %v; want the full_summary", got, err)
	}
	got, err = c.Summarize(context.Background(), "hi", TaskSummary, "en-US")
	if err != nil || got != "Main summary." {
		t.Fatalf("Summarize = %q, %v; want the summary_text", got, err)
	}
}

func TestSummarizeWithoutTextFails(t *testing.T) {
	// An empty recap must not pass for a session where nothing was said.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"task":"full_summary","result":{"full_summary":"  "}}`)
	})
	if got, err := c.Summarize(context.Background(), "hi", TaskFullSummary, "en-US"); err == nil {
		t.Fatalf("Summarize = %q, nil; want an error", got)
	}
}

func TestLengthLimitsAreInclusive(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"result":{"translations":{"en-US":"ok"},"summary_text":"ok"}}`)
	})
	if _, err := c.Translate(context.Background(), strings.Repeat("あ", 4000), "ja-JP", "en-US"); err != nil {
		t.Errorf("4000 runes rejected: %v", err)
	}
	if _, err := c.Summarize(context.Background(), strings.Repeat("a", 96*1024), TaskSummary, "en-US"); err != nil {
		t.Errorf("96KB rejected: %v", err)
	}
}

func TestHTTPErrorMapping(t *testing.T) {
	const secretText = "TOP-SECRET-TRANSCRIPT"
	tests := []struct {
		name   string
		status int
		body   string
		check  func(t *testing.T, err error)
	}{
		{"401", 401, `{"message":"bad key"}`, func(t *testing.T, err error) {
			if !errors.Is(err, ErrUnauthorized) {
				t.Errorf("err = %v", err)
			}
		}},
		{"403", 403, ``, func(t *testing.T, err error) {
			if !errors.Is(err, ErrUnauthorized) {
				t.Errorf("err = %v", err)
			}
		}},
		{"400 with message", 400, `{"code":300,"message":"unsupported language"}`, func(t *testing.T, err error) {
			var ae *APIError
			if !errors.As(err, &ae) || ae.Status != 400 || ae.Message != "unsupported language" {
				t.Errorf("err = %v", err)
			}
		}},
		{"500 non-JSON body not echoed", 500, "<html>" + secretText + "</html>", func(t *testing.T, err error) {
			var ae *APIError
			if !errors.As(err, &ae) || ae.Status != 500 || ae.Message != "" {
				t.Errorf("err = %v", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hits atomic.Int32
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			})
			_, err := c.Translate(context.Background(), secretText, "en-US", "ja-JP")
			tt.check(t, err)
			if err != nil && (strings.Contains(err.Error(), secretText) || strings.Contains(err.Error(), "secret-key")) {
				t.Errorf("error leaks request text or key: %v", err)
			}
			if hits.Load() != 1 {
				t.Errorf("attempts = %d, want 1 (no retry)", hits.Load())
			}
		})
	}
}

func TestRateLimitRetriesThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 3 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, `{"result":{"summary_text":"done"}}`)
	})
	got, err := c.Summarize(context.Background(), "hi", TaskSummary, "en-US")
	if err != nil || got != "done" || hits.Load() != 3 {
		t.Errorf("got %q, %v after %d attempts", got, err, hits.Load())
	}
}

func TestRateLimitExhausted(t *testing.T) {
	var hits atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := c.Translate(context.Background(), "hi", "en-US", "ja-JP")
	var rl *RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *RateLimitedError", err)
	}
	if hits.Load() != 3 {
		t.Errorf("attempts = %d, want 3", hits.Load())
	}
}

func TestRateLimitBackoffHonorsContext(t *testing.T) {
	var hits atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "20")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Translate(ctx, "hi", "en-US", "ja-JP")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 3*time.Second || hits.Load() != 1 {
		t.Errorf("returned after %s with %d attempts", time.Since(start), hits.Load())
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"", time.Second},
		{"garbage", time.Second},
		{"0", 0},
		{"7", 7 * time.Second},
		{"-3", 0},
		{"600", 30 * time.Second},
		{now.Add(5 * time.Second).Format(http.TimeFormat), 5 * time.Second},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{now.Add(time.Hour).Format(http.TimeFormat), 30 * time.Second},
	}
	for _, tt := range tests {
		if got := parseRetryAfter(tt.in, now); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestWithBaseURL(t *testing.T) {
	tests := []struct {
		raw string
		ok  bool
	}{
		{"https://api.zoom.us", true},
		{"https://example.com/proxy", true},
		{"http://127.0.0.1:8080", true},
		{"http://localhost:8080", true},
		{"http://[::1]:8080", true},
		{"http://api.zoom.us", false},
		{"http://127.0.0.1.evil.com", false},
		{"ftp://localhost", false},
		{"api.zoom.us", false},
		{"", false},
	}
	for _, tt := range tests {
		_, err := NewClient("k", WithBaseURL(tt.raw))
		if (err == nil) != tt.ok {
			t.Errorf("WithBaseURL(%q) err = %v, want ok=%v", tt.raw, err, tt.ok)
		}
	}
}

func TestNewClientRejectsEmptyKey(t *testing.T) {
	if _, err := NewClient("  "); err == nil {
		t.Error("want error for empty key")
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var followed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed.Store(true)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	for name, opts := range map[string][]Option{
		"default client":  {WithBaseURL(srv.URL)},
		"caller's client": {WithBaseURL(srv.URL), WithHTTPClient(&http.Client{})},
	} {
		c, err := NewClient("key", opts...)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Translate(context.Background(), "private words", "en-US", "ja-JP")
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusTemporaryRedirect {
			t.Errorf("%s: err %v, want the redirect as an APIError", name, err)
		}
		if followed.Load() {
			t.Fatalf("%s: the redirect was followed, resending the key and the text", name)
		}
	}
}
