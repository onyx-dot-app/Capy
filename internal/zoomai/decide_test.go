package zoomai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var decideReq = DecideRequest{
	Model: DecisionsModel, ReferenceID: "ref-1", Input: "NEW:\n[t1 00:01 S1] Alex, can you take it?",
	Questions: []ChoiceQuestion{
		{Name: "asked_you", Instructions: "Which NEW turn asks the listener?", Choices: []string{"none", "t1"}},
		{Name: "decision", Instructions: "Which NEW turn is a decision?", Choices: []string{"none", "t1"}},
	},
}

func TestDecideSendsChoiceQuestionsAndReadsChoicesAndRefusals(t *testing.T) {
	var body, auth, path string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body, auth, path = string(b), r.Header.Get("x-api-key")+"|"+r.Header.Get("Authorization"), r.Method+" "+r.URL.Path
		io.WriteString(w, `{"request_id":"rq-9","model":"zoom-decisions","answers":[
			{"type":"choice","name":"asked_you","choice":"t1","confidence":0.9,
			 "probabilities":[{"value":"none","probability":0.06},{"value":"t1","probability":0.94}]},
			{"type":"refusal","name":"decision"},
			{"type":"score","name":"other","score":1},
			{"type":"choice","name":"late","choice":null,"probabilities":null}]}`)
	})
	got, err := c.Decide(context.Background(), decideReq)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"model":"zoom-decisions","reference_id":"ref-1","input":"NEW:\n[t1 00:01 S1] Alex, can you take it?",` +
		`"questions":[{"type":"choice","name":"asked_you","instructions":"Which NEW turn asks the listener?","choices":[{"value":"none"},{"value":"t1"}]},` +
		`{"type":"choice","name":"decision","instructions":"Which NEW turn is a decision?","choices":[{"value":"none"},{"value":"t1"}]}]}`
	if body != want || auth != "secret-key|" || path != "POST /v2/aiservices/decisions/decide" {
		t.Fatalf("sent %s, auth %q:\n%s\nwant\n%s", path, auth, body, want)
	}
	if got.RequestID != "rq-9" || len(got.Answers) != 3 {
		t.Fatalf("answers %+v: want the choice, the refusal and the empty choice, not the score", got)
	}
	if a := got.Answers[0]; a.Name != "asked_you" || a.Refused || a.Choice != "t1" || a.Probabilities["t1"] != 0.94 || a.Probabilities["none"] != 0.06 {
		t.Fatalf("choice answer %+v", a)
	}
	if a := got.Answers[1]; a.Name != "decision" || !a.Refused {
		t.Fatalf("refusal %+v", a)
	}
	if a := got.Answers[2]; a.Refused || a.Choice != "" || len(a.Probabilities) != 0 {
		t.Fatalf("empty choice %+v", a)
	}
}

func TestDecideWaitsOutA503AsTheServerAsks(t *testing.T) {
	var calls atomic.Int32
	var down atomic.Bool
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 || down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"code":"busy","metadata":{"retry_after_seconds":0.05}}`)
			return
		}
		io.WriteString(w, `{"answers":[{"type":"choice","name":"asked_you","choice":"none","probabilities":[{"value":"none","probability":1}]}]}`)
	})
	start := time.Now()
	got, err := c.Decide(context.Background(), decideReq)
	if err != nil || calls.Load() != 2 || len(got.Answers) != 1 {
		t.Fatalf("Decide = %+v, %v after %d calls", got, err, calls.Load())
	}
	if waited := time.Since(start); waited < 50*time.Millisecond {
		t.Fatalf("asked again after %v, before the server's 50ms", waited)
	}
	// Still unavailable after every attempt, it gives up with the status.
	calls.Store(0)
	down.Store(true)
	_, err = c.Decide(context.Background(), decideReq)
	var api *APIError
	if !errors.As(err, &api) || api.Status != http.StatusServiceUnavailable || calls.Load() != maxAttempts {
		t.Fatalf("err %v after %d calls", err, calls.Load())
	}
}

func TestDecideKeepsItsInputOutOfErrorsAndBoundsIt(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"code":"INVALID_INPUT","message":"bad input: Alex, can you take it?"}`)
	})
	_, err := c.Decide(context.Background(), decideReq)
	if err == nil || strings.Contains(err.Error(), "Alex") || calls.Load() != 1 {
		t.Fatalf("err %q after %d calls: want a 400 asked once, without the input", err, calls.Load())
	}
	long := decideReq
	long.Input = strings.Repeat("a", MaxDecideInput+1)
	if _, err := c.Decide(context.Background(), long); !errors.Is(err, ErrTextTooLong) || calls.Load() != 1 {
		t.Fatalf("long input: %v, %d calls", err, calls.Load())
	}
	one := decideReq
	one.Questions = []ChoiceQuestion{{Name: "q", Instructions: "i", Choices: []string{"none"}}}
	if _, err := c.Decide(context.Background(), one); err == nil || calls.Load() != 1 {
		t.Fatalf("a question of one choice: %v, %d calls", err, calls.Load())
	}
}
