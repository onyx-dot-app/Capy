package polish

import (
	"encoding/json"
	"errors"
	"strings"
)

// maxAnswer bounds the answer read for its ops: far more than 50 ops.
const maxAnswer = 256 << 10

// errNoOps is an answer without a json block of ops that parses: the
// pass failed.
var errNoOps = errors.New("polish: the answer has no ops")

// op is one op of an answer, as the agent wrote it: what of it is used
// depends on Op, and apply checks every field.
type op struct {
	Op         string   `json:"op"`
	Turn       string   `json:"turn"`
	Text       string   `json:"text"`
	Owner      string   `json:"owner"`
	Due        string   `json:"due"`
	DecidedBy  string   `json:"decided_by"`
	Condition  string   `json:"condition"`
	Kind       string   `json:"kind"`
	Confidence *float64 `json:"confidence"`
	Into       string   `json:"into"`
	Anchor     string   `json:"anchor"`
	Topic      string   `json:"topic"`
	Sides      []side   `json:"sides"`
	Status     string   `json:"status"`
	Decision   string   `json:"decision"`
}

type side struct {
	Position string   `json:"position"`
	Speakers []string `json:"speakers"`
	Turns    []string `json:"turns"`
}

// parse reads the ops of answer: those of its last ```json block. An op
// that does not parse is left out and counted in bad; no block, or one
// that is not {"ops": [...]}, is errNoOps.
func parse(answer string) (ops []op, bad int, err error) {
	if len(answer) > maxAnswer {
		return nil, 0, errNoOps
	}
	const fence = "```json"
	i := strings.LastIndex(answer, fence)
	if i < 0 {
		return nil, 0, errNoOps
	}
	body, _, closed := strings.Cut(answer[i+len(fence):], "```")
	if !closed {
		return nil, 0, errNoOps
	}
	var doc struct {
		Ops *[]json.RawMessage `json:"ops"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil || doc.Ops == nil {
		return nil, 0, errNoOps
	}
	for _, raw := range *doc.Ops {
		var o op
		if err := json.Unmarshal(raw, &o); err != nil {
			bad++
			continue
		}
		ops = append(ops, o)
	}
	return ops, bad, nil
}
