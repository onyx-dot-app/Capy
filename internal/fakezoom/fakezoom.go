// Package fakezoom is a scripted stand-in for the Zoom AI Services APIs
// Capy uses, for integration and E2E tests: Scribe Live "recognizes" the
// turns of a script as audio bytes arrive, Translator, Summarizer, Speech
// Summarization and Decisions answer deterministically.
package fakezoom

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/onyx-dot-app/Capy/internal/audio"
)

// bytesPerMs is the rate of the audio, 100 ms a frame.
const bytesPerMs = audio.FrameBytes / 100

// Turn is one utterance of the script, recognized once the session has
// received AfterBytes bytes of audio in total. It starts where the turn
// before it ends: a flush (session.close) completes a turn whose audio has
// started arriving, as Scribe finalizes the utterance in progress.
type Turn struct {
	Text       string
	AfterBytes int
	// Language is the language Scribe detected ("zh"), sent only when set,
	// as Scribe sends it only when it detects; Speaker who said it.
	Language, Speaker string
}

// Script controls the fake.
type Script struct {
	APIKey string
	Turns  []Turn
	// DropAfterBytes closes the first live connection abruptly once it has
	// received this many bytes, to exercise reconnecting; 0 never drops.
	DropAfterBytes int
	// FatalAfterBytes sends a fatal error event and closes the connection
	// once it has received this many bytes; 0 never does.
	FatalAfterBytes int
	// FailTranslate makes Translator fail for texts containing it.
	FailTranslate string
	// TranslateDelay holds each translation this long, as a slow server does.
	TranslateDelay time.Duration
	// FailSummarize makes Summarizer's first FailSummarize calls fail with
	// a 503.
	FailSummarize int
	// CloseDelay holds a session.close this long before flushing, as a
	// slow server does.
	CloseDelay time.Duration
	// FailSpeech makes Speech Summarization fail to transcribe files whose
	// name contains it; FailSpeechSummary fail to summarize them, after
	// their transcript.
	FailSpeech, FailSpeechSummary string
	// HoldTranscript holds files whose name contains it before their
	// transcript, HoldSummary after it, before their summary, until
	// Release or their request ends.
	HoldTranscript, HoldSummary string
	// Decisions are what Decisions finds in the new turns of a check; it
	// finds nothing else.
	Decisions []Decision
	// DecideStatus, when set, is the status of every Decisions answer.
	DecideStatus int
}

// Decision is Decisions sure, at P, that the new turn containing Text is
// of question Kind's kind.
type Decision struct {
	Kind, Text string
	P          float64
}

// Server is a running fake.
type Server struct {
	*httptest.Server
	script Script

	mu          sync.Mutex
	next        int // index of the next turn to recognize
	received    int // audio bytes over all connections
	connections int
	flushes     int      // session.close messages received
	languages   []string // session.update languages, per connection; "auto" when detecting
	translate   []string // "source>target" per call
	summarize   []string // "task/language" per call
	speech      []string // "file name/language" per call
	decide      []string // the input of each Decisions call
	offline     bool     // speech requests drop their connection
	release     chan struct{}
}

// New starts a fake serving script.
func New(script Script) *Server {
	s := &Server{script: script, release: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/aiservices/scribe/live", s.live)
	mux.HandleFunc("POST /v2/aiservices/translator/translate", s.translateHandler)
	mux.HandleFunc("POST /v2/aiservices/summarizer/summarize", s.summarizeHandler)
	mux.HandleFunc("POST /v2/aiservices/speech-summarization/summarize", s.speechHandler)
	mux.HandleFunc("POST /v2/aiservices/decisions/decide", s.decideHandler)
	s.Server = httptest.NewServer(mux)
	return s
}

// SetOffline makes Speech Summarization drop each request's connection
// unanswered, as Zoom seen from a Mac offline, until it is set back.
func (s *Server) SetOffline(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offline = on
}

// Release lets the files held by HoldTranscript and HoldSummary go on, and
// those after them.
func (s *Server) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.release:
	default:
		close(s.release)
	}
}

// hold waits for Release when name contains what, until r ends.
func (s *Server) hold(r *http.Request, name, what string) {
	if what == "" || !strings.Contains(name, what) {
		return
	}
	select {
	case <-s.release:
	case <-r.Context().Done():
	}
}

// Flushes returns how many session.close messages the fake received.
func (s *Server) Flushes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushes
}

// Calls returns what the fake was asked, for assertions.
func (s *Server) Calls() (connections int, languages, translate, summarize []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connections, append([]string(nil), s.languages...),
		append([]string(nil), s.translate...), append([]string(nil), s.summarize...)
}

// SpeechCalls returns the files Speech Summarization was sent, as "file
// name/language", in the order they arrived.
func (s *Server) SpeechCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.speech...)
}

// DecideCalls returns the input of each Decisions call, in order.
func (s *Server) DecideCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.decide...)
}

// decideHandler answers each choice question with the new turn a
// Decision of its kind names, else none.
func (s *Server) decideHandler(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	var req struct {
		Input     string `json:"input"`
		Questions []struct {
			Type    string `json:"type"`
			Name    string `json:"name"`
			Choices []struct {
				Value string `json:"value"`
			} `json:"choices"`
		} `json:"questions"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Questions) == 0 {
		http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.decide = append(s.decide, req.Input)
	s.mu.Unlock()
	if s.script.DecideStatus != 0 {
		http.Error(w, `{"message":"scripted"}`, s.script.DecideStatus)
		return
	}
	// The new turns, "[t12 03:41 S2] text", by text.
	_, news, _ := strings.Cut(req.Input, "\nNEW:\n")
	ids := map[string]string{}
	for line := range strings.Lines(news) {
		head, text, ok := strings.Cut(strings.TrimPrefix(line, "["), "] ")
		if id, _, _ := strings.Cut(head, " "); ok {
			ids[strings.TrimSpace(text)] = id
		}
	}
	answers := []map[string]any{}
	for _, q := range req.Questions {
		choice, p := "none", 0.97
		for _, d := range s.script.Decisions {
			if d.Kind != q.Name {
				continue
			}
			for text, id := range ids {
				if strings.Contains(text, d.Text) {
					choice, p = id, d.P
				}
			}
		}
		probs := []map[string]any{}
		for _, c := range q.Choices {
			switch c.Value {
			case choice:
				probs = append(probs, map[string]any{"value": c.Value, "probability": p})
			case "none":
				probs = append(probs, map[string]any{"value": c.Value, "probability": 1 - p})
			default:
				probs = append(probs, map[string]any{"value": c.Value, "probability": 0})
			}
		}
		answers = append(answers, map[string]any{"type": "choice", "name": q.Name, "choice": choice, "probabilities": probs})
	}
	writeJSON(w, map[string]any{"request_id": "fake", "model": "zoom-decisions", "answers": answers})
}

func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("x-api-key") != s.script.APIKey || r.Header.Get("Authorization") != "" {
		http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *Server) live(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	ctx := r.Context()

	s.mu.Lock()
	s.connections++
	first := s.connections == 1
	s.mu.Unlock()

	send := func(v any) error {
		b, _ := json.Marshal(v)
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return conn.Write(wctx, websocket.MessageText, b)
	}
	var received int
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageText {
			var msg struct {
				Type     string `json:"type"`
				Language string `json:"language"`
				Config   struct {
					Detection *struct{} `json:"language_detection"`
				} `json:"config"`
			}
			if json.Unmarshal(data, &msg) != nil {
				return
			}
			switch msg.Type {
			case "session.update":
				if msg.Config.Detection != nil {
					msg.Language = "auto"
				}
				s.mu.Lock()
				s.languages = append(s.languages, msg.Language)
				s.mu.Unlock()
				_ = send(map[string]any{"type": "session.created", "session_id": "fake"})
				_ = send(map[string]any{"type": "session.updated"})
			case "session.close":
				s.mu.Lock()
				s.flushes++
				s.mu.Unlock()
				time.Sleep(s.script.CloseDelay)
				for _, ev := range s.recognize(-1, received) {
					_ = send(ev)
				}
				_ = send(map[string]any{"type": "session.closed", "reason": "client_close"})
				_ = conn.Close(websocket.StatusNormalClosure, "")
				return
			}
			continue
		}
		received += len(data)
		if first && s.script.DropAfterBytes > 0 && received >= s.script.DropAfterBytes {
			return // abrupt: no close frame
		}
		if s.script.FatalAfterBytes > 0 && received >= s.script.FatalAfterBytes {
			_ = send(map[string]any{"type": "error", "error": map[string]any{
				"code": "quota_exceeded", "message": "quota exceeded", "fatal": true}})
			_ = conn.Close(websocket.StatusPolicyViolation, "")
			return
		}
		for _, ev := range s.recognize(len(data), received) {
			if send(ev) != nil {
				return
			}
		}
	}
}

// recognize counts n more audio bytes and returns the events of the turns
// that completed. n < 0 flushes: the turn in progress completes too. Turns
// end at conn, the bytes this connection received: as Scribe's, their audio
// times count from the start of the connection.
func (s *Server) recognize(n, conn int) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received += max(n, 0)
	var events []map[string]any
	for s.next < len(s.script.Turns) {
		done := s.received >= s.script.Turns[s.next].AfterBytes
		started := s.next == 0 || s.received > s.script.Turns[s.next-1].AfterBytes
		if !done && (n >= 0 || !started) {
			break
		}
		turn := s.script.Turns[s.next]
		id := fmt.Sprintf("item-%d", s.next)
		end := int64(conn) / bytesPerMs
		start := max(end-900, 0)
		runes := []rune(turn.Text)
		completed := map[string]any{"type": "transcription.completed", "item_id": id, "transcript": turn.Text,
			"audio_start_ms": start, "audio_end_ms": end}
		if turn.Language != "" {
			completed["language"] = turn.Language
		}
		if turn.Speaker != "" {
			completed["speaker"] = turn.Speaker
		}
		events = append(events,
			map[string]any{"type": "transcription.delta", "item_id": id, "delta": string(runes[:len(runes)/2])},
			completed,
		)
		s.next++
	}
	return events
}

func (s *Server) translateHandler(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	var req struct {
		Text   string `json:"text"`
		Config struct {
			Source  string   `json:"source_language"`
			Targets []string `json:"target_languages"`
		} `json:"config"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Config.Targets) != 1 {
		http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
		return
	}
	target := req.Config.Targets[0]
	s.mu.Lock()
	s.translate = append(s.translate, req.Config.Source+">"+target)
	s.mu.Unlock()
	time.Sleep(s.script.TranslateDelay)
	if s.script.FailTranslate != "" && strings.Contains(req.Text, s.script.FailTranslate) {
		http.Error(w, `{"message":"translation failed"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{
		"request_id": "fake",
		"result":     map[string]any{"translations": map[string]string{target: "[" + target + "] " + req.Text}},
	})
}

func (s *Server) summarizeHandler(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	var req struct {
		Input struct {
			Text string `json:"text"`
		} `json:"input"`
		Config struct {
			Task     string `json:"task"`
			Language string `json:"language"`
		} `json:"config"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.summarize = append(s.summarize, req.Config.Task+"/"+req.Config.Language)
	fail := len(s.summarize) <= s.script.FailSummarize
	s.mu.Unlock()
	if fail {
		http.Error(w, `{"message":"summarizer unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	lines := strings.Count(req.Input.Text, "\n")
	text := fmt.Sprintf("# Recap (%s)\n\n- %d lines discussed\n- Action: follow up", req.Config.Language, lines)
	// The Summarizer names the result by its task.
	field := map[string]string{"full_summary": "full_summary", "summary": "summary_text", "recap": "recap"}[req.Config.Task]
	if field == "" {
		http.Error(w, `{"message":"unsupported task"}`, http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{
		"request_id": "fake",
		"task":       req.Config.Task,
		"model":      "fake",
		"result":     map[string]any{field: text},
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// speechHandler transcribes a file into two turns that name it and its
// size, then streams a summary of it in two growing frames, as Speech
// Summarization streams its own: the summary gateway's frames with their
// status, the last with the summary in its metadata, and its unnamed
// "data: [DONE]" terminator.
func (s *Server) speechHandler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	offline := s.offline
	s.mu.Unlock()
	if offline {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
		return
	}
	if !s.authorized(w, r) {
		return
	}
	form, err := r.MultipartReader()
	if err != nil {
		http.Error(w, `{"message":"multipart body required"}`, http.StatusBadRequest)
		return
	}
	var (
		name   string
		size   int64
		config struct {
			Transcribe struct {
				Language string `json:"language"`
			} `json:"transcribe"`
			Summary struct {
				Language string `json:"language"`
			} `json:"summary"`
		}
	)
	for {
		part, err := form.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, `{"message":"malformed multipart body"}`, http.StatusBadRequest)
			return
		}
		switch part.FormName() {
		case "config":
			if json.NewDecoder(part).Decode(&config) != nil {
				http.Error(w, `{"message":"invalid config"}`, http.StatusBadRequest)
				return
			}
		case "file":
			name = part.FileName()
			size, _ = io.Copy(io.Discard, part)
		}
	}
	if name == "" || config.Transcribe.Language == "" || config.Summary.Language != config.Transcribe.Language {
		http.Error(w, `{"message":"file and languages required"}`, http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.speech = append(s.speech, name+"/"+config.Transcribe.Language)
	call := len(s.speech)
	s.mu.Unlock()

	// As Zoom's gateway answers, with the ID its logs know the request by.
	w.Header().Set("x-zm-trackingid", fmt.Sprintf("FAKEZOOM_speech_%d", call))
	w.Header().Set("Content-Type", "text/event-stream")
	send := func(event string, data any) {
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	send("transcribe_started", map[string]any{})
	if s.script.FailSpeech != "" && strings.Contains(name, s.script.FailSpeech) {
		send("error", map[string]any{"error": "transcription failed", "stage": "transcribe"})
		return
	}
	s.hold(r, name, s.script.HoldTranscript)
	send("transcribe_complete", map[string]any{"transcript": map[string]any{"result": map[string]any{"segments": []map[string]any{
		{"id": "1", "speaker": "Speaker 1", "start": 0.5, "end": 2.25, "text": "This is " + name + "."},
		{"id": "2", "speaker": "Speaker 2", "start": 3, "end": 4.5, "text": fmt.Sprintf("It is %d bytes long.", size)},
	}}}})
	s.hold(r, name, s.script.HoldSummary)
	summary := fmt.Sprintf("## Summary (%s)\n\n- %s was discussed", config.Summary.Language, name)
	send("summary_updated", map[string]any{"summary_result": summary[:len(summary)/2], "response_state": "working",
		"status_message": "Generating summary", "status_code": 200})
	if s.script.FailSpeechSummary != "" && strings.Contains(name, s.script.FailSpeechSummary) {
		send("summary_updated", map[string]any{"summary_result": "", "response_state": "failed", "error": "summary failed"})
		return
	}
	send("summary_updated", map[string]any{"summary_result": "", "response_state": "completed", "status_message": "success",
		"status_code": 200, "metadata": map[string]any{"summary_result": map[string]any{"summarization": map[string]any{"overall_summary": summary}}}})
	fmt.Fprint(w, "data: [DONE]\n\n")
	send("done", map[string]any{"status": "complete"})
}
