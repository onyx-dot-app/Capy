package sqlite

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/onyx-dot-app/Capy/internal/store"
)

// minTrigramRunes is the shortest word the trigram tokenizer can index.
const minTrigramRunes = 3

// Search implements store.Store.
//
// Every word must occur in the same segment, in its text or translation.
// Words of three or more characters go to the FTS index as quoted strings
// (quotes doubled, so FTS operators in user input are inert); shorter words
// are matched with LIKE. All user values are bound parameters; only the number
// of LIKE terms is built into the SQL text.
func (db *DB) Search(ctx context.Context, query string, limit int) ([]store.SessionSummary, error) {
	return db.SearchRange(ctx, query, time.Time{}, time.Time{}, limit)
}

// SearchRange implements store.Store.
func (db *DB) SearchRange(ctx context.Context, query string, from, to time.Time, limit int) ([]store.SessionSummary, error) {
	if err := store.ValidateSearchQuery(query); err != nil {
		return nil, err
	}
	limit, err := store.NormalizeSearchLimit(limit)
	if err != nil {
		return nil, err
	}
	words := strings.Fields(query)
	if len(words) == 0 {
		return []store.SessionSummary{}, nil
	}

	var (
		match []string
		args  []any
		where strings.Builder
	)
	for _, w := range words {
		if utf8.RuneCountInString(w) >= minTrigramRunes {
			match = append(match, `"`+strings.ReplaceAll(w, `"`, `""`)+`"`)
			continue
		}
		where.WriteString(` AND (g.text LIKE ? ESCAPE '\' OR g.translation LIKE ? ESCAPE '\')`)
		pat := "%" + likeEscaper.Replace(w) + "%"
		args = append(args, pat, pat)
	}

	var sqlText strings.Builder
	var all []any
	sqlText.WriteString(`SELECT ` + summaryCols + ` FROM sessions s
WHERE ` + shown)
	if !from.IsZero() {
		sqlText.WriteString(` AND s.started_at >= ?`)
		all = append(all, from.UnixMilli())
	}
	if !to.IsZero() {
		sqlText.WriteString(` AND s.started_at <= ?`)
		all = append(all, to.UnixMilli())
	}
	sqlText.WriteString(` AND EXISTS (
	SELECT 1 FROM segments g WHERE g.session_id = s.id`)
	if len(match) > 0 {
		sqlText.WriteString(` AND g.rid IN (SELECT rowid FROM segments_fts WHERE segments_fts MATCH ?)`)
		all = append(all, strings.Join(match, " AND "))
	}
	sqlText.WriteString(where.String())
	sqlText.WriteString(`)
ORDER BY s.started_at DESC, s.id DESC
LIMIT ?`)
	all = append(all, args...)
	all = append(all, limit)

	rows, err := db.sql.QueryContext(ctx, sqlText.String(), all...)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	return collectSummaries("search", rows)
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
