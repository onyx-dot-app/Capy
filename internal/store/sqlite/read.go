package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/onyx-dot-app/Capy/internal/store"
)

// summaryCols must match scanSummary. A rename wins over the recap's
// title. The count excludes gap segments.
const summaryCols = `s.id, s.mode, s.source, s.lang_a, s.lang_b,
	CASE WHEN s.custom_title <> '' THEN s.custom_title ELSE s.title END, s.file_name, s.batch_id,
	s.recap_status, s.started_at, s.ended_at,
	(SELECT COUNT(*) FROM segments g WHERE g.session_id = s.id AND g.lang <> '` + store.GapLang + `'),
	EXISTS (SELECT 1 FROM segments t WHERE t.session_id = s.id AND t.translation_lang <> ''),
	s.heartbeat_at`

// shown is the condition of the sessions every read but ListTrashed
// shows: neither purged nor trashed.
const shown = `s.deleted_at IS NULL AND s.trashed_at IS NULL`

type scanner interface{ Scan(dest ...any) error }

// scanSummary scans summaryCols followed by any extra destinations.
func scanSummary(r scanner, extra ...any) (store.SessionSummary, error) {
	var (
		s              store.SessionSummary
		started        int64
		ended, beating sql.NullInt64
	)
	dest := append([]any{&s.ID, &s.Mode, &s.Source, &s.LangA, &s.LangB, &s.Title, &s.FileName, &s.BatchID, &s.RecapStatus, &started, &ended,
		&s.SegmentCount, &s.Translated, &beating}, extra...)
	if err := r.Scan(dest...); err != nil {
		return store.SessionSummary{}, err
	}
	s.StartedAt = time.UnixMilli(started).UTC()
	s.EndedAt = timeOf(ended)
	s.HeartbeatAt = timeOf(beating)
	return s, nil
}

// timeOf is the time of a nullable Unix millisecond column, zero for NULL.
func timeOf(ms sql.NullInt64) time.Time {
	if !ms.Valid {
		return time.Time{}
	}
	return time.UnixMilli(ms.Int64).UTC()
}

func collectSummaries(op string, rows *sql.Rows) ([]store.SessionSummary, error) {
	defer rows.Close()
	out := []store.SessionSummary{}
	for rows.Next() {
		s, err := scanSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}

// ListSessions implements store.Store.
func (db *DB) ListSessions(ctx context.Context, q store.ListQuery) ([]store.SessionSummary, error) {
	q, err := q.Normalize()
	if err != nil {
		return nil, err
	}
	rows, err := db.sql.QueryContext(ctx, `
SELECT `+summaryCols+` FROM sessions s
WHERE `+shown+` AND (? = 0 OR s.batch_id = '')
ORDER BY s.started_at DESC, s.id DESC
LIMIT ? OFFSET ?`, q.Unbatched, q.Limit, q.Offset)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return collectSummaries("list sessions", rows)
}

// GetSession implements store.Store.
func (db *DB) GetSession(ctx context.Context, sessionID string) (store.SessionDetail, error) {
	if err := store.ValidateID("session id", sessionID); err != nil {
		return store.SessionDetail{}, err
	}
	var d store.SessionDetail
	row := db.sql.QueryRowContext(ctx, `
SELECT `+summaryCols+`, s.recap, s.tracking_id FROM sessions s WHERE s.id = ? AND `+shown, sessionID)
	sum, err := scanSummary(row, &d.Recap, &d.TrackingID)
	if errors.Is(err, sql.ErrNoRows) {
		return store.SessionDetail{}, fmt.Errorf("get session: %w", store.ErrNotFound)
	}
	if err != nil {
		return store.SessionDetail{}, fmt.Errorf("get session: %w", err)
	}
	d.SessionSummary = sum

	// The pool has one connection, so the session row above is fully read
	// before this query starts.
	rows, err := db.sql.QueryContext(ctx, `
SELECT id, session_id, seq, lang, text, speaker, start_ms, end_ms, translation, translation_lang
FROM segments WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return store.SessionDetail{}, fmt.Errorf("get session: %w", err)
	}
	defer rows.Close()
	d.Segments = []store.Segment{}
	for rows.Next() {
		var g store.Segment
		if err := rows.Scan(&g.ID, &g.SessionID, &g.Seq, &g.Lang, &g.Text, &g.Speaker, &g.StartMs, &g.EndMs, &g.Translation, &g.TranslationLang); err != nil {
			return store.SessionDetail{}, fmt.Errorf("get session: %w", err)
		}
		d.Segments = append(d.Segments, g)
	}
	if err := rows.Err(); err != nil {
		return store.SessionDetail{}, fmt.Errorf("get session: %w", err)
	}
	if d.Highlights, err = db.highlights(ctx, sessionID); err != nil {
		return store.SessionDetail{}, fmt.Errorf("get session: %w", err)
	}
	return d, nil
}

// highlights are a session's highlights, ordered by seq.
func (db *DB) highlights(ctx context.Context, sessionID string) ([]store.Highlight, error) {
	rows, err := db.sql.QueryContext(ctx, `
SELECT id, session_id, seq, kind, confidence, dismissed, created_at, polish, polished_at FROM highlights WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.Highlight{}
	for rows.Next() {
		var h store.Highlight
		var created int64
		var polish string
		var polished sql.NullInt64
		if err := rows.Scan(&h.ID, &h.SessionID, &h.Seq, &h.Kind, &h.Confidence, &h.Dismissed, &created, &polish, &polished); err != nil {
			return nil, err
		}
		h.CreatedAt, h.PolishedAt = time.UnixMilli(created), timeOf(polished)
		if polish != "" {
			h.Polish = new(store.Polish)
			if err := json.Unmarshal([]byte(polish), h.Polish); err != nil {
				return nil, fmt.Errorf("highlight %s: decode polish: %w", h.ID, err)
			}
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ListTrashed implements store.Store.
func (db *DB) ListTrashed(ctx context.Context) ([]store.TrashedSession, error) {
	rows, err := db.sql.QueryContext(ctx, `
SELECT `+summaryCols+`, s.trashed_at, s.purge_pending FROM sessions s
WHERE (s.deleted_at IS NULL AND s.trashed_at IS NOT NULL) OR s.purge_pending = 1
ORDER BY s.trashed_at DESC, s.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list trashed: %w", err)
	}
	defer rows.Close()
	out := []store.TrashedSession{}
	for rows.Next() {
		var at int64
		var pending bool
		sum, err := scanSummary(rows, &at, &pending)
		if err != nil {
			return nil, fmt.Errorf("list trashed: %w", err)
		}
		out = append(out, store.TrashedSession{SessionSummary: sum, TrashedAt: time.UnixMilli(at).UTC(), PurgePending: pending})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list trashed: %w", err)
	}
	return out, nil
}

// ListExpired implements store.Store.
func (db *DB) ListExpired(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := db.sql.QueryContext(ctx, `
SELECT id FROM sessions WHERE (deleted_at IS NULL AND trashed_at < ?) OR purge_pending = 1 ORDER BY trashed_at`,
		now.Add(-store.TrashRetention).UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("list expired: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list expired: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list expired: %w", err)
	}
	return out, nil
}

// editCols must match scanEdit.
const editCols = `id, session_id, field, old_value, new_value, actor, origin, created_at, undone_at`

func scanEdit(r scanner) (store.Edit, error) {
	var (
		e       store.Edit
		created int64
		undone  sql.NullInt64
	)
	if err := r.Scan(&e.ID, &e.SessionID, &e.Field, &e.OldValue, &e.NewValue, &e.Actor, &e.Origin, &created, &undone); err != nil {
		return store.Edit{}, err
	}
	e.CreatedAt, e.UndoneAt = time.UnixMilli(created).UTC(), timeOf(undone)
	return e, nil
}

// ListEdits implements store.Store. The edits of purged sessions are gone
// with them.
func (db *DB) ListEdits(ctx context.Context, origin string) ([]store.Edit, error) {
	if err := store.ValidateTag("origin", origin, true); err != nil {
		return nil, err
	}
	rows, err := db.sql.QueryContext(ctx, `
SELECT `+editCols+` FROM session_edits WHERE origin = ? ORDER BY created_at, rowid`, origin)
	if err != nil {
		return nil, fmt.Errorf("list edits: %w", err)
	}
	defer rows.Close()
	out := []store.Edit{}
	for rows.Next() {
		e, err := scanEdit(rows)
		if err != nil {
			return nil, fmt.Errorf("list edits: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list edits: %w", err)
	}
	return out, nil
}

// GetEdit implements store.Store.
func (db *DB) GetEdit(ctx context.Context, id string) (store.Edit, error) {
	if err := store.ValidateID("edit id", id); err != nil {
		return store.Edit{}, err
	}
	e, err := scanEdit(db.sql.QueryRowContext(ctx, `SELECT `+editCols+` FROM session_edits WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return store.Edit{}, fmt.Errorf("get edit: %w", store.ErrNotFound)
	}
	if err != nil {
		return store.Edit{}, fmt.Errorf("get edit: %w", err)
	}
	return e, nil
}

// DataVersion implements store.Store with PRAGMA data_version, which
// changes as other connections commit. The pool's one connection keeps
// the count; should the pool replace it, the version changes once.
func (db *DB) DataVersion(ctx context.Context) (int64, error) {
	var v int64
	if err := db.sql.QueryRowContext(ctx, "PRAGMA data_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("data version: %w", err)
	}
	return v, nil
}
