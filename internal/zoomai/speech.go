package zoomai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	speechPath = "/v2/aiservices/speech-summarization/summarize"

	// MaxSpeechFileBytes is the largest file Speech Summarization takes.
	MaxSpeechFileBytes = 100 << 20

	maxSpeechEventBytes = 32 << 20 // one line of the event stream: a long transcript
	maxTrackingIDLen    = 256      // as Zoom's servers bound it
)

// speechFileTypes are the extensions of the files Speech Summarization
// takes. Immutable.
var speechFileTypes = []string{".wav", ".mp3", ".m4a", ".mp4", ".flac", ".ogg", ".webm", ".mov"}

// SpeechFileTypes returns the extensions of the audio and video files
// Speech Summarization takes, lower case, with the dot.
func SpeechFileTypes() []string { return slices.Clone(speechFileTypes) }

// IsSpeechFile reports whether Speech Summarization takes a file named name.
func IsSpeechFile(name string) bool {
	return slices.Contains(speechFileTypes, strings.ToLower(filepath.Ext(name)))
}

var (
	// ErrFileTooLarge reports a file over MaxSpeechFileBytes, never sent,
	// or one the server refused as too large.
	ErrFileTooLarge = errors.New("zoomai: file too large")
	// ErrUnsupportedFile reports a file whose type Speech Summarization does
	// not take. The request is never sent.
	ErrUnsupportedFile = errors.New("zoomai: unsupported file type")
)

// SpeechError reports a failure the event stream announced, or a stream
// that ended before it was done.
type SpeechError struct {
	// Stage is the step that failed: StageTranscribing or StageSummarizing.
	Stage SpeechStage
	// Message is the server's, if the stream carried one.
	Message string
}

// Error leaves Message out, as APIError does.
func (e *SpeechError) Error() string {
	if e.Stage == StageSummarizing {
		return "zoomai: speech summarization: summary failed"
	}
	return "zoomai: speech summarization: transcription failed"
}

// SpeechStage is the step of SummarizeSpeech under way.
type SpeechStage int

// The steps of SummarizeSpeech, in order.
const (
	StageUploading SpeechStage = iota
	StageTranscribing
	StageSummarizing
)

// SpeechProgress is how far SummarizeSpeech has come.
type SpeechProgress struct {
	Stage SpeechStage
	// Sent is the fraction of the file uploaded, from 0 to 1.
	Sent float64
	// Transcript is the whole transcript, on the progress that it arrived
	// only: the first of StageSummarizing. It is worth keeping before the
	// summary is written.
	Transcript []SpeechSegment
	// TrackingID is the request's, as SpeechResult's, on the progress that
	// carries Transcript only.
	TrackingID string
}

// SpeechSegment is one turn of a transcript.
type SpeechSegment struct {
	Text    string
	Speaker string // Scribe's label, as "Speaker 1", if any
	// StartMs and EndMs are offsets from the start of the file.
	StartMs, EndMs int64
}

// SpeechResult is what SummarizeSpeech made of a file.
type SpeechResult struct {
	// Transcribed reports that the transcript arrived: Segments is then the
	// whole of it, none for a file without speech.
	Transcribed bool
	Segments    []SpeechSegment
	// Summary is the summary, as Markdown.
	Summary string
	// TrackingID is the x-zm-trackingid Zoom answered the request with,
	// which its logs know it by; "" for none, or one that is not a plain
	// token. It is set whenever an answer came, an error's too.
	TrackingID string
}

// SummarizeSpeech uploads an audio or video file of size bytes, named name,
// in which language (BCP-47) is spoken, and returns its transcript and a
// summary in the same language. progress, which may be nil, hears how far
// it has come, from one goroutine at a time; the stage never goes back.
//
// A failure after the transcript arrived returns it along with the error,
// a *SpeechError of StageSummarizing or the request's: the transcript is
// worth keeping.
func (c *Client) SummarizeSpeech(ctx context.Context, file io.ReaderAt, size int64, name, language string,
	progress func(SpeechProgress)) (SpeechResult, error) {
	switch {
	case strings.TrimSpace(name) == "" || language == "" || size < 0:
		return SpeechResult{}, errors.New("zoomai: file name, size and language are required")
	case !IsSpeechFile(name):
		return SpeechResult{}, ErrUnsupportedFile
	case size > MaxSpeechFileBytes:
		return SpeechResult{}, ErrFileTooLarge
	}
	report := newSpeechReporter(progress)
	head, tail, contentType, err := speechForm(filepath.Base(name), language)
	if err != nil {
		return SpeechResult{}, err
	}
	// The stream lasts as long as the file takes: the context bounds it,
	// not the client's timeout for a request and its answer.
	streaming := *c.httpClient
	streaming.Timeout = 0
	endpoint := c.baseURL.JoinPath(speechPath).String()
	for attempt := 1; ; attempt++ {
		body := io.MultiReader(bytes.NewReader(head),
			&countingReader{r: io.NewSectionReader(file, 0, size), size: size, report: report}, bytes.NewReader(tail))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
		if err != nil {
			return SpeechResult{}, errors.New("zoomai: build request")
		}
		req.ContentLength = int64(len(head)) + size + int64(len(tail))
		req.Header.Set(apiKeyHeader, c.apiKey)
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := streaming.Do(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return SpeechResult{}, ctxErr
			}
			var uerr *url.Error
			if errors.As(err, &uerr) {
				err = uerr.Err
			}
			// Reading the file failed, as a drive ejected does: its error
			// names the file, and errors end up in logs.
			if pe, ok := errors.AsType[*fs.PathError](err); ok {
				return SpeechResult{}, fmt.Errorf("zoomai: request failed: %s file: %w", pe.Op, pe.Err)
			}
			return SpeechResult{}, fmt.Errorf("zoomai: request failed: %w: %w", ErrConnection, err)
		}
		answered := SpeechResult{TrackingID: trackingID(resp.Header)}
		if resp.StatusCode == http.StatusOK {
			defer resp.Body.Close()
			return readSpeechStream(ctx, resp, report, answered)
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return answered, ErrUnauthorized
		case http.StatusRequestEntityTooLarge:
			return answered, ErrFileTooLarge
		case http.StatusTooManyRequests:
			wait := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
			if attempt >= maxAttempts {
				return answered, &RateLimitedError{RetryAfter: wait}
			}
			if err := sleep(ctx, wait); err != nil {
				return answered, err
			}
		default:
			return answered, &APIError{Status: resp.StatusCode, Message: errorMessage(data)}
		}
	}
}

// speechConfig is the request's config part: Scribe with speakers told
// apart, and a summary in the spoken language, without citations, which
// Capy does not show.
func speechConfig(language string) ([]byte, error) {
	type transcribe struct {
		Language          string `json:"language"`
		WordTimeOffsets   bool   `json:"word_time_offsets"`
		ChannelSeparation bool   `json:"channel_separation"`
		Diarization       bool   `json:"diarization"`
		ProfanityFilter   bool   `json:"profanity_filter"`
		OutputFormat      string `json:"output_format"`
	}
	type summary struct {
		Language              string `json:"language"`
		Citations             bool   `json:"citations"`
		ShouldRemoveSentiment bool   `json:"should_remove_sentiment"`
		PIIRedaction          bool   `json:"pii_redaction"`
		EnableDynamicTemplate bool   `json:"enable_dynamic_template"`
	}
	return json.Marshal(struct {
		Transcribe transcribe `json:"transcribe"`
		Summary    summary    `json:"summary"`
	}{
		Transcribe: transcribe{Language: language, Diarization: true, OutputFormat: "json"},
		Summary:    summary{Language: language, EnableDynamicTemplate: true},
	})
}

// speechForm is the multipart body around the file's bytes: the config
// part and the file part's header before them, the closing boundary after.
// Knowing both, the request has a length and can be sent again.
func speechForm(name, language string) (head, tail []byte, contentType string, err error) {
	config, err := speechConfig(language)
	if err != nil {
		return nil, nil, "", err
	}
	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	if err := form.WriteField("config", string(config)); err != nil {
		return nil, nil, "", err
	}
	// CreateFormFile escapes quotes, not line breaks, which would end the
	// part's header.
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '_'
		}
		return r
	}, name)
	if _, err := form.CreateFormFile("file", name); err != nil {
		return nil, nil, "", err
	}
	n := buf.Len()
	if err := form.Close(); err != nil {
		return nil, nil, "", err
	}
	b := buf.Bytes()
	return b[:n:n], b[n:], form.FormDataContentType(), nil
}

// countingReader reports the fraction of the file read as it is sent.
type countingReader struct {
	r      io.Reader
	size   int64
	read   int64
	report *speechReporter
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.read += int64(n)
	if r.size > 0 {
		r.report.upload(float64(r.read) / float64(r.size))
	}
	return n, err
}

// speechReporter passes progress on one call at a time, the stage only
// forward and the upload by whole percents: the transport reads the body
// on a goroutine of its own, and a retry reads the file again.
type speechReporter struct {
	mu    sync.Mutex
	fn    func(SpeechProgress)
	last  SpeechProgress
	begun bool
}

func newSpeechReporter(fn func(SpeechProgress)) *speechReporter {
	return &speechReporter{fn: fn}
}

func (r *speechReporter) upload(sent float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sent = math.Floor(min(sent, 1)*100) / 100
	if r.fn == nil || r.last.Stage != StageUploading || r.begun && sent <= r.last.Sent {
		return
	}
	r.last, r.begun = SpeechProgress{Stage: StageUploading, Sent: sent}, true
	r.fn(r.last)
}

func (r *speechReporter) stage(s SpeechStage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fn == nil || s <= r.last.Stage {
		return
	}
	r.last = SpeechProgress{Stage: s, Sent: 1}
	r.fn(r.last)
}

// transcribed reports the transcript, which moves to StageSummarizing.
func (r *speechReporter) transcribed(segments []SpeechSegment, trackingID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fn == nil || r.last.Stage >= StageSummarizing {
		return
	}
	r.last = SpeechProgress{Stage: StageSummarizing, Sent: 1}
	r.fn(SpeechProgress{Stage: StageSummarizing, Sent: 1, Transcript: segments, TrackingID: trackingID})
}

// readSpeechStream reads the event stream of a 200 response into res: the
// transcript, then the summary as it grows, then done.
func readSpeechStream(ctx context.Context, resp *http.Response, report *speechReporter, res SpeechResult) (SpeechResult, error) {
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "text/event-stream" {
		return res, errors.New("zoomai: speech summarization: unexpected response")
	}
	report.stage(StageTranscribing) // the file is up
	var (
		done        bool
		event, data string
	)
	fail := func(message string) error {
		stage := StageTranscribing
		if res.Transcribed {
			stage = StageSummarizing
		}
		return &SpeechError{Stage: stage, Message: shortMessage(message)}
	}
	dispatch := func() error {
		defer func() { event, data = "", "" }()
		if data == "" {
			return nil
		}
		var payload map[string]json.RawMessage
		if json.Unmarshal([]byte(data), &payload) != nil {
			// The summary gateway's frames pass through as they are, its
			// "data: [DONE]" terminator among them: no event of ours.
			return nil
		}
		if event == "error" || failed(payload) {
			return fail(errorText(payload))
		}
		switch event {
		case "transcribe_started":
			report.stage(StageTranscribing)
		case "transcribe_complete":
			segments, err := parseTranscript(payload["transcript"])
			if err != nil {
				return err
			}
			res.Transcribed, res.Segments = true, segments
			report.transcribed(segments, res.TrackingID)
		case "done":
			done = true
		default: // summary_updated: the whole summary so far
			if text := summaryText(payload); text != "" {
				res.Summary = text
			}
		}
		return nil
	}
	// Each event's size is bounded, not the stream's: a summary updated
	// often resends itself whole. The context bounds how long it lasts.
	lines := bufio.NewScanner(resp.Body)
	lines.Buffer(make([]byte, 0, 64<<10), maxSpeechEventBytes)
	for !done && lines.Scan() {
		line := strings.TrimSuffix(lines.Text(), "\r")
		switch {
		case line == "":
			if err := dispatch(); err != nil {
				return res, err
			}
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data != "" {
				data += "\n"
			}
			data += strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
		}
	}
	if err := lines.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return res, ctxErr
		}
		return res, fmt.Errorf("zoomai: speech summarization: read stream: %w: %w", ErrConnection, err)
	}
	if err := dispatch(); err != nil { // a last event without its blank line
		return res, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, ctxErr
	}
	spoke := slices.ContainsFunc(res.Segments, func(s SpeechSegment) bool { return s.Text != "" })
	if !done || !res.Transcribed || spoke && strings.TrimSpace(res.Summary) == "" {
		return res, fail("")
	}
	return res, nil
}

// trackingID is the x-zm-trackingid of h, "" for none or for one longer
// than Zoom's or not of visible ASCII: it is shown and logged as it is.
func trackingID(h http.Header) string {
	tid := h.Get("x-zm-trackingid")
	if len(tid) > maxTrackingIDLen || strings.ContainsFunc(tid, func(r rune) bool { return r <= ' ' || r > '~' }) {
		return ""
	}
	return tid
}

// failed reports a frame that says the summary failed, as the server
// itself reads the summary gateway's: an error, a failed state, or a
// completed one with an error status. A status message alone is no failure.
func failed(payload map[string]json.RawMessage) bool {
	var state string
	var status int
	_ = json.Unmarshal(payload["response_state"], &state)
	_ = json.Unmarshal(payload["status_code"], &status)
	state = strings.ToLower(strings.TrimSpace(state))
	return state == "failed" || state == "error" || status >= 400 && state == "completed" ||
		message(payload["error"]) != ""
}

// errorText is a failed event's message: its error, else its status
// message.
func errorText(payload map[string]json.RawMessage) string {
	if m := message(payload["error"]); m != "" {
		return m
	}
	return message(payload["status_message"])
}

// message is a message given as a string or as an object's message, "" for
// none or null.
func message(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return strings.TrimSpace(obj.Message)
	}
	return ""
}

// shortMessage bounds a server message, as errorMessage does.
func shortMessage(msg string) string {
	msg = strings.TrimSpace(msg)
	if len(msg) > maxErrorMessage {
		msg = strings.ToValidUTF8(msg[:maxErrorMessage], "")
	}
	return msg
}

// summaryText is the summary a summary_updated event carries, looked for
// where the server itself looks: Markdown in summary_result, a structured
// result's overall summary, the same in metadata, else text.
func summaryText(payload map[string]json.RawMessage) string {
	type structured struct {
		Summarization struct {
			OverallSummary string `json:"overall_summary"`
		} `json:"summarization"`
	}
	var text string
	var result structured
	var meta struct {
		SummaryResult structured `json:"summary_result"`
	}
	switch {
	case json.Unmarshal(payload["summary_result"], &text) == nil && strings.TrimSpace(text) != "":
		return text
	case json.Unmarshal(payload["summary_result"], &result) == nil && result.Summarization.OverallSummary != "":
		return result.Summarization.OverallSummary
	case json.Unmarshal(payload["metadata"], &meta) == nil && meta.SummaryResult.Summarization.OverallSummary != "":
		return meta.SummaryResult.Summarization.OverallSummary
	case json.Unmarshal(payload["text"], &text) == nil && strings.TrimSpace(text) != "":
		return text
	}
	return ""
}

// parseTranscript reads transcribe_complete's transcript: Scribe's
// response, as an object or a JSON string, whose segments are in result
// or at its top. A transcript without segments is its text as one turn.
func parseTranscript(raw json.RawMessage) ([]SpeechSegment, error) {
	var quoted string
	if json.Unmarshal(raw, &quoted) == nil {
		raw = json.RawMessage(quoted)
	}
	type body struct {
		Segments []json.RawMessage `json:"segments"`
		Text     string            `json:"text"`
	}
	var transcript struct {
		body
		Result *body `json:"result"`
	}
	if err := json.Unmarshal(raw, &transcript); err != nil {
		return nil, errors.New("zoomai: speech summarization: invalid transcript")
	}
	b := transcript.body
	if transcript.Result != nil {
		b = *transcript.Result
	}
	var out []SpeechSegment
	for _, item := range b.Segments {
		var s struct {
			Text    string  `json:"text"`
			Speaker string  `json:"speaker"`
			Start   float64 `json:"start"`
			End     float64 `json:"end"`
		}
		if json.Unmarshal(item, &s) != nil || strings.TrimSpace(s.Text) == "" {
			continue // a turn without text, or a speaker that is not a label
		}
		start := max(int64(math.Round(s.Start*1000)), 0)
		out = append(out, SpeechSegment{Text: strings.TrimSpace(s.Text), Speaker: strings.TrimSpace(s.Speaker),
			StartMs: start, EndMs: max(int64(math.Round(s.End*1000)), start)})
	}
	if len(out) == 0 && strings.TrimSpace(b.Text) != "" {
		out = append(out, SpeechSegment{Text: strings.TrimSpace(b.Text)})
	}
	return out, nil
}
