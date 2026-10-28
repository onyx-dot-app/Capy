// Package store defines Capy's domain types and the persistence contract.
//
// It contains no SQL. Adapters (see the sqlite subpackage) implement Store and
// must pass the contract suite in the storetest subpackage.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Mode is how a session was run.
type Mode string

const (
	// ModeCaptions is a one-direction live caption session.
	ModeCaptions Mode = "captions"
	// ModeConversation is a two-person, two-language session.
	ModeConversation Mode = "conversation"
)

// Source is where a session's audio came from.
type Source string

const (
	// SourceMic is the microphone.
	SourceMic Source = "mic"
	// SourceSystem is the system audio output.
	SourceSystem Source = "system"
	// SourceReplay is an audio file replayed for testing.
	SourceReplay Source = "replay"
	// SourceFile is an audio or video file transcribed whole, by Speech
	// Summarization.
	SourceFile Source = "file"
)

// RecapStatus is the state of a session's recap.
type RecapStatus string

const (
	// RecapPending means the recap has not been produced yet.
	RecapPending RecapStatus = "pending"
	// RecapReady means the recap was produced.
	RecapReady RecapStatus = "ready"
	// RecapFailed means producing the recap failed and can be retried.
	RecapFailed RecapStatus = "failed"
)

// GapLang is the Lang of a segment that marks audio lost during a reconnect.
// Such a segment has empty Text and is not counted in SessionSummary.SegmentCount.
const GapLang = "gap"

// Input bounds, enforced by the Validate methods.
const (
	MaxLangLen     = 35        // longest BCP-47 tag we accept
	MaxSpeakerLen  = 64        // bytes
	MaxSegmentText = 64 << 10  // bytes, for Text and Translation
	MaxTitleLen    = 1 << 10   // bytes
	MaxFileNameLen = 1 << 10   // bytes: macOS allows 255 UTF-16 units
	MaxRecapText   = 256 << 10 // bytes

	DefaultListLimit   = 50
	MaxListLimit       = 500
	DefaultSearchLimit = 20
	MaxSearchLimit     = 100
	MaxSearchQueryLen  = 512 // bytes
	MaxTagLen          = 64  // bytes, for an edit's actor and origin
	MaxTrackingIDLen   = 256 // bytes, as Zoom's servers bound it
)

// TrashRetention is how long a session stays in Recently Deleted before
// it may be purged.
const TrashRetention = 30 * 24 * time.Hour

// Sentinel errors. Adapters wrap them; test with errors.Is.
var (
	// ErrNotFound means the session, segment or edit does not exist, or
	// the session was purged, or is in Recently Deleted for all but
	// restoring it.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict means a duplicate id, or a duplicate (session id, seq);
	// trashing a session that has not ended; or undoing an edit undone
	// already, or whose field changed since.
	ErrConflict = errors.New("store: conflict")
	// ErrInvalid means an argument failed validation. The wrapping error names
	// the field and never includes user text.
	ErrInvalid = errors.New("store: invalid argument")
)

// Session is the metadata of one captions or conversation run.
type Session struct {
	ID   string
	Mode Mode
	// Source is the audio source.
	Source Source
	// LangA and LangB are both required. Captions: LangA is the spoken
	// language and LangB the translation target. Conversation: the two
	// people's languages.
	LangA, LangB string
	StartedAt    time.Time
	// FileName is the name of the file a SourceFile session transcribed,
	// without its folders; empty for every other source.
	FileName string
	// BatchID is the upload a SourceFile session came in with others, a
	// UUID; empty for none, and for every other source.
	BatchID string
	// TrackingID is the ID Zoom's logs know a SourceFile session's
	// transcription by, visible ASCII; empty for none, and for every other
	// source.
	TrackingID string
}

// Segment is one completed turn of a session.
type Segment struct {
	ID        string
	SessionID string
	// Seq orders segments within a session; it is unique per session.
	Seq int
	// Lang is the language of Text, or GapLang for a gap marker.
	Lang string
	Text string
	// Speaker is Scribe's label of who said it ("Speaker 1"), if any. Labels
	// are numbered anew on each connection: after a reconnect or a change of
	// spoken language.
	Speaker string
	// StartMs and EndMs are offsets from the session start.
	StartMs, EndMs int64
	// TranslationLang is the language Translation is in, or is awaited in,
	// empty for a turn said with translation off. Translation is empty
	// until translated.
	Translation     string
	TranslationLang string
}

// Recap is the summary of a session.
type Recap struct {
	Status RecapStatus
	// Title is the first line of the summary, shown in the Library.
	Title string
	Text  string
}

// SessionSummary is a session as listed in the Library.
type SessionSummary struct {
	ID           string
	Mode         Mode
	Source       Source
	LangA, LangB string
	// Title is the session's name: its rename, else its recap's title.
	Title string
	// FileName is the file a SourceFile session transcribed, and BatchID
	// the upload it came in.
	FileName, BatchID string
	RecapStatus       RecapStatus
	StartedAt         time.Time
	// EndedAt is the zero time while the session has not ended.
	EndedAt time.Time
	// SegmentCount excludes gap segments.
	SegmentCount int
	// Translated reports whether any segment was to be translated, which a
	// segment with a TranslationLang was, translated yet or not.
	Translated bool
	// HeartbeatAt is when the engine last reported the session still
	// recording; zero if it never did.
	HeartbeatAt time.Time
}

// TrashedSession is a session in Recently Deleted.
type TrashedSession struct {
	SessionSummary
	TrashedAt time.Time
	// PurgePending means erasure has started and can only be retried.
	PurgePending bool
}

// EditField is what an edit changes of a session.
type EditField string

const (
	// FieldTitle is the rename: "" gives the session its recap's title.
	FieldTitle EditField = "title"
	// FieldRecap is the recap's text.
	FieldRecap EditField = "recap"
	// FieldTrash is whether the session is in Recently Deleted: "" when it
	// is not, else when it moved there, in Unix milliseconds.
	FieldTrash EditField = "trash"
)

// Edit is one change to a session, by the user or anyone else, kept with
// the value it replaced so that it can be undone.
type Edit struct {
	ID                 string
	SessionID          string
	Field              EditField
	OldValue, NewValue string
	// Actor is who made the edit: "user", or the caller's name. Origin is
	// the caller's own tag, such as a run ID, "" for none. The store gives
	// neither a meaning.
	Actor, Origin string
	CreatedAt     time.Time
	// UndoneAt is zero while the edit stands.
	UndoneAt time.Time
}

// SessionDetail is a session with its recap and transcript.
type SessionDetail struct {
	SessionSummary
	Recap string
	// TrackingID is the Session's.
	TrackingID string
	// Segments are ordered by Seq.
	Segments []Segment
	// Highlights are ordered by Seq, the dismissed ones too.
	Highlights []Highlight
}

// ListQuery pages through sessions, newest first.
type ListQuery struct {
	// Limit 0 means DefaultListLimit; values above MaxListLimit are capped.
	Limit  int
	Offset int
	// Unbatched leaves out the sessions of a batch, which ListBatches
	// lists.
	Unbatched bool
}

// Store persists sessions and transcripts. Implementations are safe for
// concurrent use.
//
// A session lives, is trashed into Recently Deleted (its text kept) and
// restored, or is purged: erased, but for a tombstone kept for a future
// sync. Purged sessions behave as missing everywhere. Trashed ones too,
// but for ListTrashed, RestoreSession, PurgeSession, undoing their trash,
// and the recap and translations still being written as they were
// trashed (SetRecap, SetTranslation).
type Store interface {
	// CreateSession stores a new session. Returns ErrConflict on a duplicate ID.
	CreateSession(ctx context.Context, s Session) error
	// AppendSegment adds a segment. Returns ErrNotFound if the session is
	// missing or deleted, and ErrConflict on a duplicate ID or (SessionID, Seq).
	AppendSegment(ctx context.Context, seg Segment) error
	// SetTranslation fills in a segment's translation.
	SetTranslation(ctx context.Context, segmentID, lang, text string) error
	// SetTarget changes LangB, a captions session's translation target,
	// while it records. Returns ErrNotFound once the session ended.
	SetTarget(ctx context.Context, sessionID, lang string) error
	// SetRecap replaces a session's recap.
	SetRecap(ctx context.Context, sessionID string, recap Recap) error
	// EndSession records when a session ended.
	EndSession(ctx context.Context, sessionID string, endedAt time.Time) error
	// ListSessions returns sessions ordered by StartedAt then ID, newest first.
	ListSessions(ctx context.Context, q ListQuery) ([]SessionSummary, error)
	// GetSession returns a session with its segments ordered by Seq.
	GetSession(ctx context.Context, sessionID string) (SessionDetail, error)
	// Search returns sessions whose segment text or translation contains all
	// the whitespace-separated words of query, newest first. The query is plain
	// words, never FTS syntax. A blank query returns no sessions. limit 0 means
	// DefaultSearchLimit; values above MaxSearchLimit are capped.
	Search(ctx context.Context, query string, limit int) ([]SessionSummary, error)
	// SearchRange is Search among the sessions started from from to to,
	// both included; a zero bound is open.
	SearchRange(ctx context.Context, query string, from, to time.Time, limit int) ([]SessionSummary, error)

	// RenameSession sets the session's title; "" gives it its recap's
	// title again. A new recap keeps the rename.
	RenameSession(ctx context.Context, sessionID, title, actor, origin string) (Edit, error)
	// UpdateRecap replaces the recap's text.
	UpdateRecap(ctx context.Context, sessionID, recap, actor, origin string) (Edit, error)
	// TrashSession moves an ended session to Recently Deleted; one that
	// has not ended is ErrConflict.
	TrashSession(ctx context.Context, sessionID, actor, origin string) (Edit, error)
	// RestoreSession takes a session out of Recently Deleted. A claimed
	// purge cannot be restored, through RestoreSession or UndoEdit.
	RestoreSession(ctx context.Context, sessionID, actor, origin string) (Edit, error)
	// UndoEdit puts back the value an edit replaced, and returns the edit,
	// undone. It is ErrConflict when the field changed since, so that Undo
	// never overwrites newer text; undoing a trash restores the session.
	UndoEdit(ctx context.Context, editID string) (Edit, error)
	// ListEdits returns the edits recorded with origin, oldest first.
	ListEdits(ctx context.Context, origin string) ([]Edit, error)
	// GetEdit returns edit id, ErrNotFound once its session is purged.
	GetEdit(ctx context.Context, id string) (Edit, error)
	// ListTrashed returns the sessions in Recently Deleted, the latest
	// trashed first.
	ListTrashed(ctx context.Context) ([]TrashedSession, error)
	// ListExpired returns sessions trashed more than TrashRetention before
	// now, and any pending purges, to finish even after a restart.
	ListExpired(ctx context.Context, now time.Time) ([]string, error)
	// ClaimPurge durably claims a trashed session before destructive hooks
	// run, preventing restoration. An existing claim can be retried;
	// a restored, missing or fully purged session is ErrNotFound.
	ClaimPurge(ctx context.Context, sessionID string) error
	// PurgeSession erases a session in Recently Deleted: its transcript,
	// titles, recap, highlights, edits and its file of a batch. Its tombstone stays. A session not in
	// Recently Deleted is ErrNotFound. A failed purge remains claimed
	// until a retry finishes erasure and its checkpoint.
	PurgeSession(ctx context.Context, sessionID string) error
	// Heartbeat records that a session not yet ended still records, at at.
	Heartbeat(ctx context.Context, sessionID string, at time.Time) error

	// AddHighlights stores highlights of one session's turns, all or
	// none. A session trashed meanwhile still takes them, to have them if
	// restored. A missing or purged session is ErrNotFound; a duplicate ID,
	// or a turn highlighted already, ErrConflict.
	AddHighlights(ctx context.Context, hs []Highlight) error
	// DismissHighlight marks highlight id dismissed. One whose session is
	// missing, trashed or purged is ErrNotFound.
	DismissHighlight(ctx context.Context, id string) error
	// PolishHighlights saves an agent's polish of a session's highlights,
	// all or none. A highlight dismissed meanwhile, a debate's too, is
	// skipped, its polish not saved, as is a debate on a turn another
	// kind's highlight has: Highlights may mark it while the agent writes.
	// A missing or purged session, or a highlight not of it, is
	// ErrNotFound. A session trashed meanwhile still takes it, as
	// AddHighlights.
	PolishHighlights(ctx context.Context, p Polishing) error

	// CreateBatch stores a batch and its files, numbered in order,
	// waiting. A duplicate ID is ErrConflict.
	CreateBatch(ctx context.Context, b Batch, files []BatchFile) error
	// ListBatches returns the batches, the newest first, with their files
	// in order, a saved file's session listed. A file whose session is in
	// Recently Deleted is left out, and a batch left without files too.
	ListBatches(ctx context.Context) ([]BatchSummary, error)
	// GetBatch returns batch id as ListBatches lists it, with each saved
	// file's recap.
	GetBatch(ctx context.Context, id string) (BatchSummary, error)
	// SetFileState records where a file not saved is: waiting, failed for
	// problem, or canceled. A saved file is ErrConflict.
	SetFileState(ctx context.Context, fileID string, state FileState, problem string) error
	// SaveFile records that a file was saved as session sessionID, a file
	// session, which joins the file's batch and takes the file's Title as
	// its rename; the file's path is forgotten. A file saved already is
	// ErrConflict. A file no longer in a batch, moved out or its batch
	// deleted, is ErrNotFound, and the session is left on its own.
	SaveFile(ctx context.Context, fileID, sessionID string) error
	// RenameBatch names a batch.
	RenameBatch(ctx context.Context, id, name string) error
	// RenameFile names a file not saved yet; a saved one is ErrConflict:
	// rename its session.
	RenameFile(ctx context.Context, fileID, title string) error
	// RemoveFile takes a file out of its batch: a saved file's session
	// stays, on its own. A batch left without files is forgotten.
	RemoveFile(ctx context.Context, fileID string) error
	// UngroupBatch forgets a batch: its sessions stay, on their own, and
	// its files not saved are forgotten with it.
	UngroupBatch(ctx context.Context, id string) error
	// DeleteBatch moves the batch's ended sessions to Recently Deleted, on
	// their own, and forgets the batch with its files; it returns the
	// edits that trashed them.
	DeleteBatch(ctx context.Context, id, actor, origin string) ([]Edit, error)
	// DataVersion changes when another connection to the store, such as
	// another process's, has changed it.
	DataVersion(ctx context.Context) (int64, error)
}

func invalid(field, why string) error {
	return fmt.Errorf("%w: %s %s", ErrInvalid, field, why)
}

// ValidateID checks that id is a UUID.
func ValidateID(field, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return invalid(field, "must be a UUID")
	}
	return nil
}

func validateLang(field, lang string, required bool) error {
	if lang == "" {
		if required {
			return invalid(field, "is required")
		}
		return nil
	}
	if len(lang) > MaxLangLen {
		return invalid(field, "is too long")
	}
	return nil
}

func validateText(field, text string, max int) error {
	if len(text) > max {
		return invalid(field, "is too long")
	}
	if !utf8.ValidString(text) {
		return invalid(field, "is not valid UTF-8")
	}
	return nil
}

// Validate checks s against the contract.
func (s Session) Validate() error {
	if err := ValidateID("session id", s.ID); err != nil {
		return err
	}
	switch s.Mode {
	case ModeCaptions, ModeConversation:
	default:
		return invalid("mode", "is unknown")
	}
	switch s.Source {
	case SourceMic, SourceSystem, SourceReplay, SourceFile:
	default:
		return invalid("source", "is unknown")
	}
	if s.Source == SourceFile {
		if err := ValidateFileName(s.FileName); err != nil {
			return err
		}
	} else if s.FileName != "" {
		return invalid("file name", "is for a file only")
	} else if s.BatchID != "" {
		return invalid("batch id", "is for a file only")
	} else if s.TrackingID != "" {
		return invalid("tracking id", "is for a file only")
	}
	if len(s.TrackingID) > MaxTrackingIDLen || strings.ContainsFunc(s.TrackingID, func(r rune) bool { return r <= ' ' || r > '~' }) {
		return invalid("tracking id", "must be visible ASCII, not too long")
	}
	if s.BatchID != "" {
		if err := ValidateID("batch id", s.BatchID); err != nil {
			return err
		}
	}
	if err := validateLang("lang a", s.LangA, true); err != nil {
		return err
	}
	if err := validateLang("lang b", s.LangB, true); err != nil {
		return err
	}
	if s.StartedAt.IsZero() {
		return invalid("started at", "is required")
	}
	return nil
}

// Validate checks seg against the contract. A non-gap segment needs text; a
// gap segment must have none.
func (seg Segment) Validate() error {
	if err := ValidateID("segment id", seg.ID); err != nil {
		return err
	}
	if err := ValidateID("session id", seg.SessionID); err != nil {
		return err
	}
	if seg.Seq < 0 {
		return invalid("seq", "must not be negative")
	}
	if seg.StartMs < 0 || seg.StartMs > seg.EndMs {
		return invalid("time range", "must satisfy 0 <= start <= end")
	}
	if err := validateLang("lang", seg.Lang, true); err != nil {
		return err
	}
	if err := validateText("text", seg.Text, MaxSegmentText); err != nil {
		return err
	}
	if err := validateText("speaker", seg.Speaker, MaxSpeakerLen); err != nil {
		return err
	}
	if (seg.Lang == GapLang) != (seg.Text == "") {
		return invalid("text", "must be empty exactly for gap segments")
	}
	if err := validateText("translation", seg.Translation, MaxSegmentText); err != nil {
		return err
	}
	if err := validateLang("translation lang", seg.TranslationLang, seg.Translation != ""); err != nil {
		return err
	}
	return nil
}

// ValidateTag checks an edit's actor or origin: a short string of
// printable characters, required or not.
func ValidateTag(field, tag string, required bool) error {
	switch {
	case tag == "" && required:
		return invalid(field, "is required")
	case len(tag) > MaxTagLen:
		return invalid(field, "is too long")
	case !utf8.ValidString(tag) || strings.ContainsFunc(tag, func(r rune) bool { return !unicode.IsPrint(r) }):
		return invalid(field, "is not printable")
	}
	return nil
}

// ValidateTitle checks a session's new title: one line of printable
// characters, or "". Control and format characters, which could hide or
// reorder what a title shows, are refused.
func ValidateTitle(title string) error {
	if err := validateText("title", title, MaxTitleLen); err != nil {
		return err
	}
	if strings.ContainsFunc(title, func(r rune) bool { return !unicode.IsPrint(r) }) {
		return invalid("title", "must be one line of printable characters")
	}
	return nil
}

// ValidateFileName checks the name of a file a session transcribes: a name
// without folders, on one line, without the controls that could reorder
// what it shows. Spaces of every kind and emoji are names macOS gives, as
// the narrow space before "PM" in a screen recording's.
func ValidateFileName(name string) error {
	if name == "" {
		return invalid("file name", "is required for a file")
	}
	if err := validateText("file name", name, MaxFileNameLen); err != nil {
		return err
	}
	if strings.ContainsRune(name, '/') || strings.ContainsFunc(name, unsafeInName) {
		return invalid("file name", "must be a name without folders, on one line")
	}
	return nil
}

// unsafeInName reports a control character, a line or paragraph separator,
// or a bidirectional embedding, override or isolate.
func unsafeInName(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Zl, unicode.Zp) ||
		r >= '\u202a' && r <= '\u202e' || r >= '\u2066' && r <= '\u2069'
}

// ValidateRecapText checks a recap's new text.
func ValidateRecapText(text string) error {
	return validateText("recap text", text, MaxRecapText)
}

// ValidateTranslation checks the arguments of SetTranslation.
func ValidateTranslation(segmentID, lang, text string) error {
	if err := ValidateID("segment id", segmentID); err != nil {
		return err
	}
	if err := validateLang("translation lang", lang, true); err != nil {
		return err
	}
	if text == "" {
		return invalid("translation", "is required")
	}
	return validateText("translation", text, MaxSegmentText)
}

// ValidateTarget checks the arguments of SetTarget.
func ValidateTarget(sessionID, lang string) error {
	if err := ValidateID("session id", sessionID); err != nil {
		return err
	}
	return validateLang("lang b", lang, true)
}

// Validate checks r against the contract.
func (r Recap) Validate() error {
	switch r.Status {
	case RecapPending, RecapReady, RecapFailed:
	default:
		return invalid("recap status", "is unknown")
	}
	if err := validateText("title", r.Title, MaxTitleLen); err != nil {
		return err
	}
	return validateText("recap text", r.Text, MaxRecapText)
}

// Normalize validates q and applies the default and maximum limit.
func (q ListQuery) Normalize() (ListQuery, error) {
	if q.Limit < 0 || q.Offset < 0 {
		return q, invalid("list query", "must not be negative")
	}
	if q.Limit == 0 {
		q.Limit = DefaultListLimit
	}
	q.Limit = min(q.Limit, MaxListLimit)
	return q, nil
}

// NormalizeSearchLimit validates limit and applies the default and maximum.
func NormalizeSearchLimit(limit int) (int, error) {
	if limit < 0 {
		return 0, invalid("limit", "must not be negative")
	}
	if limit == 0 {
		limit = DefaultSearchLimit
	}
	return min(limit, MaxSearchLimit), nil
}

// ValidateSearchQuery checks the length and encoding of a search query.
func ValidateSearchQuery(query string) error {
	if len(query) > MaxSearchQueryLen {
		return invalid("query", "is too long")
	}
	if !utf8.ValidString(query) {
		return invalid("query", "is not valid UTF-8")
	}
	return nil
}
