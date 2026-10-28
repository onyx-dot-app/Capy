package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/store"
)

// CreateBatch implements store.Store.
func (db *DB) CreateBatch(ctx context.Context, b store.Batch, files []store.BatchFile) error {
	if err := store.ValidateBatch(b, files); err != nil {
		return err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("create batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO batches (id, name, lang, target, created_at) VALUES (?, ?, ?, ?, ?)`,
		b.ID, b.Name, b.Lang, b.Target, b.CreatedAt.UnixMilli()); err != nil {
		return mapErr("create batch", err)
	}
	for i, f := range files {
		var recorded sql.NullInt64
		if !f.RecordedAt.IsZero() {
			recorded = sql.NullInt64{Int64: f.RecordedAt.UnixMilli(), Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO batch_files (id, batch_id, seq, path, name, size, duration_ms, recorded_at, state, title)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			f.ID, b.ID, i, f.Path, f.Name, f.Size, f.Duration.Milliseconds(), recorded, store.FileWaiting, f.Title); err != nil {
			return mapErr("create batch", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("create batch: %w", err)
	}
	return nil
}

// fileCols must match scanFile. A saved file's session follows, as
// summaryCols, NULL when it is not listed: in Recently Deleted or purged.
const fileCols = `f.id, f.batch_id, f.seq, f.path, f.name, f.size, f.duration_ms, f.recorded_at, f.state, f.problem,
	f.title, f.session_id`

// listedFile is the condition of the files a batch lists: those not saved,
// and those whose session shows.
const listedFile = `(f.session_id = '' OR s.id IS NOT NULL)`

// nullSummaryCols is summaryCols of a session joined that may be missing.
const nullSummaryCols = `s.id, s.mode, s.source, s.lang_a, s.lang_b,
	CASE WHEN s.custom_title <> '' THEN s.custom_title ELSE s.title END, s.file_name, s.batch_id,
	s.recap_status, s.started_at, s.ended_at,
	(SELECT COUNT(*) FROM segments g WHERE g.session_id = s.id AND g.lang <> '` + store.GapLang + `'),
	EXISTS (SELECT 1 FROM segments t WHERE t.session_id = s.id AND t.translation_lang <> ''),
	s.heartbeat_at`

// scanFile scans fileCols, then nullSummaryCols, then extra.
func scanFile(r scanner, extra ...any) (store.BatchFile, error) {
	var (
		f                        store.BatchFile
		durationMs               int64
		recorded, started, ended sql.NullInt64
		beating                  sql.NullInt64
		sid, mode, source        sql.NullString
		langA, langB, title      sql.NullString
		fileName, batchID        sql.NullString
		recapStatus              sql.NullString
		count                    sql.NullInt64
		translated               sql.NullBool
	)
	dest := append([]any{&f.ID, &f.BatchID, &f.Seq, &f.Path, &f.Name, &f.Size, &durationMs, &recorded, &f.State, &f.Problem,
		&f.Title, &f.SessionID,
		&sid, &mode, &source, &langA, &langB, &title, &fileName, &batchID, &recapStatus, &started, &ended, &count, &translated, &beating},
		extra...)
	if err := r.Scan(dest...); err != nil {
		return store.BatchFile{}, err
	}
	f.Duration = time.Duration(durationMs) * time.Millisecond
	f.RecordedAt = timeOf(recorded)
	if sid.Valid {
		f.Session = store.SessionSummary{ID: sid.String, Mode: store.Mode(mode.String), Source: store.Source(source.String),
			LangA: langA.String, LangB: langB.String, Title: title.String, FileName: fileName.String, BatchID: batchID.String,
			RecapStatus: store.RecapStatus(recapStatus.String), StartedAt: timeOf(started), EndedAt: timeOf(ended),
			SegmentCount: int(count.Int64), Translated: translated.Bool, HeartbeatAt: timeOf(beating)}
	}
	return f, nil
}

// readBatches reads the batches where cond holds, with their files, the
// newest first; withRecap reads each saved file's recap too.
func (db *DB) readBatches(ctx context.Context, op, cond string, withRecap bool, args ...any) ([]store.BatchSummary, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT id, name, lang, target, created_at FROM batches WHERE `+cond+`
ORDER BY created_at DESC, id DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	var out []store.BatchSummary
	index := make(map[string]int)
	for rows.Next() {
		var b store.BatchSummary
		var created int64
		if err := rows.Scan(&b.ID, &b.Name, &b.Lang, &b.Target, &created); err != nil {
			rows.Close()
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		b.CreatedAt = time.UnixMilli(created).UTC()
		index[b.ID] = len(out)
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	rows.Close()
	if len(out) == 0 {
		return out, nil
	}

	recapCol := `''`
	if withRecap {
		recapCol = `COALESCE(s.recap, '')`
	}
	// The pool has one connection: the batches are read before this starts.
	rows, err = db.sql.QueryContext(ctx, `
SELECT `+fileCols+`, `+nullSummaryCols+`, `+recapCol+`
FROM batch_files f
JOIN batches b ON b.id = f.batch_id
LEFT JOIN sessions s ON s.id = f.session_id AND f.session_id <> '' AND `+shown+`
WHERE `+listedFile+` AND b.id IN (SELECT id FROM batches WHERE `+cond+`)
ORDER BY f.batch_id, f.seq`, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer rows.Close()
	for rows.Next() {
		var recap string
		f, err := scanFile(rows, &recap)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		f.Recap = recap
		i := index[f.BatchID]
		out[i].Files = append(out[i].Files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	// A batch whose every file is in Recently Deleted shows no more.
	listed := out[:0]
	for _, b := range out {
		if len(b.Files) > 0 {
			listed = append(listed, b)
		}
	}
	return listed, nil
}

// ListBatches implements store.Store.
func (db *DB) ListBatches(ctx context.Context) ([]store.BatchSummary, error) {
	return db.readBatches(ctx, "list batches", "1", false)
}

// GetBatch implements store.Store.
func (db *DB) GetBatch(ctx context.Context, id string) (store.BatchSummary, error) {
	if err := store.ValidateID("batch id", id); err != nil {
		return store.BatchSummary{}, err
	}
	list, err := db.readBatches(ctx, "get batch", "id = ?", true, id)
	if err != nil {
		return store.BatchSummary{}, err
	}
	if len(list) == 0 {
		return store.BatchSummary{}, fmt.Errorf("get batch: %w", store.ErrNotFound)
	}
	return list[0], nil
}

// SetFileState implements store.Store.
func (db *DB) SetFileState(ctx context.Context, fileID string, state store.FileState, problem string) error {
	if err := store.ValidateFileState(fileID, state, problem); err != nil {
		return err
	}
	return db.changeUnsaved(ctx, "set file state", fileID, `state = ?, problem = ?`, state, problem)
}

// RenameFile implements store.Store.
func (db *DB) RenameFile(ctx context.Context, fileID, title string) error {
	if err := store.ValidateID("file id", fileID); err != nil {
		return err
	}
	if err := store.ValidateTitle(title); err != nil {
		return err
	}
	return db.changeUnsaved(ctx, "rename file", fileID, `title = ?`, title)
}

// changeUnsaved sets columns of a file not saved: a missing file is
// ErrNotFound, a saved one ErrConflict.
func (db *DB) changeUnsaved(ctx context.Context, op, fileID, set string, args ...any) error {
	res, err := db.sql.ExecContext(ctx, `UPDATE batch_files SET `+set+` WHERE id = ? AND state <> ?`,
		append(args, fileID, store.FileSaved)...)
	if err := requireOne(op, res, err); !errors.Is(err, store.ErrNotFound) {
		return err
	}
	var exists bool
	if err := db.sql.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM batch_files WHERE id = ?)`, fileID).Scan(&exists); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if exists {
		return fmt.Errorf("%s: saved: %w", op, store.ErrConflict)
	}
	return fmt.Errorf("%s: %w", op, store.ErrNotFound)
}

// SaveFile implements store.Store. The session joins the file's batch, and
// the file's title becomes its rename, without an edit: it was named
// before it was a session.
func (db *DB) SaveFile(ctx context.Context, fileID, sessionID string) error {
	const op = "save file"
	if err := store.ValidateID("file id", fileID); err != nil {
		return err
	}
	if err := store.ValidateID("session id", sessionID); err != nil {
		return err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	var batchID, title string
	var state store.FileState
	err = tx.QueryRowContext(ctx, `SELECT batch_id, title, state FROM batch_files WHERE id = ?`, fileID).Scan(&batchID, &title, &state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Taken out of its batch while transcribed: the session stands on
		// its own.
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET batch_id = '', updated_at = ? WHERE id = ? AND source = ?`,
			nowMs(), sessionID, store.SourceFile); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		return fmt.Errorf("%s: %w", op, store.ErrNotFound)
	case err != nil:
		return fmt.Errorf("%s: %w", op, err)
	case state == store.FileSaved:
		return fmt.Errorf("%s: saved already: %w", op, store.ErrConflict)
	}
	res, err := tx.ExecContext(ctx, `
UPDATE sessions SET batch_id = ?, custom_title = CASE WHEN ? <> '' THEN ? ELSE custom_title END, updated_at = ?
WHERE id = ? AND source = ? AND deleted_at IS NULL`, batchID, title, title, nowMs(), sessionID, store.SourceFile)
	if err := requireOne(op, res, err); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE batch_files SET state = ?, problem = '', path = '', title = '', session_id = ? WHERE id = ?`,
		store.FileSaved, sessionID, fileID); err != nil {
		return mapErr(op, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// RenameBatch implements store.Store.
func (db *DB) RenameBatch(ctx context.Context, id, name string) error {
	if err := store.ValidateID("batch id", id); err != nil {
		return err
	}
	if err := store.ValidateBatchName(name); err != nil {
		return err
	}
	res, err := db.sql.ExecContext(ctx, `UPDATE batches SET name = ? WHERE id = ?`, name, id)
	return requireOne("rename batch", res, err)
}

// RemoveFile implements store.Store.
func (db *DB) RemoveFile(ctx context.Context, fileID string) error {
	const op = "remove file"
	if err := store.ValidateID("file id", fileID); err != nil {
		return err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	var batchID, sessionID string
	err = tx.QueryRowContext(ctx, `SELECT batch_id, session_id FROM batch_files WHERE id = ?`, fileID).Scan(&batchID, &sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", op, store.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if sessionID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET batch_id = '', updated_at = ? WHERE id = ?`, nowMs(), sessionID); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM batch_files WHERE id = ?`, fileID); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if err := forgetEmptyBatch(ctx, tx, batchID); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// forgetEmptyBatch deletes batch id once it has no file left: its name is
// the user's text.
func forgetEmptyBatch(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM batches WHERE id = ? AND NOT EXISTS (SELECT 1 FROM batch_files WHERE batch_id = ?)`,
		id, id)
	return err
}

// forgetBatch deletes batch id and its files, its sessions standing on
// their own.
func forgetBatch(ctx context.Context, tx *sql.Tx, id string, now int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET batch_id = '', updated_at = ? WHERE batch_id = ?`, now, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM batch_files WHERE batch_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM batches WHERE id = ?`, id)
	return requireOne("forget batch", res, err)
}

// UngroupBatch implements store.Store.
func (db *DB) UngroupBatch(ctx context.Context, id string) error {
	if err := store.ValidateID("batch id", id); err != nil {
		return err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ungroup batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := forgetBatch(ctx, tx, id, nowMs()); err != nil {
		return fmt.Errorf("ungroup batch: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ungroup batch: %w", err)
	}
	return nil
}

// DeleteBatch implements store.Store: one transaction trashes every ended
// session of the batch, with its edit, and forgets the batch.
func (db *DB) DeleteBatch(ctx context.Context, id, actor, origin string) ([]store.Edit, error) {
	const op = "delete batch"
	if err := store.ValidateID("batch id", id); err != nil {
		return nil, err
	}
	if err := store.ValidateTag("actor", actor, true); err != nil {
		return nil, err
	}
	if err := store.ValidateTag("origin", origin, false); err != nil {
		return nil, err
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
SELECT s.id FROM batch_files f JOIN sessions s ON s.id = f.session_id
WHERE f.batch_id = ? AND f.session_id <> '' AND `+shown+` AND s.ended_at IS NOT NULL AND s.purge_pending = 0
ORDER BY f.seq`, id)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	var ids []string
	for rows.Next() {
		var sid string
		if err := rows.Scan(&sid); err != nil {
			rows.Close()
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		ids = append(ids, sid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	now := nowMs()
	at := strconv.FormatInt(now, 10)
	edits := make([]store.Edit, 0, len(ids))
	for _, sid := range ids {
		e := store.Edit{ID: uuid.NewString(), SessionID: sid, Field: store.FieldTrash, NewValue: at,
			Actor: actor, Origin: origin, CreatedAt: time.UnixMilli(now).UTC()}
		if err := setField(ctx, tx, sid, store.FieldTrash, at, now); err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO session_edits (id, session_id, field, old_value, new_value, actor, origin, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, e.ID, e.SessionID, e.Field, e.OldValue, e.NewValue, e.Actor, e.Origin, now); err != nil {
			return nil, mapErr(op, err)
		}
		edits = append(edits, e)
	}
	if err := forgetBatch(ctx, tx, id, now); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return edits, nil
}
