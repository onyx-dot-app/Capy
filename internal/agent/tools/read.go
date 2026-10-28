package tools

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/onyx-dot-app/Capy/internal/lang"
	"github.com/onyx-dot-app/Capy/internal/store"
)

// Bounds of the read tools.
const (
	maxSessions   = 100       // list_sessions
	maxHits       = 50        // search
	pageTurns     = 200       // get_session, get_live_session
	pageBytes     = 256 << 10 // of a page's text, so that long turns make shorter pages
	snippetRunes  = 240
	listPageLimit = 200 // sessions read from the library at a time
)

type listIn struct {
	From     string `json:"from,omitempty" jsonschema:"only sessions started on or after this date (YYYY-MM-DD) or RFC 3339 time"`
	To       string `json:"to,omitempty" jsonschema:"only sessions started on or before this date (YYYY-MM-DD, the whole day) or RFC 3339 time"`
	Query    string `json:"query,omitempty" jsonschema:"only sessions whose transcript contains all these words"`
	LiveOnly bool   `json:"live_only,omitempty" jsonschema:"only the session being recorded"`
	Limit    int    `json:"limit,omitempty" jsonschema:"at most this many sessions, newest first (default 20, at most 100)"`
}

// sessionInfo is a session as the tools list it.
type sessionInfo struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Started     string   `json:"started"`
	DurationSec int      `json:"duration_seconds"`
	Languages   []string `json:"languages"`
	Speakers    []string `json:"speakers,omitempty"`
	RecapStatus string   `json:"recap_status"`
	Live        bool     `json:"live"`
}

type listOut struct {
	Sessions []sessionInfo `json:"sessions"`
}

type searchIn struct {
	Query string `json:"query" jsonschema:"words that must all occur in one turn, its text or its translation"`
	From  string `json:"from,omitempty" jsonschema:"only sessions started on or after this date (YYYY-MM-DD) or RFC 3339 time"`
	To    string `json:"to,omitempty" jsonschema:"only sessions started on or before this date (YYYY-MM-DD, the whole day) or RFC 3339 time"`
	Limit int    `json:"limit,omitempty" jsonschema:"at most this many turns (default 20, at most 50)"`
}

type hit struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
	At        string `json:"at"`
	AtMs      int64  `json:"at_ms"`
	Speaker   string `json:"speaker,omitempty"`
	Snippet   string `json:"snippet"`
}

type searchOut struct {
	Hits []hit `json:"hits"`
}

type getIn struct {
	ID      string `json:"id" jsonschema:"the session's id"`
	Part    string `json:"part,omitempty" jsonschema:"recap, transcript or all (default all)"`
	AfterMs int64  `json:"after_ms,omitempty" jsonschema:"only turns starting after this many milliseconds into the session"`
	Cursor  string `json:"cursor,omitempty" jsonschema:"the next_cursor of the previous page"`
}

type getOut struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Started     string `json:"started"`
	RecapStatus string `json:"recap_status"`
	Recap       string `json:"recap,omitempty"`
	// Transcript is one turn a line, "[mm:ss] Speaker: text", its
	// translation indented under it.
	Transcript string `json:"transcript,omitempty"`
	Turns      int    `json:"turns"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type liveIn struct {
	SinceMs int64 `json:"since_ms,omitempty" jsonschema:"only turns starting this many milliseconds into the session or later"`
}

type liveOut struct {
	Recording  bool   `json:"recording"`
	Message    string `json:"message,omitempty"`
	ID         string `json:"id,omitempty"`
	Title      string `json:"title,omitempty"`
	ElapsedSec int    `json:"elapsed_seconds,omitempty"`
	Transcript string `json:"transcript,omitempty"`
	Turns      int    `json:"turns"`
	// Omitted is how many earlier turns since since_ms are left out: the
	// latest pageTurns are given.
	Omitted int `json:"omitted,omitempty"`
}

type profileOut = Profile

func (t *tools) addRead(s *mcp.Server) {
	add(t, s, &mcp.Tool{Name: "list_sessions", Annotations: readOnly("List sessions"),
		Description: "Lists the user's recorded sessions, newest first: id, title, start, length, languages, speakers, recap status and whether it is being recorded."},
		t.listSessions)
	add(t, s, &mcp.Tool{Name: "search", Annotations: readOnly("Search transcripts"),
		Description: "Finds the turns of any session whose text or translation contains all the words, with the session, the turn's time and a snippet."},
		t.search)
	add(t, s, &mcp.Tool{Name: "get_session", Annotations: readOnly("Read a session"),
		Description: "Reads a session's recap and transcript. The transcript comes " + strconv.Itoa(pageTurns) + " turns a page: pass next_cursor back for the next."},
		t.getSession)
	add(t, s, &mcp.Tool{Name: "get_live_session", Annotations: readOnly("Read the live session"),
		Description: "Reads the session being recorded now: what has been said so far, the sentence being spoken excepted. Says when nothing is being recorded."},
		t.getLiveSession)
	add(t, s, &mcp.Tool{Name: "get_profile", Annotations: readOnly("Read the user's name"),
		Description: "The user's name and the other names they are called by, to tell whether they were mentioned."},
		func(context.Context, struct{}) (profileOut, error) { return t.o.Profile(), nil })
}

func (t *tools) listSessions(ctx context.Context, in listIn) (listOut, error) {
	lo, hi, err := timeRange(in.From, in.To)
	if err != nil {
		return listOut{}, err
	}
	if err := store.ValidateSearchQuery(in.Query); err != nil {
		return listOut{}, err
	}
	limit := clamp(in.Limit, 20, maxSessions)
	keep := func(s store.SessionSummary) bool {
		return within(s.StartedAt, lo, hi) && (!in.LiveOnly || t.live(s))
	}
	var found []store.SessionSummary
	if q := strings.TrimSpace(in.Query); q != "" {
		list, err := t.o.Store.SearchRange(ctx, q, lo, hi, store.MaxSearchLimit)
		if err != nil {
			return listOut{}, toolError(err)
		}
		found = slices.DeleteFunc(list, func(s store.SessionSummary) bool { return !keep(s) })
	} else {
		found, err = t.page(ctx, lo, keep, limit)
		if err != nil {
			return listOut{}, err
		}
	}
	out := listOut{Sessions: []sessionInfo{}}
	for _, s := range found[:min(len(found), limit)] {
		d, err := t.o.Store.GetSession(ctx, s.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue // trashed since it was listed
		}
		if err != nil {
			return listOut{}, toolError(err)
		}
		out.Sessions = append(out.Sessions, t.info(d))
	}
	return out, nil
}

// page lists sessions newest first, kept by keep, until limit are kept or
// they start before lo.
func (t *tools) page(ctx context.Context, lo time.Time, keep func(store.SessionSummary) bool, limit int) ([]store.SessionSummary, error) {
	var out []store.SessionSummary
	for offset := 0; len(out) < limit; offset += listPageLimit {
		list, err := t.o.Store.ListSessions(ctx, store.ListQuery{Limit: listPageLimit, Offset: offset})
		if err != nil {
			return nil, toolError(err)
		}
		for _, s := range list {
			if !lo.IsZero() && s.StartedAt.Before(lo) {
				return out, nil
			}
			if keep(s) {
				out = append(out, s)
			}
		}
		if len(list) < listPageLimit {
			break
		}
	}
	return out, nil
}

// live reports whether s is being recorded: not ended, its heartbeat fresh.
func (t *tools) live(s store.SessionSummary) bool {
	return s.EndedAt.IsZero() && !s.HeartbeatAt.IsZero() && t.o.Now().Sub(s.HeartbeatAt) < liveWithin
}

// info describes session d for the tools.
func (t *tools) info(d store.SessionDetail) sessionInfo {
	live := t.live(d.SessionSummary)
	end := d.EndedAt
	if end.IsZero() {
		end = t.o.Now()
	}
	var speakers []string
	langs := map[string]bool{}
	for _, seg := range d.Segments {
		if seg.Lang == store.GapLang {
			continue
		}
		langs[seg.Lang] = true
		if seg.TranslationLang != "" {
			langs[seg.TranslationLang] = true
		}
		if seg.Speaker != "" && !slices.Contains(speakers, seg.Speaker) {
			speakers = append(speakers, seg.Speaker)
		}
	}
	if len(langs) == 0 {
		langs[d.LangA] = true
	}
	names := []string{}
	for code := range langs {
		names = append(names, langName(code))
	}
	slices.Sort(names)
	return sessionInfo{ID: d.ID, Title: d.Title, Started: d.StartedAt.Local().Format(time.RFC3339),
		DurationSec: int(max(end.Sub(d.StartedAt), 0) / time.Second), Languages: names, Speakers: speakers,
		RecapStatus: string(d.RecapStatus), Live: live}
}

func langName(code string) string {
	if l, ok := lang.Lookup(code); ok {
		return l.Name
	}
	return code
}

func (t *tools) search(ctx context.Context, in searchIn) (searchOut, error) {
	if err := store.ValidateSearchQuery(in.Query); err != nil {
		return searchOut{}, err
	}
	words := strings.Fields(strings.ToLower(in.Query))
	if len(words) == 0 {
		return searchOut{}, errors.New("query must have a word")
	}
	lo, hi, err := timeRange(in.From, in.To)
	if err != nil {
		return searchOut{}, err
	}
	limit := clamp(in.Limit, 20, maxHits)
	sessions, err := t.o.Store.SearchRange(ctx, in.Query, lo, hi, store.MaxSearchLimit)
	if err != nil {
		return searchOut{}, toolError(err)
	}
	out := searchOut{Hits: []hit{}}
	for _, s := range sessions {
		d, err := t.o.Store.GetSession(ctx, s.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return searchOut{}, toolError(err)
		}
		for _, seg := range d.Segments {
			if seg.Lang == store.GapLang || !hasAll(seg, words) {
				continue
			}
			snippet := seg.Text
			if seg.Translation != "" {
				snippet += " / " + seg.Translation
			}
			out.Hits = append(out.Hits, hit{SessionID: d.ID, Title: d.Title, At: clock(seg.StartMs), AtMs: seg.StartMs,
				Speaker: seg.Speaker, Snippet: short(snippet, snippetRunes)})
			if len(out.Hits) == limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// hasAll reports whether every word occurs in the turn's text or in its
// translation, as the library's search matches.
func hasAll(seg store.Segment, words []string) bool {
	text, translation := strings.ToLower(seg.Text), strings.ToLower(seg.Translation)
	for _, w := range words {
		if !strings.Contains(text, w) && !strings.Contains(translation, w) {
			return false
		}
	}
	return true
}

func (t *tools) getSession(ctx context.Context, in getIn) (getOut, error) {
	if err := validID(in.ID); err != nil {
		return getOut{}, err
	}
	part := in.Part
	switch part {
	case "":
		part = "all"
	case "all", "recap", "transcript":
	default:
		return getOut{}, errors.New("part must be recap, transcript or all")
	}
	if in.AfterMs < 0 {
		return getOut{}, errors.New("after_ms must not be negative")
	}
	from, err := decodeCursor(in.Cursor)
	if err != nil {
		return getOut{}, err
	}
	d, err := t.o.Store.GetSession(ctx, in.ID)
	if err != nil {
		return getOut{}, toolError(err)
	}
	out := getOut{ID: d.ID, Title: d.Title, Started: d.StartedAt.Local().Format(time.RFC3339), RecapStatus: string(d.RecapStatus)}
	if part != "transcript" && in.Cursor == "" {
		out.Recap = d.Recap
	}
	if part == "recap" {
		return out, nil
	}
	turns := slices.DeleteFunc(slices.Clone(d.Segments), func(seg store.Segment) bool {
		return seg.Seq < from || seg.StartMs <= in.AfterMs && in.AfterMs > 0
	})
	if n := pageLen(turns); n < len(turns) {
		out.NextCursor = encodeCursor(turns[n].Seq)
		turns = turns[:n]
	}
	out.Transcript, out.Turns = transcript(turns, manySpeakers(d.Segments)), len(turns)
	return out, nil
}

func (t *tools) getLiveSession(ctx context.Context, in liveIn) (liveOut, error) {
	if in.SinceMs < 0 {
		return liveOut{}, errors.New("since_ms must not be negative")
	}
	list, err := t.o.Store.ListSessions(ctx, store.ListQuery{Limit: 10})
	if err != nil {
		return liveOut{}, toolError(err)
	}
	i := slices.IndexFunc(list, t.live)
	if i < 0 {
		return liveOut{Message: "Capy is not recording."}, nil
	}
	d, err := t.o.Store.GetSession(ctx, list[i].ID)
	if err != nil {
		return liveOut{}, toolError(err)
	}
	turns := slices.DeleteFunc(slices.Clone(d.Segments), func(seg store.Segment) bool { return seg.StartMs < in.SinceMs })
	// The latest page: the turns read backwards, as a page reads forwards.
	slices.Reverse(turns)
	n := pageLen(turns)
	omitted := len(turns) - n
	turns = turns[:n]
	slices.Reverse(turns)
	return liveOut{Recording: true, ID: d.ID, Title: d.Title,
		ElapsedSec: int(max(t.o.Now().Sub(d.StartedAt), 0) / time.Second),
		Transcript: transcript(turns, manySpeakers(d.Segments)), Turns: len(turns), Omitted: omitted}, nil
}

// pageLen is how many of turns make a page: at most pageTurns, and no
// more text than pageBytes, but always the first.
func pageLen(turns []store.Segment) int {
	size := 0
	for n, seg := range turns {
		size += len(seg.Text) + len(seg.Translation)
		if n == pageTurns || n > 0 && size > pageBytes {
			return n
		}
	}
	return len(turns)
}

// transcript sets turns one a line, "[mm:ss] Speaker: text", with the
// translation indented under it; speakers only when the session names
// more than one.
func transcript(turns []store.Segment, named bool) string {
	var b strings.Builder
	for _, seg := range turns {
		b.WriteString("[" + clock(seg.StartMs) + "] ")
		if seg.Lang == store.GapLang {
			b.WriteString("(connection lost)\n")
			continue
		}
		if named && seg.Speaker != "" {
			b.WriteString(seg.Speaker + ": ")
		}
		b.WriteString(seg.Text + "\n")
		if seg.Translation != "" {
			b.WriteString("    " + seg.Translation + "\n")
		}
	}
	return b.String()
}

// manySpeakers reports whether turns name more than one speaker.
func manySpeakers(turns []store.Segment) bool {
	first := ""
	for _, seg := range turns {
		switch {
		case seg.Speaker == "":
		case first == "":
			first = seg.Speaker
		case seg.Speaker != first:
			return true
		}
	}
	return false
}

// clock is a time into a session as 00:42, or 1:02:03 past an hour, as
// Capy's window shows it and citations give it.
func clock(ms int64) string {
	d := time.Duration(max(ms, 0)) * time.Millisecond
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// The cursor of a transcript's next page: the seq it starts at, opaque to
// the agent.
const cursorPrefix = "capy1:"

func encodeCursor(seq int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursorPrefix + strconv.Itoa(seq)))
}

func decodeCursor(c string) (int, error) {
	if c == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(c)
	rest, ok := strings.CutPrefix(string(b), cursorPrefix)
	seq, aerr := strconv.Atoi(rest)
	if err != nil || !ok || aerr != nil || seq < 0 {
		return 0, errors.New("cursor is not one get_session gave")
	}
	return seq, nil
}
