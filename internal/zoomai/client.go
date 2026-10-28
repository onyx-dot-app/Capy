// Package zoomai is a small client for the Zoom AI Services APIs Capy uses:
// Scribe Live (streaming transcription), Translator, Summarizer, Speech
// Summarization and Decisions.
//
// Requests authenticate with "x-api-key: <key>", the WebSocket upgrade too.
// The package never logs, and error strings never contain the API key or any request text.
package zoomai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultBaseURL = "https://api.zoom.us"

	translatePath = "/v2/aiservices/translator/translate"
	summarizePath = "/v2/aiservices/summarizer/summarize"
	livePath      = "/v2/aiservices/scribe/live"

	maxTranslateRunes = 4000
	maxSummarizeBytes = 96 * 1024

	maxAttempts       = 3
	defaultRetryAfter = time.Second
	maxRetryAfter     = 30 * time.Second

	// apiKeyHeader carries the API key on every request: Bearer is for a JWT.
	apiKeyHeader = "x-api-key"

	maxResponseBytes = 4 << 20
	maxErrorMessage  = 200
)

var (
	// ErrUnauthorized reports a 401/403 response, or a WebSocket upgrade
	// rejected with 401/403.
	ErrUnauthorized = errors.New("zoomai: unauthorized")
	// ErrTextTooLong reports input over the API limit (4,000 characters for
	// Translate, 96 KB for Summarize). The request is never sent.
	ErrTextTooLong = errors.New("zoomai: text too long")
	// ErrConnection reports a request that never got its answer: the
	// network is down, or the connection dropped. It may pass once the
	// Mac is online again.
	ErrConnection = errors.New("zoomai: connection failed")
)

// RateLimitedError reports that the API still answered 429 after all retries.
type RateLimitedError struct {
	// RetryAfter is the server's last suggested wait, capped at 30 seconds.
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("zoomai: rate limited (retry after %s)", e.RetryAfter)
}

// APIError reports any other non-2xx response.
type APIError struct {
	Status int
	// Message is the server-provided message, if the body carried one.
	Message string
}

// Error leaves Message out: a server may echo request text in it, and
// errors end up in logs.
func (e *APIError) Error() string {
	return fmt.Sprintf("zoomai: api error (status %d)", e.Status)
}

// noRedirects returns redirects as responses instead of following them: a
// redirect would resend the API key and the request's text.
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Client talks to the Zoom AI Services APIs. It is safe for concurrent use.
type Client struct {
	apiKey     string
	baseURL    *url.URL
	httpClient *http.Client
}

// Option configures a Client.
type Option func(*Client) error

// WithBaseURL overrides the API origin. The URL must be https; plain http is
// accepted only for loopback hosts so tests can use httptest servers.
func WithBaseURL(raw string) Option {
	return func(c *Client) error {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return errors.New("zoomai: invalid base URL")
		}
		switch u.Scheme {
		case "https":
		case "http":
			if !isLoopbackHost(u.Hostname()) {
				return errors.New("zoomai: base URL must be https")
			}
		default:
			return errors.New("zoomai: base URL must be https")
		}
		c.baseURL = u
		return nil
	}
}

// WithHTTPClient replaces the HTTP client used for requests and for the
// WebSocket upgrade.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) error {
		if h == nil {
			return errors.New("zoomai: nil http client")
		}
		own := *h
		own.CheckRedirect = noRedirects
		c.httpClient = &own
		return nil
	}
}

// NewClient returns a Client that authenticates with apiKey.
func NewClient(apiKey string, opts ...Option) (*Client, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("zoomai: empty api key")
	}
	base, err := url.Parse(defaultBaseURL)
	if err != nil {
		return nil, err
	}
	c := &Client{
		apiKey:     apiKey,
		baseURL:    base,
		httpClient: &http.Client{Timeout: 60 * time.Second, CheckRedirect: noRedirects},
	}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Translate translates text from the BCP-47 source language into target.
func (c *Client) Translate(ctx context.Context, text, source, target string) (string, error) {
	if strings.TrimSpace(text) == "" || source == "" || target == "" {
		return "", errors.New("zoomai: text, source and target are required")
	}
	if utf8.RuneCountInString(text) > maxTranslateRunes {
		return "", ErrTextTooLong
	}
	req := translateRequest{Text: text}
	req.Config.SourceLanguage = source
	req.Config.TargetLanguages = []string{target}
	var resp translateResponse
	if err := c.postJSON(ctx, translatePath, req, &resp, false); err != nil {
		return "", err
	}
	out, ok := resp.Result.Translations[target]
	if !ok {
		return "", errors.New("zoomai: response missing translation for target")
	}
	return out, nil
}

// SummaryTask selects the kind of summary Summarize produces.
type SummaryTask string

// Summary tasks supported by the Summarizer API.
const (
	TaskRecap       SummaryTask = "recap"
	TaskSummary     SummaryTask = "summary"
	TaskActionItems SummaryTask = "action_items"
	TaskFullSummary SummaryTask = "full_summary"
)

// Summarize summarizes a conversation transcript in the given BCP-47 language.
func (c *Client) Summarize(ctx context.Context, text string, task SummaryTask, language string) (string, error) {
	if strings.TrimSpace(text) == "" || task == "" || language == "" {
		return "", errors.New("zoomai: text, task and language are required")
	}
	if len(text) > maxSummarizeBytes {
		return "", ErrTextTooLong
	}
	var req summarizeRequest
	req.Input.Text = text
	req.Config.SummaryType = "conversation"
	req.Config.Task = task
	req.Config.Language = language
	var resp summarizeResponse
	if err := c.postJSON(ctx, summarizePath, req, &resp, false); err != nil {
		return "", err
	}
	if text := resp.Result.text(task); text != "" {
		return text, nil
	}
	return "", errors.New("zoomai: summarizer returned no text")
}

type translateRequest struct {
	Text   string `json:"text"`
	Config struct {
		SourceLanguage  string   `json:"source_language"`
		TargetLanguages []string `json:"target_languages"`
	} `json:"config"`
}

type translateResponse struct {
	Result struct {
		Translations map[string]string `json:"translations"`
	} `json:"result"`
}

type summarizeRequest struct {
	Input struct {
		Text string `json:"text"`
	} `json:"input"`
	Config struct {
		SummaryType string      `json:"summary_type"`
		Task        SummaryTask `json:"task"`
		Language    string      `json:"language"`
	} `json:"config"`
}

type summarizeResponse struct {
	Result summarizeResult `json:"result"`
}

// summarizeResult holds the text of each task; a full_summary response
// carries the recap and summary_text as well.
type summarizeResult struct {
	FullSummary string `json:"full_summary"`
	SummaryText string `json:"summary_text"`
	Recap       string `json:"recap"`
}

// text is the result of task, else the first text the response has.
func (r summarizeResult) text(task SummaryTask) string {
	byTask := map[SummaryTask]string{TaskFullSummary: r.FullSummary, TaskSummary: r.SummaryText, TaskRecap: r.Recap}
	for _, s := range []string{byTask[task], r.FullSummary, r.SummaryText, r.Recap} {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

// postJSON sends body to path and decodes a 2xx response into out. It retries
// 429 responses up to maxAttempts total, honoring Retry-After, and 503s too
// when retryUnavailable, honoring the body's metadata.retry_after_seconds.
func (c *Client) postJSON(ctx context.Context, path string, body, out any, retryUnavailable bool) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("zoomai: encode request: %w", err)
	}
	endpoint := c.baseURL.JoinPath(path).String()

	var wait time.Duration
	for attempt := 1; ; attempt++ {
		status, header, data, err := c.doOnce(ctx, endpoint, payload)
		if err != nil {
			return err
		}
		switch {
		case status >= 200 && status < 300:
			if err := json.Unmarshal(data, out); err != nil {
				return errors.New("zoomai: invalid response body")
			}
			return nil
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			return ErrUnauthorized
		case status == http.StatusTooManyRequests:
			wait = parseRetryAfter(header.Get("Retry-After"), time.Now())
			if attempt >= maxAttempts {
				return &RateLimitedError{RetryAfter: wait}
			}
			if err := sleep(ctx, wait); err != nil {
				return err
			}
		case status == http.StatusServiceUnavailable && retryUnavailable && attempt < maxAttempts:
			if err := sleep(ctx, unavailableRetryAfter(data)); err != nil {
				return err
			}
		default:
			return &APIError{Status: status, Message: errorMessage(data)}
		}
	}
}

func (c *Client) doOnce(ctx context.Context, endpoint string, payload []byte) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, nil, errors.New("zoomai: build request")
	}
	req.Header.Set(apiKeyHeader, c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, nil, nil, ctxErr
		}
		// url.Error embeds the URL, not the body or key, but unwrap anyway to
		// keep the message free of anything request-derived.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return 0, nil, nil, fmt.Errorf("zoomai: request failed: %w: %w", ErrConnection, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, nil, nil, ctxErr
		}
		return 0, nil, nil, fmt.Errorf("zoomai: read response: %w: %w", ErrConnection, err)
	}
	return resp.StatusCode, resp.Header, data, nil
}

// parseRetryAfter reads a Retry-After value (delta-seconds or HTTP-date),
// defaulting to one second and capping at thirty.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	d := defaultRetryAfter
	if secs, err := strconv.Atoi(v); err == nil {
		d = time.Duration(max(secs, 0)) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = max(t.Sub(now), 0)
	}
	return min(d, maxRetryAfter)
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// errorMessage extracts a short message from a JSON error body, if present.
func errorMessage(data []byte) string {
	var body struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &body) != nil {
		return ""
	}
	msg := strings.TrimSpace(body.Message)
	if len(msg) > maxErrorMessage {
		msg = strings.ToValidUTF8(msg[:maxErrorMessage], "")
	}
	return msg
}
