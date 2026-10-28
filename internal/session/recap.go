package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/onyx-dot-app/Capy/internal/lang"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/zoomai"
)

// Summarizer input limits: the API takes 96 KB; chunks stay under 90 KB.
const (
	maxSummaryInput = 96 * 1024
	maxChunk        = 90 * 1024
	maxTitleRunes   = 80
)

// Untranslated reports whether seg awaits a translation it does not have:
// one that failed, or was cut short. A turn said with translation off
// awaits none.
func Untranslated(seg store.Segment) bool {
	return seg.Lang != store.GapLang && seg.Text != "" && seg.TranslationLang != "" && seg.Translation == ""
}

// RecapLanguage returns the language a session's recap is written in: the
// translation language when it translated and Summarizer writes it, else
// the language most of its turns were spoken in, else English.
func RecapLanguage(d store.SessionDetail) string {
	if d.Translated && lang.Summarizable(d.LangB) {
		return d.LangB
	}
	count := make(map[string]int)
	most := ""
	for _, seg := range d.Segments {
		if !lang.Summarizable(seg.Lang) {
			continue
		}
		count[seg.Lang]++
		if count[seg.Lang] > count[most] {
			most = seg.Lang
		}
	}
	switch {
	case most != "":
		return most
	case lang.Summarizable(d.LangA):
		return d.LangA
	}
	return "en-US"
}

// SpokenLanguages are the languages a session's turns were spoken in, in
// the order first heard, or its spoken language before any turn named one.
func SpokenLanguages(d store.SessionDetail) []string {
	var out []string
	for _, seg := range d.Segments {
		if seg.Lang != store.GapLang && seg.Lang != lang.Undetermined && !slices.Contains(out, seg.Lang) {
			out = append(out, seg.Lang)
		}
	}
	if len(out) == 0 {
		return []string{d.LangA}
	}
	return out
}

// ManySpeakers reports whether turns name more than one speaker: only then
// do their labels tell anything.
func ManySpeakers[T any](turns []T, speaker func(T) string) bool {
	first := ""
	for _, t := range turns {
		switch s := speaker(t); {
		case s == "":
		case first == "":
			first = s
		case s != first:
			return true
		}
	}
	return false
}

// SegmentSpeaker is who said seg, for ManySpeakers.
func SegmentSpeaker(seg store.Segment) string { return seg.Speaker }

// Recap summarizes a saved session and stores the result: status ready
// with the recap, or failed. It is used at the end of a session and to retry.
func Recap(ctx context.Context, deps Deps, sessionID string) error {
	detail, err := deps.Store.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	code := RecapLanguage(detail)
	text := Transcript(detail, code)
	if text == "" {
		// Nothing was said: there is nothing to summarize.
		return deps.Store.SetRecap(ctx, sessionID, store.Recap{Status: store.RecapReady})
	}
	recap, err := summarize(ctx, deps.AI, text, code)
	if err != nil {
		// Cancelled or not, the recap must not stay pending: it would read
		// "Writing the recap…" for good, with nothing to retry.
		if serr := deps.Store.SetRecap(context.WithoutCancel(ctx), sessionID, store.Recap{Status: store.RecapFailed}); serr != nil {
			return errors.Join(err, serr)
		}
		return fmt.Errorf("session: summarize: %w", err)
	}
	title := Title(recap)
	if detail.Source == store.SourceFile {
		title = FileTitle(detail.FileName) // as Transcribe titled it
	}
	return deps.Store.SetRecap(ctx, sessionID, store.Recap{Status: store.RecapReady, Title: title, Text: recap})
}

// Transcript is the session's text for a recap in language code: each turn
// in that language, from its translation when it was spoken in another, and
// after who said it when more than one spoke.
func Transcript(detail store.SessionDetail, code string) string {
	var b strings.Builder
	named := ManySpeakers(detail.Segments, SegmentSpeaker)
	for _, seg := range detail.Segments {
		if seg.Lang == store.GapLang || seg.Text == "" {
			continue
		}
		text := seg.Text
		if seg.Lang != code && seg.TranslationLang == code && seg.Translation != "" {
			text = seg.Translation
		}
		if named && seg.Speaker != "" {
			b.WriteString(seg.Speaker + ": ")
		}
		b.WriteString(text)
		b.WriteByte('\n')
	}
	return b.String()
}

// summarize writes a full summary of text, summarizing chunks first when it
// is too long for one request.
func summarize(ctx context.Context, ai *zoomai.Client, text, code string) (string, error) {
	for len(text) > maxSummaryInput {
		var parts []string
		for _, chunk := range Chunks(text, maxChunk) {
			s, err := summarizeTask(ctx, ai, chunk, zoomai.TaskSummary, code)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		joined := strings.Join(parts, "\n")
		if len(joined) >= len(text) {
			return "", errors.New("session: transcript does not shrink when summarized")
		}
		text = joined
	}
	return summarizeTask(ctx, ai, text, zoomai.TaskFullSummary, code)
}

// summarizeAttempts is how many times one summary is asked for when
// Summarizer fails in a way that may pass; the first retry waits
// summarizeRetryWait, each one after twice as long.
const summarizeAttempts = 3

var summarizeRetryWait = 2 * time.Second

// summarizeTask asks Summarizer for one summary, again when it failed in a
// way that may pass.
func summarizeTask(ctx context.Context, ai *zoomai.Client, text string, task zoomai.SummaryTask, code string) (string, error) {
	wait := summarizeRetryWait
	for attempt := 1; ; attempt++ {
		s, err := ai.Summarize(ctx, text, task, code)
		if err == nil || attempt == summarizeAttempts || ctx.Err() != nil || !mayPass(err) {
			return s, err
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return "", ctx.Err()
		case <-t.C:
		}
		wait *= 2
	}
}

// mayPass reports whether a Summarizer error may pass when asked again: a
// server error, a dropped connection or a reply without text, but not a
// rejected key, a request it refuses, or a rate limit the client already
// waited out.
func mayPass(err error) bool {
	var api *zoomai.APIError
	var limited *zoomai.RateLimitedError
	switch {
	case errors.Is(err, zoomai.ErrUnauthorized), errors.Is(err, zoomai.ErrTextTooLong), errors.As(err, &limited):
		return false
	case errors.As(err, &api):
		return api.Status >= 500
	}
	return true
}

// Chunks splits text at line ends into pieces of at most limit bytes. A
// single line longer than limit is split at a rune boundary.
func Chunks(text string, limit int) []string {
	var chunks []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			chunks = append(chunks, cur.String())
			cur.Reset()
		}
	}
	for line := range strings.SplitAfterSeq(text, "\n") {
		for len(line) > limit {
			flush()
			cut := limit
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			chunks = append(chunks, line[:cut])
			line = line[cut:]
		}
		if cur.Len()+len(line) > limit {
			flush()
		}
		cur.WriteString(line)
	}
	flush()
	return chunks
}

// Title is the first line of a recap with Markdown marks removed, shortened.
func Title(recap string) string {
	for line := range strings.Lines(recap) {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#*->• "))
		line = strings.TrimSpace(strings.Trim(line, "*_"))
		if line == "" {
			continue
		}
		if utf8.RuneCountInString(line) > maxTitleRunes {
			runes := []rune(line)
			line = strings.TrimSpace(string(runes[:maxTitleRunes-1])) + "…"
		}
		return line
	}
	return ""
}

// RetryTranslations translates the saved turns of a session that are
// Untranslated. It returns how many still failed.
func RetryTranslations(ctx context.Context, deps Deps, sessionID string) (int, error) {
	detail, err := deps.Store.GetSession(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	failed := 0
	for _, seg := range detail.Segments {
		if !Untranslated(seg) {
			continue
		}
		to := seg.TranslationLang
		text, err := TranslateText(ctx, deps.AI, seg.Text, seg.Lang, to)
		if err == nil {
			err = deps.Store.SetTranslation(ctx, seg.ID, to, text)
		}
		if err != nil {
			if errors.Is(err, zoomai.ErrUnauthorized) || ctx.Err() != nil {
				return failed, err
			}
			failed++
		}
	}
	return failed, nil
}

// RecoverInterrupted ends the sessions a quit or crash left open, as of
// their last saved turn, and marks failed every recap it left pending, of
// those and of sessions it stopped while their recap was being written,
// trashed or not, so the library offers to retry it. Call it at launch,
// before any session starts: no recap is being written then.
func RecoverInterrupted(ctx context.Context, st store.Store) error {
	list, err := st.ListSessions(ctx, store.ListQuery{Limit: store.MaxListLimit})
	if err != nil {
		return err
	}
	pending := func(s store.SessionSummary) error {
		if s.RecapStatus != store.RecapPending {
			return nil
		}
		return st.SetRecap(ctx, s.ID, store.Recap{Status: store.RecapFailed})
	}
	for _, s := range list {
		if s.EndedAt.IsZero() {
			d, err := st.GetSession(ctx, s.ID)
			if err != nil {
				return err
			}
			var lastMs int64
			for _, seg := range d.Segments {
				lastMs = max(lastMs, seg.EndMs)
			}
			if err := st.EndSession(ctx, s.ID, s.StartedAt.Add(time.Duration(lastMs)*time.Millisecond)); err != nil {
				return err
			}
		}
		if err := pending(s); err != nil {
			return err
		}
	}
	trashed, err := st.ListTrashed(ctx)
	if err != nil {
		return err
	}
	for _, t := range trashed {
		if t.PurgePending {
			continue // being erased: there is nothing to retry
		}
		if err := pending(t.SessionSummary); err != nil {
			return err
		}
	}
	return nil
}
