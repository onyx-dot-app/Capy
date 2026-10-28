package zoomai

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sse writes events as Speech Summarization streams them.
func sse(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		io.WriteString(w, e+"\n\n")
	}
}

const (
	transcript = `event: transcribe_complete
data: {"transcript":{"result":{"segments":[` +
		`{"id":"1","speaker":"Speaker 1","start":0.25,"end":2.5,"text":" We ship Friday. "},` +
		`{"id":"2","speaker":"Speaker 2","start":3,"end":4.75,"text":"Agreed."},` +
		`{"id":"3","start":5,"end":5,"text":"  "}]}}}`
	summaryDraft = `event: summary_updated
data: {"summary_result":"## Launch","response_state":"working","error":null}`
	summaryFinal = `event: summary_updated
data: {"summary_result":"## Launch\n\n- Ship Friday","response_state":"completed","error":null}`
	done = "event: done\ndata: {}"
)

func TestSummarizeSpeechSendsTheFileAndConfig(t *testing.T) {
	audio := []byte("RIFF....fake wav bytes")
	var (
		auth, accept string
		length       int64
		config       string
		fileName     string
		file         []byte
	)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/aiservices/speech-summarization/summarize" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		auth, accept, length = r.Header.Get("x-api-key")+"|"+r.Header.Get("Authorization"), r.Header.Get("Accept"), r.ContentLength
		form, err := r.MultipartReader()
		if err != nil {
			t.Fatal(err)
		}
		for {
			part, err := form.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(part)
			switch part.FormName() {
			case "config":
				config = string(b)
			case "file":
				fileName, file = part.FileName(), b
			}
		}
		sse(w, transcript, summaryFinal, done)
	})
	if _, err := c.SummarizeSpeech(context.Background(), bytes.NewReader(audio), int64(len(audio)),
		"/Users/me/Weekly sync.WAV", "ja-JP", nil); err != nil {
		t.Fatal(err)
	}
	if auth != "secret-key|" || accept != "text/event-stream" || length <= int64(len(audio)) {
		t.Errorf("auth %q, accept %q, length %d", auth, accept, length)
	}
	want := `{"transcribe":{"language":"ja-JP","word_time_offsets":false,"channel_separation":false,"diarization":true,` +
		`"profanity_filter":false,"output_format":"json"},"summary":{"language":"ja-JP","citations":false,` +
		`"should_remove_sentiment":false,"pii_redaction":false,"enable_dynamic_template":true}}`
	if config != want {
		t.Errorf("config = %s\nwant      %s", config, want)
	}
	// The file goes by its name alone, never the folders it is in.
	if fileName != "Weekly sync.WAV" || !bytes.Equal(file, audio) {
		t.Errorf("file %q = %q", fileName, file)
	}
}

func TestSummarizeSpeechReadsTheTranscriptAndTheLatestSummary(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		sse(w, "event: transcribe_started\ndata: {}", transcript, summaryDraft, summaryFinal, done)
	})
	audio := bytes.Repeat([]byte{1}, 300<<10)
	var stages []SpeechStage
	var sent []float64
	res, err := c.SummarizeSpeech(context.Background(), bytes.NewReader(audio), int64(len(audio)), "a.mp3", "en-US",
		func(p SpeechProgress) {
			if len(stages) == 0 || stages[len(stages)-1] != p.Stage {
				stages = append(stages, p.Stage)
			}
			if p.Stage == StageUploading {
				sent = append(sent, p.Sent)
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	want := []SpeechSegment{
		{Text: "We ship Friday.", Speaker: "Speaker 1", StartMs: 250, EndMs: 2500},
		{Text: "Agreed.", Speaker: "Speaker 2", StartMs: 3000, EndMs: 4750},
	}
	if !res.Transcribed || !slices.Equal(res.Segments, want) || res.Summary != "## Launch\n\n- Ship Friday" {
		t.Fatalf("result = %+v", res)
	}
	if !slices.Equal(stages, []SpeechStage{StageUploading, StageTranscribing, StageSummarizing}) {
		t.Errorf("stages = %v", stages)
	}
	if len(sent) < 2 || sent[len(sent)-1] != 1 || !slices.IsSorted(sent) {
		t.Errorf("upload progress = %v, want rising to 1", sent)
	}
}

func TestSummarizeSpeechHandsOverTheTranscriptBeforeTheSummary(t *testing.T) {
	summaryWritten := make(chan struct{})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		sse(w, transcript)
		w.(http.Flusher).Flush()
		<-summaryWritten // the summary waits for the transcript to be kept
		sse(w, summaryFinal, done)
	})
	var handed [][]SpeechSegment
	_, err := c.SummarizeSpeech(context.Background(), strings.NewReader("x"), 1, "a.wav", "en-US", func(p SpeechProgress) {
		if p.Transcript != nil {
			if p.Stage != StageSummarizing {
				t.Errorf("transcript handed over at stage %v", p.Stage)
			}
			handed = append(handed, p.Transcript)
			close(summaryWritten)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(handed) != 1 || len(handed[0]) != 2 || handed[0][0].Text != "We ship Friday." {
		t.Fatalf("transcripts handed over = %+v; want it once, whole", handed)
	}
}

func TestSummarizeSpeechKeepsTheRequestsTrackingID(t *testing.T) {
	const tid = "WEB_f2a7c3164c9c0aa134f59fedda450c5e"
	answer := func(status int, events ...string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.Header().Set("x-zm-trackingid", tid)
			if status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
			sse(w, events...)
		}
	}
	for name, tc := range map[string]struct {
		handler http.HandlerFunc
		failed  bool
	}{
		"summarized":     {answer(http.StatusOK, transcript, summaryFinal, done), false},
		"summary failed": {answer(http.StatusOK, transcript, done), true},
		"refused":        {answer(http.StatusBadRequest), true},
	} {
		t.Run(name, func(t *testing.T) {
			var handed []string
			res, err := newTestClient(t, tc.handler).SummarizeSpeech(context.Background(), strings.NewReader("x"), 1, "a.wav",
				"en-US", func(p SpeechProgress) {
					if p.Transcript != nil {
						handed = append(handed, p.TrackingID)
					}
				})
			if (err != nil) != tc.failed || res.TrackingID != tid {
				t.Fatalf("tracking ID %q, err %v; want %q", res.TrackingID, err, tid)
			}
			if res.Transcribed && !slices.Equal(handed, []string{tid}) {
				t.Errorf("tracking IDs with the transcript = %q, want %q", handed, tid)
			}
		})
	}
	for name, unsafe := range map[string]string{
		"spaced":    "WEB_1 2",
		"not ASCII": "WEB_1\u2028",
		"too long":  strings.Repeat("a", maxTrackingIDLen+1),
	} {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("x-zm-trackingid", unsafe)
			sse(w, transcript, summaryFinal, done)
		})
		if res, err := c.SummarizeSpeech(context.Background(), strings.NewReader("x"), 1, "a.wav", "en-US", nil); err != nil || res.TrackingID != "" {
			t.Errorf("%s: tracking ID %q, err %v; want none kept", name, res.TrackingID, err)
		}
	}
}

func TestSummarizeSpeechSaysWhenTheConnectionFailed(t *testing.T) {
	// Nothing listens: the Mac is offline, or Zoom unreachable.
	dead, err := NewClient("secret-key", WithBaseURL("http://127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dead.SummarizeSpeech(context.Background(), strings.NewReader("x"), 1, "a.wav", "en-US", nil); !errors.Is(err, ErrConnection) {
		t.Errorf("unreachable: %v; want ErrConnection", err)
	}
	if _, err := dead.Translate(context.Background(), "Hello", "en-US", "fr-FR"); !errors.Is(err, ErrConnection) {
		t.Errorf("Translate unreachable: %v; want ErrConnection", err)
	}
	// The stream cut before the transcript: the connection, not the file.
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: transcribe_started\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	})
	res, err := c.SummarizeSpeech(context.Background(), strings.NewReader("x"), 1, "a.wav", "en-US", nil)
	if !errors.Is(err, ErrConnection) || res.Transcribed {
		t.Errorf("cut stream: %+v, %v; want ErrConnection", res, err)
	}
	// A file that cannot be read is no connection's fault.
	if _, err := c.SummarizeSpeech(context.Background(), failingFile{}, 10, "a.wav", "en-US", nil); errors.Is(err, ErrConnection) {
		t.Errorf("unreadable file: %v; want no ErrConnection", err)
	}
}

// serverStream is the stream as the server writes it: its own events, and the summary
// gateway's frames passed through, named summary_updated, with its
// "data: [DONE]" terminator unnamed.
var serverStream = []string{
	"event: transcribe_started\ndata: {\"status\":\"transcribing\"}",
	`event: transcribe_complete
data: {"transcript":{"result":{"segments":[{"speaker":"Speaker 1","start":0,"end":1.5,"text":"Q3 is locked."}]}},"latency_ms":812,"body_bytes":120,"status_code":200}`,
	`event: summary_updated
data: {"summary_result":"## Q3","response_state":"working","status_message":"Generating summary","status_code":200}`,
	`event: summary_updated
data: {"summary_result":"","metadata":{"summary_result":{"summarization":{"overall_summary":"## Q3\n\n- Team locked Q3."}}},"response_state":"completed","status_message":"success","status_code":200}`,
	"data: [DONE]",
	"event: done\ndata: {\"status\":\"complete\",\"total_latency_ms\":2100,\"transcribe_ms\":812,\"summary_ms\":1288}",
}

func TestSummarizeSpeechReadsTheStreamAsTheServerWritesIt(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { sse(w, serverStream...) })
	res, err := c.SummarizeSpeech(context.Background(), strings.NewReader("x"), 1, "a.wav", "en-US", nil)
	if err != nil || len(res.Segments) != 1 || res.Summary != "## Q3\n\n- Team locked Q3." {
		t.Fatalf("result %+v, err %v", res, err)
	}
}

func TestSummarizeSpeechFailsAFrameAsTheServerDoes(t *testing.T) {
	for name, frame := range map[string]string{
		"error":            `{"error":"invalid transcript","response_state":"failed"}`,
		"failed state":     `{"summary_result":"","response_state":"failed","status_message":"model overloaded"}`,
		"completed in 4xx": `{"summary_result":"","response_state":"completed","status_code":429,"status_message":"rate limited"}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				sse(w, serverStream[1], "event: summary_updated\ndata: "+frame, done)
			})
			_, err := c.SummarizeSpeech(context.Background(), strings.NewReader("x"), 1, "a.wav", "en-US", nil)
			var se *SpeechError
			if !errors.As(err, &se) || se.Stage != StageSummarizing || se.Message == "" {
				t.Fatalf("err = %v, want a summary failure with its message", err)
			}
		})
	}
}

func TestSummarizeSpeechReadsOtherTranscriptShapes(t *testing.T) {
	for name, event := range map[string]string{
		"a JSON string": `event: transcribe_complete
data: {"transcript":"{\"segments\":[{\"speaker\":\"Speaker 1\",\"start\":1,\"end\":2,\"text\":\"Hi.\"}]}"}`,
		"text alone": `event: transcribe_complete
data: {"transcript":{"result":{"text":"Hi."}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				sse(w, event, summaryFinal, done)
			})
			res, err := c.SummarizeSpeech(context.Background(), strings.NewReader("x"), 1, "a.m4a", "en-US", nil)
			if err != nil || len(res.Segments) != 1 || res.Segments[0].Text != "Hi." {
				t.Fatalf("result = %+v, %v", res, err)
			}
		})
	}
}

func TestSummarizeSpeechKeepsTheTranscriptWhenTheSummaryFails(t *testing.T) {
	for name, events := range map[string][]string{
		"failed state": {transcript, summaryDraft, `event: summary_updated
data: {"summary_result":"","response_state":"failed","error":"model overloaded"}`},
		"error event": {transcript, `event: error
data: {"error":"model overloaded","stage":"summary"}`},
		"no done":    {transcript, summaryFinal},
		"no summary": {transcript, done},
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { sse(w, events...) })
			res, err := c.SummarizeSpeech(context.Background(), strings.NewReader("x"), 1, "a.wav", "en-US", nil)
			var se *SpeechError
			if !errors.As(err, &se) || se.Stage != StageSummarizing {
				t.Fatalf("err = %v, want a summary failure", err)
			}
			if !res.Transcribed || len(res.Segments) != 2 {
				t.Fatalf("result = %+v, want the transcript", res)
			}
			if strings.Contains(err.Error(), "overloaded") {
				t.Errorf("error %q carries the server's message", err)
			}
		})
	}
}

func TestSummarizeSpeechReportsAFailedTranscription(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		sse(w, "event: transcribe_started\ndata: {}", `event: error
data: {"error":"audio could not be decoded","stage":"transcribe"}`)
	})
	res, err := c.SummarizeSpeech(context.Background(), strings.NewReader("x"), 1, "a.wav", "en-US", nil)
	var se *SpeechError
	if !errors.As(err, &se) || se.Stage != StageTranscribing || se.Message != "audio could not be decoded" || res.Transcribed {
		t.Fatalf("result %+v, err %v; want a transcription failure with its message", res, err)
	}
}

func TestSummarizeSpeechWithoutSpeechNeedsNoSummary(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		sse(w, `event: transcribe_complete
data: {"transcript":{"result":{"segments":[]}}}`, done)
	})
	res, err := c.SummarizeSpeech(context.Background(), strings.NewReader("x"), 1, "a.wav", "en-US", nil)
	if err != nil || !res.Transcribed || len(res.Segments) != 0 {
		t.Fatalf("result %+v, err %v", res, err)
	}
}

func TestSummarizeSpeechRefusesFilesItCannotSend(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	for name, tc := range map[string]struct {
		file string
		size int64
		want error
	}{
		"type":   {"notes.txt", 10, ErrUnsupportedFile},
		"size":   {"long.mp3", MaxSpeechFileBytes + 1, ErrFileTooLarge},
		"no ext": {"recording", 10, ErrUnsupportedFile},
	} {
		if _, err := c.SummarizeSpeech(context.Background(), strings.NewReader(""), tc.size, tc.file, "en-US", nil); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("%d requests sent", calls.Load())
	}
	if !IsSpeechFile("Interview.M4A") || IsSpeechFile("a.wav.txt") {
		t.Error("IsSpeechFile ignores case or reads the wrong extension")
	}
}

func TestSummarizeSpeechMapsHTTPErrors(t *testing.T) {
	for status, want := range map[int]error{
		http.StatusUnauthorized:          ErrUnauthorized,
		http.StatusForbidden:             ErrUnauthorized,
		http.StatusRequestEntityTooLarge: ErrFileTooLarge,
	} {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
		if _, err := c.SummarizeSpeech(context.Background(), strings.NewReader("x"), 1, "a.wav", "en-US", nil); !errors.Is(err, want) {
			t.Errorf("%d: err = %v, want %v", status, err, want)
		}
	}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"message":"unsupported codec"}`)
	})
	_, err := c.SummarizeSpeech(context.Background(), strings.NewReader("x"), 1, "a.wav", "en-US", nil)
	var api *APIError
	if !errors.As(err, &api) || api.Status != http.StatusBadRequest || api.Message != "unsupported codec" {
		t.Errorf("err = %v", err)
	}
}

func TestSummarizeSpeechSendsTheWholeFileAgainAfterARateLimit(t *testing.T) {
	var calls atomic.Int32
	audio := []byte("the whole file")
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		form, _ := r.MultipartReader()
		var got []byte
		for part, err := form.NextPart(); err == nil; part, err = form.NextPart() {
			if part.FormName() == "file" {
				got, _ = io.ReadAll(part)
			}
		}
		if !bytes.Equal(got, audio) {
			t.Errorf("attempt %d sent %q", calls.Load()+1, got)
		}
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		sse(w, transcript, summaryFinal, done)
	})
	if _, err := c.SummarizeSpeech(context.Background(), bytes.NewReader(audio), int64(len(audio)), "a.wav", "en-US", nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

func TestSummarizeSpeechSendsANameThatCannotEndThePartHeader(t *testing.T) {
	var name string
	var parts []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		form, _ := r.MultipartReader()
		for part, err := form.NextPart(); err == nil; part, err = form.NextPart() {
			parts = append(parts, part.FormName())
			if part.FormName() == "file" {
				name = part.FileName()
			}
		}
		sse(w, transcript, summaryFinal, done)
	})
	if _, err := c.SummarizeSpeech(context.Background(), strings.NewReader("x"), 1, "a\r\nX-Evil: 1\r\n.wav", "en-US", nil); err != nil {
		t.Fatal(err)
	}
	if name != "a__X-Evil: 1__.wav" || !slices.Equal(parts, []string{"config", "file"}) {
		t.Errorf("file name %q, parts %q", name, parts)
	}
}

func TestSummarizeSpeechEndsAtDoneThoughTheStreamStaysOpen(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		sse(w, transcript, summaryFinal, done)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // a proxy keeping the connection
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := c.SummarizeSpeech(ctx, strings.NewReader("x"), 1, "a.wav", "en-US", nil)
	if err != nil || res.Summary == "" {
		t.Fatalf("result %+v, err %v; want it done", res, err)
	}
}

// failingFile fails to read as a file on an ejected drive does.
type failingFile struct{}

func (failingFile) ReadAt([]byte, int64) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: "/Volumes/Private/Interview with Ann.wav", Err: io.ErrUnexpectedEOF}
}

func TestSummarizeSpeechErrorsNeverNameTheFile(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body) })
	_, err := c.SummarizeSpeech(context.Background(), failingFile{}, 100, "Interview with Ann.wav", "en-US", nil)
	if err == nil || strings.Contains(err.Error(), "Ann") || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v; want the cause without the path", err)
	}
}

func TestSummarizeSpeechRejectsAnAnswerThatIsNotAStream(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"transcript":"x"}`)
	})
	if _, err := c.SummarizeSpeech(context.Background(), strings.NewReader("x"), 1, "a.wav", "en-US", nil); err == nil {
		t.Fatal("want an error")
	}
}
