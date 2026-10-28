// Package threads defines Agent's conversations about Capy's recordings
// and the contract that stores them. It contains no SQL. Adapters (see the
// sqlite subpackage) implement Store and must pass the contract suite in
// the storetest subpackage.
//
// A thread belongs to one capability, such as Ask, and either to one
// session of Capy's library or to none (cross-session). Its messages are
// the user's questions, the agent's answers, and the edits the agent made,
// in order.
package threads

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/onyx-dot-app/Capy/internal/store"
)

// Scopes of a thread: what it asks about. A range is RangeScope's.
const (
	// ScopeSession is the thread's session.
	ScopeSession = "session"
	// ScopeAll is every session in the library.
	ScopeAll = "all"
)

// rangePrefix starts a range scope: "range:<from_ms>-<to_ms>"; batchPrefix
// a batch's: "batch:<batch id>".
const (
	rangePrefix = "range:"
	batchPrefix = "batch:"
)

// BatchScope is the scope of the sessions of batch id, a batch of Capy's
// library.
func BatchScope(id string) string { return batchPrefix + id }

// ParseBatch returns the batch of a batch scope; ok is false for any other
// scope.
func ParseBatch(scope string) (id string, ok bool) {
	id, found := strings.CutPrefix(scope, batchPrefix)
	return id, found && store.ValidateID("batch id", id) == nil
}

// RangeScope is the scope of the sessions started from from to to.
func RangeScope(from, to time.Time) string {
	return rangePrefix + strconv.FormatInt(from.UnixMilli(), 10) + "-" + strconv.FormatInt(to.UnixMilli(), 10)
}

// ParseRange returns the bounds of a range scope; ok is false for any
// other scope.
func ParseRange(scope string) (from, to time.Time, ok bool) {
	rest, found := strings.CutPrefix(scope, rangePrefix)
	if !found {
		return time.Time{}, time.Time{}, false
	}
	a, b, found := strings.Cut(rest, "-")
	if !found {
		return time.Time{}, time.Time{}, false
	}
	fromMs, errA := strconv.ParseInt(a, 10, 64)
	toMs, errB := strconv.ParseInt(b, 10, 64)
	if errA != nil || errB != nil || fromMs < 0 || toMs < fromMs {
		return time.Time{}, time.Time{}, false
	}
	return time.UnixMilli(fromMs).UTC(), time.UnixMilli(toMs).UTC(), true
}

// Role is who a message is from.
type Role string

const (
	// RoleUser is the user's question.
	RoleUser Role = "user"
	// RoleAgent is the agent's answer.
	RoleAgent Role = "agent"
	// RoleEdit is an edit the agent made to the library: its Text is the
	// edit's ID in Capy's store.
	RoleEdit Role = "edit"
)

// Status is how a message stands.
type Status string

const (
	// StatusDone is a question asked, an answer given in full, an edit.
	StatusDone Status = "done"
	// StatusIncomplete is an answer cut short: the run stopped or failed.
	StatusIncomplete Status = "incomplete"
	// StatusUnanswered is a question whose run never ended, as when Capy
	// quit during it.
	StatusUnanswered Status = "unanswered"
)

// Input bounds, enforced by the Validate methods.
const (
	MaxTitleLen = 1 << 10   // bytes
	MaxTextLen  = 256 << 10 // bytes, of a message
)

// Sentinel errors. Adapters wrap them; test with errors.Is.
var (
	// ErrNotFound means the thread or message does not exist.
	ErrNotFound = errors.New("threads: not found")
	// ErrErasing means the thread's rows are deleted but its file erasure
	// needs retrying. GetThread returns only its ID, capability and session.
	ErrErasing = errors.New("threads: erasure pending")
	// ErrConflict means a duplicate ID.
	ErrConflict = errors.New("threads: conflict")
	// ErrInvalid means an argument failed validation. The wrapping error
	// names the field and never includes user text.
	ErrInvalid = errors.New("threads: invalid argument")
)

// Thread is one conversation with an agent.
type Thread struct {
	ID string
	// Capability is the Agent capability the thread is of, such as "ask".
	Capability string
	// SessionID is the session of Capy's library it is about, "" for a
	// cross-session thread.
	SessionID string
	// Scope is ScopeSession for a session's thread; ScopeAll, a
	// RangeScope or a BatchScope for a cross-session one.
	Scope string
	// Agent is the ID of the gateway agent that answers it.
	Agent string
	// ConversationID is the agent's own ID of the conversation, to resume
	// it; "" until the agent gives one.
	ConversationID string
	// Title is the thread's first question.
	Title string
	// CreatedAt and UpdatedAt are set by the store.
	CreatedAt, UpdatedAt time.Time
}

// Message is one message of a thread.
type Message struct {
	ID       string
	ThreadID string
	// Seq orders a thread's messages; the store gives each the next.
	Seq  int
	Role Role
	// Text is the question or the answer; for RoleEdit, the edit's ID.
	Text string
	// Status "" is StatusDone.
	Status Status
	// CreatedAt is set by the store.
	CreatedAt time.Time
}

// Detail is a thread with its messages, ordered by Seq.
type Detail struct {
	Thread
	Messages []Message
}

// ListQuery selects threads: a session's (of Capability too, when it is
// set), or, with no SessionID, the cross-session threads of Capability.
type ListQuery struct {
	SessionID  string
	Capability string
}

// Store persists threads. Implementations are safe for concurrent use.
type Store interface {
	// CreateThread stores a new thread, without messages. It returns
	// ErrConflict on a duplicate ID.
	CreateThread(ctx context.Context, t Thread) (Thread, error)
	// AppendMessage adds a message at the end of its thread, and returns
	// it with its Seq. It returns ErrNotFound for a missing thread and
	// ErrConflict on a duplicate ID.
	AppendMessage(ctx context.Context, m Message) (Message, error)
	// SetMessageStatus changes a message's status, as when a question's
	// run ends without an answer.
	SetMessageStatus(ctx context.Context, messageID string, s Status) error
	// SetConversation records the agent's ID of the thread's conversation.
	SetConversation(ctx context.Context, threadID, conversationID string) error
	// SetAgent hands the thread to another agent: its conversation, the
	// agent's before, is forgotten until the new agent starts one.
	SetAgent(ctx context.Context, threadID, agent string) error
	// ListThreads returns the threads q selects, oldest first.
	ListThreads(ctx context.Context, q ListQuery) ([]Thread, error)
	// GetThread returns a thread with its messages. For ErrErasing, only
	// its ID, capability and session are returned, to authorize a retry.
	GetThread(ctx context.Context, threadID string) (Detail, error)
	// DeleteThread erases a thread and its messages, at once, from the
	// store's files too.
	DeleteThread(ctx context.Context, threadID string) error
	// EraseSession erases every thread of a session and their messages,
	// so that nothing of them remains in the store's files: the purge hook
	// of Capy's library. A session without threads is no error.
	EraseSession(ctx context.Context, sessionID string) error
}

func invalid(field, why string) error {
	return fmt.Errorf("%w: %s %s", ErrInvalid, field, why)
}

// wrapInvalid makes a validation error of Capy's store one of this
// package's too.
func wrapInvalid(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrInvalid, err)
}

// ValidateID checks that id is a UUID.
func ValidateID(field, id string) error { return wrapInvalid(store.ValidateID(field, id)) }

func validateText(field, text string, max int) error {
	if len(text) > max {
		return invalid(field, "is too long")
	}
	if !utf8.ValidString(text) {
		return invalid(field, "is not valid UTF-8")
	}
	return nil
}

// ValidateConversation checks an agent's ID of a conversation: a short
// printable string.
func ValidateConversation(id string) error {
	return wrapInvalid(store.ValidateTag("conversation id", id, true))
}

// ValidateAgent checks a gateway agent's ID.
func ValidateAgent(agent string) error { return wrapInvalid(store.ValidateTag("agent", agent, true)) }

// Validate checks t against the contract.
func (t Thread) Validate() error {
	if err := ValidateID("thread id", t.ID); err != nil {
		return err
	}
	if err := wrapInvalid(store.ValidateTag("capability", t.Capability, true)); err != nil {
		return err
	}
	if err := ValidateAgent(t.Agent); err != nil {
		return err
	}
	if t.SessionID != "" {
		if err := ValidateID("session id", t.SessionID); err != nil {
			return err
		}
	}
	_, _, isRange := ParseRange(t.Scope)
	_, isBatch := ParseBatch(t.Scope)
	cross := t.Scope == ScopeAll || isRange || isBatch
	switch {
	case t.Scope == ScopeSession && t.SessionID == "":
		return invalid("scope", "needs a session")
	case t.Scope == ScopeSession:
	case cross && t.SessionID != "":
		return invalid("scope", "is cross-session but names a session")
	case cross:
	default:
		return invalid("scope", "is unknown")
	}
	if t.ConversationID != "" {
		if err := ValidateConversation(t.ConversationID); err != nil {
			return err
		}
	}
	return validateText("title", t.Title, MaxTitleLen)
}

// Validate checks m against the contract.
func (m Message) Validate() error {
	if err := ValidateID("message id", m.ID); err != nil {
		return err
	}
	if err := ValidateID("thread id", m.ThreadID); err != nil {
		return err
	}
	switch m.Role {
	case RoleUser, RoleAgent:
		if err := validateText("text", m.Text, MaxTextLen); err != nil {
			return err
		}
	case RoleEdit:
		if err := ValidateID("edit id", m.Text); err != nil {
			return err
		}
	default:
		return invalid("role", "is unknown")
	}
	return ValidateStatus(m.Status)
}

// ValidateStatus checks a message's status; "" is StatusDone.
func ValidateStatus(s Status) error {
	switch s {
	case "", StatusDone, StatusIncomplete, StatusUnanswered:
		return nil
	}
	return invalid("status", "is unknown")
}

// Validate checks q: a session, or a capability without one.
func (q ListQuery) Validate() error {
	if q.SessionID != "" {
		if err := ValidateID("session id", q.SessionID); err != nil {
			return err
		}
	} else if q.Capability == "" {
		return invalid("list query", "needs a session or a capability")
	}
	if q.Capability != "" {
		return wrapInvalid(store.ValidateTag("capability", q.Capability, true))
	}
	return nil
}
