package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/store"
)

func nowMs() int64 { return time.Now().UnixMilli() }

// CreateSession implements store.Store.
func (db *DB) CreateSession(ctx context.Context, s store.Session) error {
	if err := s.Validate(); err != nil {
		return err
	}
	now := nowMs()
	_, err := db.sql.ExecContext(ctx, `
INSERT INTO sessions (id, mode, source, lang_a, lang_b, file_name, batch_id, tracking_id, recap_status, started_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Mode, s.Source, s.LangA, s.LangB, s.FileName, s.BatchID, s.TrackingID, store.RecapPending, s.StartedAt.UnixMilli(), now, now)
	if err != nil {
		return mapErr("create session", err)
	}
	return nil
}

// AppendSegment implements store.Store. The insert selects from the live
// session row, so a missing, trashed or purged session inserts nothing.
func (db *DB) AppendSegment(ctx context.Context, seg store.Segment) error {
	if err := seg.Validate(); err != nil {
		return err
	}
	now := nowMs()
	res, err := db.sql.ExecContext(ctx, `
INSERT INTO segments (id, session_id, seq, lang, text, speaker, translation, translation_lang, start_ms, end_ms, created_at, updated_at)
SELECT ?, s.id, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
FROM sessions s WHERE s.id = ? AND s.deleted_at IS NULL AND s.trashed_at IS NULL`,
		seg.ID, seg.Seq, seg.Lang, seg.Text, seg.Speaker, seg.Translation, seg.TranslationLang, seg.StartMs, seg.EndMs, now, now,
		seg.SessionID)
	return requireOne("append segment", res, err)
}

// SetTranslation implements store.Store. A session trashed meanwhile still
// takes it, to have it if restored.
func (db *DB) SetTranslation(ctx context.Context, segmentID, lang, text string) error {
	if err := store.ValidateTranslation(segmentID, lang, text); err != nil {
		return err
	}
	res, err := db.sql.ExecContext(ctx, `
UPDATE segments SET translation = ?, translation_lang = ?, updated_at = ?
WHERE id = ? AND session_id IN (SELECT id FROM sessions WHERE deleted_at IS NULL AND purge_pending = 0)`,
		text, lang, nowMs(), segmentID)
	return requireOne("set translation", res, err)
}

// SetTarget implements store.Store.
func (db *DB) SetTarget(ctx context.Context, sessionID, lang string) error {
	if err := store.ValidateTarget(sessionID, lang); err != nil {
		return err
	}
	res, err := db.sql.ExecContext(ctx, `
UPDATE sessions SET lang_b = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL AND ended_at IS NULL`,
		lang, nowMs(), sessionID)
	return requireOne("set target", res, err)
}

// SetRecap implements store.Store. It writes the recap's title, which a
// rename overrides, and takes a session trashed while its recap was being
// written, to have it if restored.
func (db *DB) SetRecap(ctx context.Context, sessionID string, recap store.Recap) error {
	if err := store.ValidateID("session id", sessionID); err != nil {
		return err
	}
	if err := recap.Validate(); err != nil {
		return err
	}
	res, err := db.sql.ExecContext(ctx, `
UPDATE sessions SET title = ?, recap = ?, recap_status = ?, updated_at = ?
WHERE id = ? AND deleted_at IS NULL AND purge_pending = 0`,
		recap.Title, recap.Text, recap.Status, nowMs(), sessionID)
	return requireOne("set recap", res, err)
}

// EndSession implements store.Store.
func (db *DB) EndSession(ctx context.Context, sessionID string, endedAt time.Time) error {
	if err := store.ValidateID("session id", sessionID); err != nil {
		return err
	}
	if endedAt.IsZero() {
		return fmt.Errorf("%w: ended at is required", store.ErrInvalid)
	}
	res, err := db.sql.ExecContext(ctx, `
UPDATE sessions SET ended_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL AND trashed_at IS NULL`,
		endedAt.UnixMilli(), nowMs(), sessionID)
	return requireOne("end session", res, err)
}

// Heartbeat implements store.Store.
func (db *DB) Heartbeat(ctx context.Context, sessionID string, at time.Time) error {
	if err := store.ValidateID("session id", sessionID); err != nil {
		return err
	}
	res, err := db.sql.ExecContext(ctx, `
UPDATE sessions SET heartbeat_at = ? WHERE id = ? AND deleted_at IS NULL AND ended_at IS NULL`, at.UnixMilli(), sessionID)
	return requireOne("heartbeat", res, err)
}

// AddHighlights implements store.Store. A session trashed meanwhile
// still takes them, to have them if restored, as its translations.
func (db *DB) AddHighlights(ctx context.Context, hs []store.Highlight) error {
	if len(hs) == 0 {
		return nil
	}
	for _, h := range hs {
		if err := h.Validate(); err != nil {
			return err
		}
		if h.SessionID != hs[0].SessionID {
			return fmt.Errorf("%w: highlights must be of one session", store.ErrInvalid)
		}
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("add highlights: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, h := range hs {
		polish, err := encodePolish(h.Polish)
		if err != nil {
			return fmt.Errorf("add highlights: %w", err)
		}
		res, err := tx.ExecContext(ctx, `
INSERT INTO highlights (id, session_id, seq, kind, confidence, dismissed, created_at, polish, polished_at)
SELECT ?, s.id, ?, ?, ?, ?, ?, ?, ? FROM sessions s WHERE s.id = ? AND s.deleted_at IS NULL AND s.purge_pending = 0`,
			h.ID, h.Seq, h.Kind, h.Confidence, h.Dismissed, h.CreatedAt.UnixMilli(), polish, msOrNull(h.PolishedAt), h.SessionID)
		if err := requireOne("add highlights", res, err); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("add highlights: %w", err)
	}
	return nil
}

// PolishHighlights implements store.Store.
func (db *DB) PolishHighlights(ctx context.Context, p store.Polishing) error {
	if err := p.Validate(); err != nil {
		return err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("polish highlights: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var one int
	err = tx.QueryRowContext(ctx, `
SELECT 1 FROM sessions WHERE id = ? AND deleted_at IS NULL AND purge_pending = 0`, p.SessionID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("polish highlights: %w", store.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("polish highlights: %w", err)
	}
	at := p.At.UnixMilli()
	for id, pol := range p.Polish {
		var kind store.HighlightKind
		var dismissed bool
		err := tx.QueryRowContext(ctx, `SELECT kind, dismissed FROM highlights WHERE id = ? AND session_id = ?`, id, p.SessionID).
			Scan(&kind, &dismissed)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("polish highlights: %w", store.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("polish highlights: %w", err)
		}
		if kind == store.KindDebate {
			return fmt.Errorf("%w: a debate's polish goes in debates", store.ErrInvalid)
		}
		if dismissed {
			continue
		}
		polish, err := encodePolish(&pol)
		if err != nil {
			return fmt.Errorf("polish highlights: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE highlights SET polish = ?, polished_at = ? WHERE id = ?`, polish, at, id); err != nil {
			return fmt.Errorf("polish highlights: %w", err)
		}
	}
	for _, d := range p.Debates {
		polish, err := encodePolish(d.Polish)
		if err != nil {
			return fmt.Errorf("polish highlights: %w", err)
		}
		var id string
		var kind store.HighlightKind
		var dismissed bool
		err = tx.QueryRowContext(ctx, `SELECT id, kind, dismissed FROM highlights WHERE session_id = ? AND seq = ?`, p.SessionID, d.Seq).
			Scan(&id, &kind, &dismissed)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			_, err = tx.ExecContext(ctx, `
INSERT INTO highlights (id, session_id, seq, kind, confidence, dismissed, created_at, polish, polished_at)
VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?)`, d.ID, p.SessionID, d.Seq, d.Kind, d.Confidence, d.CreatedAt.UnixMilli(), polish, at)
			if err != nil {
				return mapErr("polish highlights", err)
			}
		case err != nil:
			return fmt.Errorf("polish highlights: %w", err)
		case kind != store.KindDebate, dismissed:
			// Highlights marked the turn meanwhile, which wins; or the
			// user dismissed the debate.
		default:
			// Decisions' mark keeps its confidence: the agent's is the
			// polish's.
			if _, err := tx.ExecContext(ctx, `UPDATE highlights SET polish = ?, polished_at = ? WHERE id = ?`,
				polish, at, id); err != nil {
				return fmt.Errorf("polish highlights: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("polish highlights: %w", err)
	}
	return nil
}

// encodePolish is p as the polish column keeps it: JSON, empty for none.
func encodePolish(p *store.Polish) (string, error) {
	if p == nil {
		return "", nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encode polish: %w", err)
	}
	return string(b), nil
}

// msOrNull is t in milliseconds, NULL for the zero time.
func msOrNull(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

// DismissHighlight implements store.Store.
func (db *DB) DismissHighlight(ctx context.Context, id string) error {
	if err := store.ValidateID("highlight id", id); err != nil {
		return err
	}
	res, err := db.sql.ExecContext(ctx, `
UPDATE highlights SET dismissed = 1
WHERE id = ? AND session_id IN (SELECT id FROM sessions WHERE deleted_at IS NULL AND trashed_at IS NULL)`, id)
	return requireOne("dismiss highlight", res, err)
}

// current is what an edit may change of a session, as it is.
type current struct {
	title, recap string
	trashed      sql.NullInt64
	ended        bool
}

// value is the edit value of field.
func (c current) value(field store.EditField) string {
	switch field {
	case store.FieldTitle:
		return c.title
	case store.FieldRecap:
		return c.recap
	}
	if !c.trashed.Valid {
		return ""
	}
	return strconv.FormatInt(c.trashed.Int64, 10)
}

// readCurrent reads what an edit may change of a session not purged.
func readCurrent(ctx context.Context, tx *sql.Tx, op, sessionID string) (current, error) {
	var c current
	var pending bool
	err := tx.QueryRowContext(ctx, `
SELECT custom_title, recap, trashed_at, ended_at IS NOT NULL, purge_pending FROM sessions WHERE id = ? AND deleted_at IS NULL`,
		sessionID).Scan(&c.title, &c.recap, &c.trashed, &c.ended, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return current{}, fmt.Errorf("%s: %w", op, store.ErrNotFound)
	}
	if err != nil {
		return current{}, fmt.Errorf("%s: %w", op, err)
	}
	if pending {
		return current{}, fmt.Errorf("%s: erasure has started: %w", op, store.ErrConflict)
	}
	return c, nil
}

// setField writes value to field of the session.
func setField(ctx context.Context, tx *sql.Tx, sessionID string, field store.EditField, value string, now int64) error {
	var err error
	switch field {
	case store.FieldTitle:
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET custom_title = ?, updated_at = ? WHERE id = ?`, value, now, sessionID)
	case store.FieldRecap:
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET recap = ?, updated_at = ? WHERE id = ?`, value, now, sessionID)
	case store.FieldTrash:
		var at sql.NullInt64
		if value != "" {
			ms, perr := strconv.ParseInt(value, 10, 64)
			if perr != nil {
				return fmt.Errorf("trash value: %w", perr)
			}
			at = sql.NullInt64{Int64: ms, Valid: true}
		}
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET trashed_at = ?, updated_at = ? WHERE id = ?`, at, now, sessionID)
	default:
		return fmt.Errorf("%w: field is unknown", store.ErrInvalid)
	}
	return err
}

// editSession changes field of a session in one transaction, recording
// the edit with the value it replaces. next returns the new value from
// the session as it is, or why the edit is refused.
func (db *DB) editSession(ctx context.Context, op, sessionID string, field store.EditField, actor, origin string,
	next func(c current) (string, error)) (store.Edit, error) {
	if err := store.ValidateID("session id", sessionID); err != nil {
		return store.Edit{}, err
	}
	if err := store.ValidateTag("actor", actor, true); err != nil {
		return store.Edit{}, err
	}
	if err := store.ValidateTag("origin", origin, false); err != nil {
		return store.Edit{}, err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return store.Edit{}, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()

	c, err := readCurrent(ctx, tx, op, sessionID)
	if err != nil {
		return store.Edit{}, err
	}
	value, err := next(c)
	if err != nil {
		return store.Edit{}, fmt.Errorf("%s: %w", op, err)
	}
	now := nowMs()
	e := store.Edit{ID: uuid.NewString(), SessionID: sessionID, Field: field, OldValue: c.value(field), NewValue: value,
		Actor: actor, Origin: origin, CreatedAt: time.UnixMilli(now).UTC()}
	if err := setField(ctx, tx, sessionID, field, value, now); err != nil {
		return store.Edit{}, fmt.Errorf("%s: %w", op, err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO session_edits (id, session_id, field, old_value, new_value, actor, origin, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, e.ID, e.SessionID, e.Field, e.OldValue, e.NewValue, e.Actor, e.Origin, now); err != nil {
		return store.Edit{}, mapErr(op, err)
	}
	if err := tx.Commit(); err != nil {
		return store.Edit{}, fmt.Errorf("%s: %w", op, err)
	}
	return e, nil
}

// notTrashed refuses an edit to a session in Recently Deleted, which is
// missing to all but restoring it.
func notTrashed(c current) error {
	if c.trashed.Valid {
		return store.ErrNotFound
	}
	return nil
}

// RenameSession implements store.Store.
func (db *DB) RenameSession(ctx context.Context, sessionID, title, actor, origin string) (store.Edit, error) {
	if err := store.ValidateTitle(title); err != nil {
		return store.Edit{}, err
	}
	return db.editSession(ctx, "rename session", sessionID, store.FieldTitle, actor, origin, func(c current) (string, error) {
		return title, notTrashed(c)
	})
}

// UpdateRecap implements store.Store.
func (db *DB) UpdateRecap(ctx context.Context, sessionID, recap, actor, origin string) (store.Edit, error) {
	if err := store.ValidateRecapText(recap); err != nil {
		return store.Edit{}, err
	}
	return db.editSession(ctx, "update recap", sessionID, store.FieldRecap, actor, origin, func(c current) (string, error) {
		return recap, notTrashed(c)
	})
}

// TrashSession implements store.Store. A session being recorded is
// refused, so that no one can hide a recording from the person making it.
func (db *DB) TrashSession(ctx context.Context, sessionID, actor, origin string) (store.Edit, error) {
	return db.editSession(ctx, "trash session", sessionID, store.FieldTrash, actor, origin, func(c current) (string, error) {
		if err := notTrashed(c); err != nil {
			return "", err
		}
		if !c.ended {
			return "", store.ErrConflict
		}
		return strconv.FormatInt(nowMs(), 10), nil
	})
}

// RestoreSession implements store.Store.
func (db *DB) RestoreSession(ctx context.Context, sessionID, actor, origin string) (store.Edit, error) {
	return db.editSession(ctx, "restore session", sessionID, store.FieldTrash, actor, origin, func(c current) (string, error) {
		if !c.trashed.Valid {
			return "", store.ErrNotFound
		}
		return "", nil
	})
}

// UndoEdit implements store.Store.
func (db *DB) UndoEdit(ctx context.Context, editID string) (store.Edit, error) {
	const op = "undo edit"
	if err := store.ValidateID("edit id", editID); err != nil {
		return store.Edit{}, err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return store.Edit{}, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()

	e, err := scanEdit(tx.QueryRowContext(ctx, `SELECT `+editCols+` FROM session_edits WHERE id = ?`, editID))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Edit{}, fmt.Errorf("%s: %w", op, store.ErrNotFound)
	}
	if err != nil {
		return store.Edit{}, fmt.Errorf("%s: %w", op, err)
	}
	if !e.UndoneAt.IsZero() {
		return store.Edit{}, fmt.Errorf("%s: undone already: %w", op, store.ErrConflict)
	}
	c, err := readCurrent(ctx, tx, op, e.SessionID)
	if err != nil {
		return store.Edit{}, err
	}
	if e.Field != store.FieldTrash && c.trashed.Valid {
		return store.Edit{}, fmt.Errorf("%s: %w", op, store.ErrNotFound)
	}
	if c.value(e.Field) != e.NewValue {
		return store.Edit{}, fmt.Errorf("%s: changed since: %w", op, store.ErrConflict)
	}
	now := nowMs()
	if err := setField(ctx, tx, e.SessionID, e.Field, e.OldValue, now); err != nil {
		return store.Edit{}, fmt.Errorf("%s: %w", op, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE session_edits SET undone_at = ? WHERE id = ?`, now, e.ID); err != nil {
		return store.Edit{}, fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return store.Edit{}, fmt.Errorf("%s: %w", op, err)
	}
	e.UndoneAt = time.UnixMilli(now).UTC()
	return e, nil
}

// ClaimPurge implements store.Store. The update and any restore serialize
// across connections: whichever commits first determines whether erasure
// may begin. The claim survives hook failures and process exits.
func (db *DB) ClaimPurge(ctx context.Context, sessionID string) error {
	if err := store.ValidateID("session id", sessionID); err != nil {
		return err
	}
	res, err := db.sql.ExecContext(ctx, `
UPDATE sessions SET purge_pending = 1
WHERE id = ? AND ((deleted_at IS NULL AND trashed_at IS NOT NULL) OR purge_pending = 1)`, sessionID)
	return requireOne("claim purge", res, err)
}

// PurgeSession implements store.Store. It leaves a tombstone row for
// future record-level sync, but erases the transcript, translations,
// titles, file name, recap, highlights, edits and its file of a batch: a user who
// deletes a session expects the private text to be gone. Only a trashed session is purged, so that a
// purge of the sessions found expired misses one restored meanwhile.
func (db *DB) PurgeSession(ctx context.Context, sessionID string) error {
	if err := db.ClaimPurge(ctx, sessionID); err != nil {
		return err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("purge session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := nowMs()
	res, err := tx.ExecContext(ctx, `
UPDATE sessions SET title = '', custom_title = '', file_name = '', tracking_id = '', recap = '', deleted_at = ?, updated_at = ?
WHERE id = ? AND purge_pending = 1`, now, now, sessionID)
	if err := requireOne("purge session", res, err); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE segments SET text = '', translation = '', translation_lang = '', updated_at = ?
WHERE session_id = ?`, now, sessionID); err != nil {
		return fmt.Errorf("purge session: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_edits WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("purge session: %w", err)
	}
	// The session's row stays, a tombstone: its highlights do not go with it.
	if _, err := tx.ExecContext(ctx, `DELETE FROM highlights WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("purge session: %w", err)
	}
	if err := forgetFile(ctx, tx, sessionID); err != nil {
		return fmt.Errorf("purge session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("purge session: %w", err)
	}
	// Move the erased pages out of the write-ahead log, which still holds
	// their old content, into the zeroed database file. A reader in another
	// process can keep the log from being emptied, which SQLite reports as
	// a result, not an error: keep the claim so a retry empties the log.
	var busy, frames, moved int
	if err := db.sql.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &frames, &moved); err != nil {
		return fmt.Errorf("purge session: checkpoint: %w", err)
	}
	if busy != 0 {
		return fmt.Errorf("purge session: checkpoint: %w", errCheckpointBusy)
	}
	if _, err := db.sql.ExecContext(ctx, `UPDATE sessions SET purge_pending = 0 WHERE id = ?`, sessionID); err != nil {
		return fmt.Errorf("purge session: finish: %w", err)
	}
	return nil
}

// errCheckpointBusy is a purge whose erased text may remain in the
// write-ahead log until a later checkpoint.
var errCheckpointBusy = errors.New("another connection kept the write-ahead log")

// forgetFile deletes the file of a batch that was saved as session
// sessionID, if any, and the batch once it has no file left.
func forgetFile(ctx context.Context, tx *sql.Tx, sessionID string) error {
	var batchID string
	err := tx.QueryRowContext(ctx, `SELECT batch_id FROM batch_files WHERE session_id = ?`, sessionID).Scan(&batchID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM batch_files WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	return forgetEmptyBatch(ctx, tx, batchID)
}
