package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/onyx-dot-app/Capy/internal/agent"
	"github.com/onyx-dot-app/Capy/internal/audio"
	"github.com/onyx-dot-app/Capy/internal/keychain"
	"github.com/onyx-dot-app/Capy/internal/profile"
	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/store/sqlite"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

type replayOptions struct {
	wav, langA, langB, report, db string
	realtime, translate           bool
	ask                           askOptions
	highlights                    highlightOptions
	// polish polishes the highlights with the -agent, live and at Stop.
	polish bool
}

// highlightOptions turn Highlights on for a replay: the listener's names
// and how often it checks.
type highlightOptions struct {
	on       bool
	listener string
	also     string // the listener's other names, comma-separated
	every    time.Duration
}

// config is the session's Highlights config: every kind, How many at
// Fewer.
func (o highlightOptions) config() session.HighlightConfig {
	p := profile.Profile{Name: o.listener}
	for a := range strings.SplitSeq(o.also, ",") {
		if a = strings.TrimSpace(a); a != "" {
			p.Aliases = append(p.Aliases, a)
		}
	}
	return session.HighlightConfig{On: o.on, Every: o.every, Kinds: store.HighlightKinds, Threshold: session.ThresholdFewer, Profile: p}
}

// report is the replay's artifact: what was saved, without IDs, times or
// hosts, so the same inputs give the same file, and a verdict per check.
type report struct {
	LangA     string       `json:"langA"`
	LangB     string       `json:"langB"`
	Translate bool         `json:"translate"`
	Status    string       `json:"status"`
	Error     string       `json:"error,omitempty"`
	Segments  []reportLine `json:"segments"`
	Recap     reportRecap  `json:"recap"`
	// Highlights are the turns Highlights marked, with -highlights.
	Highlights []reportHighlight `json:"highlights,omitempty"`
	Ask        *reportAsk        `json:"ask,omitempty"`
	Checks     []reportCheck     `json:"checks"`
	Verdict    string            `json:"verdict"`
}

type reportLine struct {
	Lang            string `json:"lang"`
	Text            string `json:"text"`
	Speaker         string `json:"speaker,omitempty"`
	Translation     string `json:"translation"`
	TranslationLang string `json:"translationLang"`
}

// reportHighlight is a marked turn: its kind, seq and time in the
// session, and its polish, with -polish.
type reportHighlight struct {
	Kind   string        `json:"kind"`
	Seq    int           `json:"seq"`
	Time   string        `json:"time"`
	Polish *reportPolish `json:"polish,omitempty"`
}

// reportPolish is what the agent wrote of a highlight, the highlights it
// names by the times of their turns.
type reportPolish struct {
	By         string        `json:"by"`
	Text       string        `json:"text,omitempty"`
	Kind       string        `json:"kind,omitempty"`
	Owner      string        `json:"owner,omitempty"`
	Due        string        `json:"due,omitempty"`
	DecidedBy  string        `json:"decidedBy,omitempty"`
	Condition  string        `json:"condition,omitempty"`
	Tentative  bool          `json:"tentative,omitempty"`
	FoldedInto string        `json:"foldedInto,omitempty"`
	Debate     *store.Debate `json:"debate,omitempty"`
}

type reportRecap struct {
	Status string `json:"status"`
	Title  string `json:"title"`
	Text   string `json:"text"`
}

type reportCheck struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// runReplay runs one session from a WAV file and returns the exit code: 0
// when every check passed.
func runReplay(o replayOptions, log *slog.Logger) int {
	r, err := replay(o, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "capy:", err)
		return 2
	}
	return emit(o.report, r, r.Checks, r.Verdict)
}

// emit writes report r to path, unless path is empty, prints its checks
// and verdict, and returns the exit code: 0 when every check passed.
func emit(path string, r any, checks []reportCheck, verdict string) int {
	if path != "" {
		b, _ := json.MarshalIndent(r, "", "  ")
		// The report holds transcripts: it is the user's alone.
		if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "capy:", err)
			return 2
		}
	}
	for _, c := range checks {
		mark := "PASS"
		if !c.Pass {
			mark = "FAIL"
		}
		fmt.Printf("%s  %s %s\n", mark, c.Name, c.Detail)
	}
	fmt.Println(verdict)
	if verdict != "pass" {
		return 1
	}
	return 0
}

func replay(o replayOptions, log *slog.Logger) (report, error) {
	if o.ask.question != "" && o.ask.agent == "" {
		return report{}, errors.New("-ask needs -agent: the agent the replay may send the question to")
	}
	if o.ask.question != "" && o.db != "" {
		// Agent's data sits beside the library: -ask must not touch the app's.
		return report{}, errors.New("-ask runs on a library of its own: drop -db")
	}
	if o.polish && (!o.highlights.on || o.ask.agent == "" || o.db != "") {
		return report{}, errors.New("-polish needs -highlights and -agent, and a library of its own: drop -db")
	}
	key, err := keychain.APIKey()
	if err != nil {
		return report{}, fmt.Errorf("set $%s or store a key in the app: %w", keychain.EnvKey, err)
	}
	var opts []zoomai.Option
	if u := os.Getenv(baseURLEnv); u != "" {
		opts = append(opts, zoomai.WithBaseURL(u))
	}
	ai, err := zoomai.NewClient(key, opts...)
	if err != nil {
		return report{}, err
	}
	dbPath := o.db
	if dbPath == "" {
		dir, err := os.MkdirTemp("", "capy-replay-")
		if err != nil {
			return report{}, err
		}
		defer os.RemoveAll(dir)
		dbPath = filepath.Join(dir, "capy.db")
	}
	ctx := context.Background()
	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		return report{}, err
	}
	defer db.Close()

	var ag *agent.Module
	if o.ask.question != "" || o.polish {
		if ag, err = newAgent(ctx, o.ask, o.polish, db, filepath.Dir(dbPath), log); err != nil {
			return report{}, err
		}
		defer ag.Close()
	}
	deps := session.Deps{AI: ai, Store: db, Log: log}
	if o.polish {
		deps.Highlighted = ag.Highlighted
	}

	cfg := session.Config{Source: store.SourceReplay, LangA: o.langA, LangB: o.langB, Translate: o.translate,
		Highlights: o.highlights.config()}
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	changed := make(chan struct{}, 1)
	onChange := func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	run, err := session.Start(startCtx, deps, cfg, audio.NewWAV(o.wav, o.realtime), onChange)
	if err != nil {
		return report{}, err
	}
	// Asked while recording, the answer may end after the session.
	var live chan *reportAsk
	if ag != nil && o.ask.at > 0 {
		live = make(chan *reportAsk, 1)
		go func() {
			r, err := askAt(ctx, ag, db, o.ask, run, changed)
			if err != nil {
				r = failedAsk(o.ask, err)
			}
			live <- r
		}()
	}
	<-run.Done()
	snap := run.Snapshot()
	var asked *reportAsk
	if o.polish {
		ag.WaitPolish() // its final pass, after the session's last check
	}
	switch {
	case live != nil:
		asked = <-live
	case o.ask.question != "":
		if asked, err = askNow(ctx, ag, db, o.ask, snap.SessionID, false); err != nil {
			asked = failedAsk(o.ask, err)
		}
	}
	detail, err := db.GetSession(ctx, snap.SessionID)
	if err != nil {
		return report{}, err
	}
	r := buildReport(o, snap, detail)
	if asked != nil {
		r.Ask = asked
		r.Checks = append(r.Checks, asked.checks...)
		r.Verdict = verdict(r.Checks)
	}
	return r, nil
}

func buildReport(o replayOptions, snap session.Snapshot, d store.SessionDetail) report {
	r := report{
		LangA: o.langA, LangB: o.langB, Translate: o.translate, Status: string(snap.Status),
		Segments: []reportLine{},
		Recap:    reportRecap{Status: string(d.RecapStatus), Title: d.Title, Text: d.Recap},
	}
	if snap.Err != nil {
		r.Error = snap.Err.Error()
	}
	translated, untranslated := 0, 0
	speech := 0
	for _, s := range d.Segments {
		r.Segments = append(r.Segments, reportLine{Lang: s.Lang, Text: s.Text, Speaker: s.Speaker, Translation: s.Translation, TranslationLang: s.TranslationLang})
		if s.Lang == store.GapLang {
			continue
		}
		speech++
		switch {
		case s.Translation != "":
			translated++
		case session.Untranslated(s):
			untranslated++
		}
	}
	check := func(name string, pass bool, detail string) {
		r.Checks = append(r.Checks, reportCheck{Name: name, Pass: pass, Detail: detail})
	}
	check("transcribed", speech > 0, fmt.Sprintf("%d turns", speech))
	// Translating, at least one turn must be: else turning it on was lost.
	check("translated", speech > 0 && untranslated == 0 && (!o.translate || translated > 0),
		fmt.Sprintf("%d of %d turns", translated, translated+untranslated))
	check("recap", d.RecapStatus == store.RecapReady && d.Recap != "", string(d.RecapStatus))
	if o.highlights.on {
		starts := map[int]int64{}
		for _, s := range d.Segments {
			starts[s.Seq] = s.StartMs
		}
		timeOf := func(seq int) string {
			at := time.Duration(starts[seq]) * time.Millisecond
			return fmt.Sprintf("%02d:%02d", int(at.Minutes()), int(at.Seconds())%60)
		}
		seqs := map[string]int{}
		for _, h := range d.Highlights {
			seqs[h.ID] = h.Seq
		}
		marked, polished := 0, 0
		for _, h := range d.Highlights {
			rh := reportHighlight{Kind: string(h.Kind), Seq: h.Seq, Time: timeOf(h.Seq), Polish: polishReport(h, seqs, timeOf)}
			r.Highlights = append(r.Highlights, rh)
			if !h.Dismissed {
				marked++
				if h.Polish != nil {
					polished++
				}
			}
		}
		detail := fmt.Sprintf("%d marked", marked)
		if snap.HighlightsErr != nil {
			detail = snap.HighlightsErr.Error()
		}
		check("highlights", snap.HighlightsErr == nil, detail)
		if o.polish {
			check("polished", marked > 0 && polished == marked, fmt.Sprintf("%d of %d", polished, marked))
		}
	}
	r.Verdict = verdict(r.Checks)
	return r
}

// polishReport is h's polish as the report has it, nil for none: the
// highlights it names, by seqs, at their turns' times, by timeOf.
func polishReport(h store.Highlight, seqs map[string]int, timeOf func(seq int) string) *reportPolish {
	p := h.Polish
	if p == nil {
		return nil
	}
	r := &reportPolish{By: p.ByAgent, Text: p.Text, Kind: string(p.Kind), Owner: p.Owner, Due: p.Due,
		DecidedBy: p.DecidedBy, Condition: p.Condition, Tentative: p.Tentative()}
	if seq, ok := seqs[p.FoldedInto]; ok {
		r.FoldedInto = timeOf(seq)
	}
	if p.Debate != nil {
		d := *p.Debate
		if seq, ok := seqs[d.Decision]; ok {
			d.Decision = timeOf(seq) // an ID is not the same twice
		}
		r.Debate = &d
	}
	return r
}

// verdict is "pass" when every check passed.
func verdict(checks []reportCheck) string {
	for _, c := range checks {
		if !c.Pass {
			return "fail"
		}
	}
	return "pass"
}
