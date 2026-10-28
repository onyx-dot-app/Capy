package zoomai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	decidePath = "/v2/aiservices/decisions/decide"

	// MaxDecideInput is the most input Capy sends Decisions in one
	// request. The API publishes no limit; this keeps a request quick.
	MaxDecideInput = 24 * 1024
	// MaxDecideQuestions and MaxDecideChoices are the API's limits.
	MaxDecideQuestions = 10
	MaxDecideChoices   = 255

	// DecisionsModel is the default model, for latency and cost.
	DecisionsModel = "zoom-decisions"
)

// ChoiceQuestion asks which of Choices the input supports; Decisions
// answers each question on its own.
type ChoiceQuestion struct {
	Name         string
	Instructions string
	Choices      []string
}

// MarshalJSON writes q as the API's choice question.
func (q ChoiceQuestion) MarshalJSON() ([]byte, error) {
	type choice struct {
		Value string `json:"value"`
	}
	choices := make([]choice, len(q.Choices))
	for i, v := range q.Choices {
		choices[i] = choice{Value: v}
	}
	return json.Marshal(struct {
		Type         string   `json:"type"`
		Name         string   `json:"name"`
		Instructions string   `json:"instructions"`
		Choices      []choice `json:"choices"`
	}{"choice", q.Name, q.Instructions, choices})
}

// DecideRequest is one call to Decisions: questions over one input.
type DecideRequest struct {
	Model string `json:"model,omitempty"`
	// ReferenceID is the caller's, for support to trace the request. It
	// must hold no user data.
	ReferenceID string           `json:"reference_id,omitempty"`
	Input       string           `json:"input"`
	Questions   []ChoiceQuestion `json:"questions"`
}

// Answer is Decisions' answer to one choice question: the choice it
// prefers and how likely it finds each one, or a refusal to answer.
type Answer struct {
	Name    string
	Refused bool
	Choice  string
	// Probabilities are by choice; a choice it did not rate is missing.
	Probabilities map[string]float64
}

// DecideResponse is Decisions' answers, by question.
type DecideResponse struct {
	// RequestID is Zoom's ID of the request, for its logs.
	RequestID string
	Answers   []Answer
}

type decideResponse struct {
	RequestID string `json:"request_id"`
	Answers   []struct {
		Type          string  `json:"type"`
		Name          string  `json:"name"`
		Choice        *string `json:"choice"`
		Probabilities []struct {
			Value       string  `json:"value"`
			Probability float64 `json:"probability"`
		} `json:"probabilities"`
	} `json:"answers"`
}

// Decide asks Decisions the request's choice questions. Input over
// MaxDecideInput is ErrTextTooLong, unsent. A 429 or a 503 is asked again
// after the wait the server gives, as many times as other calls are.
func (c *Client) Decide(ctx context.Context, req DecideRequest) (DecideResponse, error) {
	if strings.TrimSpace(req.Input) == "" || len(req.Questions) == 0 || len(req.Questions) > MaxDecideQuestions {
		return DecideResponse{}, errors.New("zoomai: input and 1 to 10 questions are required")
	}
	for _, q := range req.Questions {
		if q.Name == "" || q.Instructions == "" || len(q.Choices) < 2 || len(q.Choices) > MaxDecideChoices {
			return DecideResponse{}, errors.New("zoomai: a question needs a name, instructions and 2 to 255 choices")
		}
	}
	if len(req.Input) > MaxDecideInput {
		return DecideResponse{}, ErrTextTooLong
	}
	var resp decideResponse
	if err := c.postJSON(ctx, decidePath, req, &resp, true); err != nil {
		return DecideResponse{}, err
	}
	out := DecideResponse{RequestID: resp.RequestID}
	for _, a := range resp.Answers {
		switch a.Type {
		case "refusal":
			out.Answers = append(out.Answers, Answer{Name: a.Name, Refused: true})
		case "choice":
			ans := Answer{Name: a.Name, Probabilities: make(map[string]float64, len(a.Probabilities))}
			if a.Choice != nil {
				ans.Choice = *a.Choice
			}
			for _, p := range a.Probabilities {
				ans.Probabilities[p.Value] = p.Probability
			}
			out.Answers = append(out.Answers, ans)
		}
	}
	return out, nil
}

// unavailableRetryAfter reads how long a 503's body asks to wait,
// metadata.retry_after_seconds, defaulting to one second and capping at
// thirty.
func unavailableRetryAfter(data []byte) time.Duration {
	var body struct {
		Metadata struct {
			RetryAfterSeconds float64 `json:"retry_after_seconds"`
		} `json:"metadata"`
	}
	if json.Unmarshal(data, &body) != nil || body.Metadata.RetryAfterSeconds <= 0 {
		return defaultRetryAfter
	}
	return min(time.Duration(body.Metadata.RetryAfterSeconds*float64(time.Second)), maxRetryAfter)
}
