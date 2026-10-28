package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/onyx-dot-app/Capy/internal/keychain"
	"github.com/onyx-dot-app/Capy/internal/profile"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

// The quality eval's settings: the thresholds it scores, the models it
// asks, and the gate a kind must pass at Fewer to ship on.
var (
	evalThetas = []float64{0.70, 0.75, 0.80, 0.85, 0.90}
	evalModels = []string{zoomai.DecisionsModel, "zoom-decisions-pro"}
)

const (
	gatePrecisionForYou = 0.90
	gatePrecisionOther  = 0.85
	gateWrongPerHour    = 1.0
	maxEvalFile         = 4 << 20
)

// evalMeeting is a labeled transcript: who listens, its turns, and the
// turns that should be marked, by index. A quiet meeting should get no
// mark at all.
type evalMeeting struct {
	Name     string          `json:"name"`
	Listener profile.Profile `json:"listener"`
	Quiet    bool            `json:"quiet"`
	Turns    []struct {
		Ms      int64  `json:"ms"`
		Speaker string `json:"speaker"`
		Text    string `json:"text"`
	} `json:"turns"`
	Labels []struct {
		Turn int                 `json:"turn"`
		Kind store.HighlightKind `json:"kind"`
	} `json:"labels"`
}

// evalReport is the eval's artifact: per model and θ, each kind's
// precision, recall and wrong marks per hour, the marks on quiet
// meetings, and whether each kind passes the gate at Fewer.
type evalReport struct {
	Meetings []string      `json:"meetings"`
	Hours    float64       `json:"hours"`
	Models   []evalModel   `json:"models"`
	Checks   []reportCheck `json:"checks"`
	Verdict  string        `json:"verdict"`
}

type evalModel struct {
	Model  string      `json:"model"`
	Thetas []evalTheta `json:"thetas"`
}

type evalTheta struct {
	Theta      float64             `json:"theta"`
	Kinds      map[string]evalKind `json:"kinds"`
	QuietMarks int                 `json:"quietMarks"`
}

type evalKind struct {
	Marked       int     `json:"marked"`
	Right        int     `json:"right"`
	Labeled      int     `json:"labeled"`
	Precision    float64 `json:"precision"`
	Recall       float64 `json:"recall"`
	WrongPerHour float64 `json:"wrongPerHour"`
}

// runHighlightsEval replays labeled meetings through Decisions and writes
// the report; the exit code is 0 when every kind passes the gate with
// the default model.
func runHighlightsEval(files []string, reportPath string, log *slog.Logger) int {
	r, err := highlightsEval(files, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "capy:", err)
		return 2
	}
	return emit(reportPath, r, r.Checks, r.Verdict)
}

func highlightsEval(files []string, log *slog.Logger) (evalReport, error) {
	if len(files) == 0 {
		return evalReport{}, errors.New("-highlights-eval needs the labeled meetings, as files after the flags")
	}
	var meetings []evalMeeting
	for _, f := range files {
		m, err := readMeeting(f)
		if err != nil {
			return evalReport{}, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		meetings = append(meetings, m)
	}
	key, err := keychain.APIKey()
	if err != nil {
		return evalReport{}, fmt.Errorf("set $%s or store a key in the app: %w", keychain.EnvKey, err)
	}
	var opts []zoomai.Option
	if u := os.Getenv(baseURLEnv); u != "" {
		opts = append(opts, zoomai.WithBaseURL(u))
	}
	ai, err := zoomai.NewClient(key, opts...)
	if err != nil {
		return evalReport{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	r := evalReport{}
	for _, m := range meetings {
		r.Meetings = append(r.Meetings, m.Name)
		r.Hours += m.hours()
	}
	for _, model := range evalModels {
		em := evalModel{Model: model}
		// Each request is asked once; the thresholds score its answers.
		answers := make([][]zoomai.DecideResponse, len(meetings))
		for i, m := range meetings {
			for _, req := range session.EvalRequests(m.lines(), m.config(evalThetas[0]), "") {
				req.Req.Model = model
				resp, err := ai.Decide(ctx, req.Req)
				if err != nil {
					return evalReport{}, fmt.Errorf("%s with %s: %w", m.Name, model, err)
				}
				answers[i] = append(answers[i], resp)
			}
			log.Info("highlights eval", "meeting", i, "model", model, "requests", len(answers[i]))
		}
		for _, theta := range evalThetas {
			em.Thetas = append(em.Thetas, scoreMeetings(meetings, answers, theta, r.Hours))
		}
		r.Models = append(r.Models, em)
	}
	r.Checks = gateChecks(r.Models[0])
	r.Verdict = verdict(r.Checks)
	return r, nil
}

func readMeeting(path string) (evalMeeting, error) {
	f, err := os.Open(path)
	if err != nil {
		return evalMeeting{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxEvalFile+1))
	if err != nil {
		return evalMeeting{}, err
	}
	var m evalMeeting
	if len(b) > maxEvalFile || json.Unmarshal(b, &m) != nil || len(m.Turns) == 0 {
		return evalMeeting{}, errors.New("not a labeled meeting")
	}
	for _, l := range m.Labels {
		if l.Turn < 0 || l.Turn >= len(m.Turns) || !slices.Contains(store.HighlightKinds, l.Kind) {
			return evalMeeting{}, fmt.Errorf("label of turn %d: no such turn or kind", l.Turn)
		}
	}
	return m, nil
}

func (m evalMeeting) lines() []session.Line {
	out := make([]session.Line, len(m.Turns))
	for i, t := range m.Turns {
		out[i] = session.Line{Seq: i, StartMs: t.Ms, Lang: "en-US", Text: t.Text, Speaker: t.Speaker}
	}
	return out
}

func (m evalMeeting) config(theta float64) session.HighlightConfig {
	return session.HighlightConfig{On: true, Kinds: store.HighlightKinds, Threshold: theta, Profile: m.Listener}
}

// hours is how long the meeting ran, to its last turn.
func (m evalMeeting) hours() float64 {
	return float64(m.Turns[len(m.Turns)-1].Ms) / float64(time.Hour.Milliseconds())
}

// scoreMeetings marks each meeting at theta from its answers, as a
// recording would, and scores the marks against the labels.
func scoreMeetings(meetings []evalMeeting, answers [][]zoomai.DecideResponse, theta, hours float64) evalTheta {
	out := evalTheta{Theta: theta, Kinds: map[string]evalKind{}}
	for i, m := range meetings {
		lines, cfg := m.lines(), m.config(theta)
		labeled := map[int]store.HighlightKind{}
		for _, l := range m.Labels {
			labeled[l.Turn] = l.Kind
		}
		for _, l := range m.Labels {
			k := out.Kinds[string(l.Kind)]
			k.Labeled++
			out.Kinds[string(l.Kind)] = k
		}
		var marks []store.Highlight
		for j, req := range session.EvalRequests(lines, cfg, "") {
			marks = append(marks, session.EvalMarks(answers[i][j], req, cfg, marks, lines)...)
		}
		for _, h := range marks {
			k := out.Kinds[string(h.Kind)]
			k.Marked++
			if labeled[h.Seq] == h.Kind {
				k.Right++
			}
			out.Kinds[string(h.Kind)] = k
		}
		if m.Quiet {
			out.QuietMarks += len(marks)
		}
	}
	for name, k := range out.Kinds {
		k.Precision = ratio(k.Right, k.Marked)
		k.Recall = ratio(k.Right, k.Labeled)
		if hours > 0 {
			k.WrongPerHour = round(float64(k.Marked-k.Right) / hours)
		}
		out.Kinds[name] = k
	}
	return out
}

// ratio is a/b to three places, 1 when b is 0: no mark made is no wrong
// mark.
func ratio(a, b int) float64 {
	if b == 0 {
		return 1
	}
	return round(float64(a) / float64(b))
}

func round(f float64) float64 { return math.Round(f*1000) / 1000 }

// gateChecks are the gate at Fewer for each kind with the default model:
// precision of 0.90 for the user's kinds and 0.85 for the others, at most
// one wrong mark an hour, and no mark on a quiet meeting.
func gateChecks(m evalModel) []reportCheck {
	i := slices.IndexFunc(m.Thetas, func(t evalTheta) bool { return t.Theta == session.ThresholdFewer })
	fewer := m.Thetas[i]
	var checks []reportCheck
	for _, kind := range store.HighlightKinds {
		k := fewer.Kinds[string(kind)]
		want := gatePrecisionOther
		if kind.ForYou() {
			want = gatePrecisionForYou
		}
		checks = append(checks, reportCheck{Name: string(kind), Pass: k.Precision >= want && k.WrongPerHour <= gateWrongPerHour,
			Detail: fmt.Sprintf("precision %.3f (≥ %.2f), %.2f wrong an hour, recall %.3f", k.Precision, want, k.WrongPerHour, k.Recall)})
	}
	checks = append(checks, reportCheck{Name: "quiet meetings", Pass: fewer.QuietMarks == 0, Detail: fmt.Sprintf("%d marks", fewer.QuietMarks)})
	return checks
}
