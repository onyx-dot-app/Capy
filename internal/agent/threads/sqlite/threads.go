package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/onyx-dot-app/Capy/internal/agent/threads"
)

func nowMs() int64 { return time.Now().UnixMilli() }

// nullable stores "" as NULL, as a cross-session thread's session.
func nullable(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// threadCols must match scanThread.
const threadCols = `id, capability, session_id, scope, agent, conversation_id, title, created_at, updated_at`

type scanner interface{ Scan(dest ...any) error }

func scanThread(r scanner) (threads.Thread, error) {
	var (
		t                threads.Thread
		session          sql.NullString
		created, updated int64
	)
	if err := r.Scan(&t.ID, &t.Capability, &session, &t.Scope, &t.Agent, &t.ConversationID, &t.Title, &created, &updated); err != nil {
		return threads.Thread{}, err
	}
	t.SessionID = session.String
	t.CreatedAt, t.UpdatedAt = time.UnixMilli(created).UTC(), time.UnixMilli(updated).UTC()
	return t, nil
}

// CreateThread implements threads.Store.
func (db *DB) CreateThread(ctx context.Context, t threads.Thread) (threads.Thread, error) {
	if err := t.Validate(); err != nil {
		return threads.Thread{}, err
	}
	now := nowMs()
	if _, err := db.sql.ExecContext(ctx, `
INSERT INTO threads (id, capability, session_id, scope, agent, conversation_id, title, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Capability, nullable(t.SessionID), t.Scope, t.Agent, t.ConversationID, t.Title, now, now); err != nil {
		return threads.Thread{}, mapErr("create thread", err)
	}
	t.CreatedAt = time.UnixMilli(now).UTC()
	t.UpdatedAt = t.CreatedAt
	return t, nil
}

// AppendMessage implements threads.Store. It reads the thread's last seq
// and inserts after it in one transaction, which takes the write lock as it
// begins.
func (db *DB) AppendMessage(ctx context.Context, m threads.Message) (threads.Message, error) {
	const op = "append message"
	if err := m.Validate(); err != nil {
		return threads.Message{}, err
	}
	if m.Status == "" {
		m.Status = threads.StatusDone
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return threads.Message{}, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()

	var next sql.NullInt64
	err = tx.QueryRowContext(ctx, `
SELECT (SELECT MAX(seq) + 1 FROM messages WHERE thread_id = t.id) FROM threads t WHERE t.id = ?`, m.ThreadID).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		return threads.Message{}, fmt.Errorf("%s: %w", op, threads.ErrNotFound)
	}
	if err != nil {
		return threads.Message{}, fmt.Errorf("%s: %w", op, err)
	}
	m.Seq = int(next.Int64) // NULL for the first: 0
	now := nowMs()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO messages (id, thread_id, seq, role, text, status, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.ThreadID, m.Seq, m.Role, m.Text, m.Status, now); err != nil {
		return threads.Message{}, mapErr(op, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET updated_at = ? WHERE id = ?`, now, m.ThreadID); err != nil {
		return threads.Message{}, fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return threads.Message{}, fmt.Errorf("%s: %w", op, err)
	}
	m.CreatedAt = time.UnixMilli(now).UTC()
	return m, nil
}

// SetMessageStatus implements threads.Store.
func (db *DB) SetMessageStatus(ctx context.Context, messageID string, s threads.Status) error {
	if err := threads.ValidateID("message id", messageID); err != nil {
		return err
	}
	if s == "" {
		s = threads.StatusDone
	}
	if err := threads.ValidateStatus(s); err != nil {
		return err
	}
	res, err := db.sql.ExecContext(ctx, `UPDATE messages SET status = ? WHERE id = ?`, s, messageID)
	return requireOne("set message status", res, err)
}

// SetConversation implements threads.Store.
func (db *DB) SetConversation(ctx context.Context, threadID, conversationID string) error {
	if err := threads.ValidateID("thread id", threadID); err != nil {
		return err
	}
	if err := threads.ValidateConversation(conversationID); err != nil {
		return err
	}
	res, err := db.sql.ExecContext(ctx, `UPDATE threads SET conversation_id = ?, updated_at = ? WHERE id = ?`,
		conversationID, nowMs(), threadID)
	return requireOne("set conversation", res, err)
}

// SetAgent implements threads.Store.
func (db *DB) SetAgent(ctx context.Context, threadID, agent string) error {
	if err := threads.ValidateID("thread id", threadID); err != nil {
		return err
	}
	if err := threads.ValidateAgent(agent); err != nil {
		return err
	}
	res, err := db.sql.ExecContext(ctx, `UPDATE threads SET agent = ?, conversation_id = '', updated_at = ? WHERE id = ?`,
		agent, nowMs(), threadID)
	return requireOne("set agent", res, err)
}

// ListThreads implements threads.Store.
func (db *DB) ListThreads(ctx context.Context, q threads.ListQuery) ([]threads.Thread, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	var (
		rows *sql.Rows
		err  error
	)
	switch {
	case q.SessionID != "" && q.Capability != "":
		rows, err = db.sql.QueryContext(ctx, `
SELECT `+threadCols+` FROM threads WHERE session_id = ? AND capability = ? ORDER BY created_at, rowid`, q.SessionID, q.Capability)
	case q.SessionID != "":
		rows, err = db.sql.QueryContext(ctx, `
SELECT `+threadCols+` FROM threads WHERE session_id = ? ORDER BY created_at, rowid`, q.SessionID)
	default:
		rows, err = db.sql.QueryContext(ctx, `
SELECT `+threadCols+` FROM threads WHERE session_id IS NULL AND capability = ? ORDER BY created_at, rowid`, q.Capability)
	}
	if err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	defer rows.Close()
	out := []threads.Thread{}
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, fmt.Errorf("list threads: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	return out, nil
}

// GetThread implements threads.Store.
func (db *DB) GetThread(ctx context.Context, threadID string) (threads.Detail, error) {
	const op = "get thread"
	if err := threads.ValidateID("thread id", threadID); err != nil {
		return threads.Detail{}, err
	}
	t, err := scanThread(db.sql.QueryRowContext(ctx, `SELECT `+threadCols+` FROM threads WHERE id = ?`, threadID))
	if errors.Is(err, sql.ErrNoRows) {
		var session sql.NullString
		err = db.sql.QueryRowContext(ctx, `SELECT id, capability, session_id FROM pending_thread_erases WHERE id = ?`, threadID).
			Scan(&t.ID, &t.Capability, &session)
		if err == nil {
			t.SessionID = session.String
			return threads.Detail{Thread: t}, fmt.Errorf("%s: %w", op, threads.ErrErasing)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return threads.Detail{}, fmt.Errorf("%s: %w", op, err)
		}
		return threads.Detail{}, fmt.Errorf("%s: %w", op, threads.ErrNotFound)
	}
	if err != nil {
		return threads.Detail{}, fmt.Errorf("%s: %w", op, err)
	}
	// The pool has one connection, so the thread row above is fully read
	// before this query starts.
	rows, err := db.sql.QueryContext(ctx, `
SELECT id, thread_id, seq, role, text, status, created_at FROM messages WHERE thread_id = ? ORDER BY seq`, threadID)
	if err != nil {
		return threads.Detail{}, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close()
	d := threads.Detail{Thread: t, Messages: []threads.Message{}}
	for rows.Next() {
		var (
			m       threads.Message
			created int64
		)
		if err := rows.Scan(&m.ID, &m.ThreadID, &m.Seq, &m.Role, &m.Text, &m.Status, &created); err != nil {
			return threads.Detail{}, fmt.Errorf("%s: %w", op, err)
		}
		m.CreatedAt = time.UnixMilli(created).UTC()
		d.Messages = append(d.Messages, m)
	}
	if err := rows.Err(); err != nil {
		return threads.Detail{}, fmt.Errorf("%s: %w", op, err)
	}
	return d, nil
}

// DeleteThread implements threads.Store. Its messages go with it (ON
// DELETE CASCADE); secure_delete zeroes their pages and the checkpoint
// empties the log, so a deleted thread is erased at once.
func (db *DB) DeleteThread(ctx context.Context, threadID string) error {
	const op = "delete thread"
	if err := threads.ValidateID("thread id", threadID); err != nil {
		return err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO pending_thread_erases (id, capability, session_id)
SELECT id, capability, session_id FROM threads WHERE id = ?
ON CONFLICT(id) DO NOTHING`, threadID); err != nil {
		return mapErr(op, err)
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pending_thread_erases WHERE id = ?)`, threadID).Scan(&exists); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if !exists {
		return fmt.Errorf("%s: %w", op, threads.ErrNotFound)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM threads WHERE id = ?`, threadID); err != nil {
		return mapErr(op, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if err := db.checkpoint(ctx, op); err != nil {
		return err
	}
	_, err = db.sql.ExecContext(ctx, `DELETE FROM pending_thread_erases WHERE id = ?`, threadID)
	if err != nil {
		return fmt.Errorf("%s: finish erasure: %w", op, err)
	}
	return nil
}

// EraseSession implements threads.Store.
func (db *DB) EraseSession(ctx context.Context, sessionID string) error {
	const op = "erase session"
	if err := threads.ValidateID("session id", sessionID); err != nil {
		return err
	}
	if _, err := db.sql.ExecContext(ctx, `DELETE FROM threads WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return db.checkpoint(ctx, op)
}
