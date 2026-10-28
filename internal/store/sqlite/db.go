// Package sqlite implements store.Store on SQLite via modernc.org/sqlite
// (pure Go, no cgo).
//
// Search uses an FTS5 index with the trigram tokenizer: it matches any
// substring of three or more characters, so it works for CJK text without
// word boundaries and makes prefix matching of the last word implicit. Words
// shorter than three characters cannot be indexed by trigram, so they are
// matched with LIKE over the same segments.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/onyx-dot-app/Capy/internal/store"

	msqlite "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Result codes; extended codes carry them in the low byte.
const (
	sqliteBusy       = 5
	sqliteConstraint = 19
)

// busyTimeout is how long a connection waits for another's lock.
const busyTimeout = 5 * time.Second

// DB is a SQLite-backed store.Store.
type DB struct {
	sql *sql.DB
}

var _ store.Store = (*DB)(nil)

// Open opens or creates the database at path and applies pending migrations.
// The parent directory is created with mode 0700 and the file with 0600
// because it holds private transcripts.
func Open(ctx context.Context, path string) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("sqlite: resolve path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, fmt.Errorf("sqlite: create directory: %w", err)
	}
	f, err := os.OpenFile(abs, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("sqlite: create file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("sqlite: create file: %w", err)
	}

	// Writes take the write lock as they begin, so that one waits for
	// another process's instead of failing as a read turns into a write.
	q := url.Values{"_txlock": {"immediate"}}
	for _, p := range []string{
		// First, so that the pragmas after it wait for another process
		// opening the file too, as switching it to WAL does.
		"busy_timeout(" + strconv.Itoa(int(busyTimeout.Milliseconds())) + ")",
		"journal_mode(WAL)",
		"foreign_keys(ON)",
		"synchronous(NORMAL)",
		// Zero freed pages, so deleted transcripts do not linger in the file.
		"secure_delete(ON)",
	} {
		q.Add("_pragma", p)
	}
	dsn := (&url.URL{Scheme: "file", Path: abs, RawQuery: q.Encode()}).String()

	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}
	// One connection serializes all access. Writes are tiny (one segment per
	// completed turn), so queueing is cheaper than reasoning about SQLITE_BUSY
	// and lock upgrades between pooled connections; busy_timeout still covers
	// a second process.
	sdb.SetMaxOpenConns(1)
	if err := connect(ctx, sdb); err != nil {
		_ = sdb.Close()
		return nil, err
	}

	db := &DB{sql: sdb}
	if err := db.migrate(ctx); err != nil {
		_ = sdb.Close()
		return nil, err
	}
	return db, nil
}

// connect opens the pool's connection. A new file turns to WAL as its
// first connection opens, which another process's, opening it at the same
// time, cannot wait for as it waits for locks: it tries again instead,
// for as long as it would wait for a lock.
func connect(ctx context.Context, sdb *sql.DB) error {
	deadline := time.Now().Add(busyTimeout)
	for {
		err := sdb.PingContext(ctx)
		var se *msqlite.Error
		if err == nil || !errors.As(err, &se) || se.Code()&0xff != sqliteBusy || time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("sqlite: connect: %w", err)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("sqlite: connect: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Close closes the database.
func (db *DB) Close() error {
	return db.sql.Close()
}

// migrate applies the embedded migrations that PRAGMA user_version has not
// seen yet, in one transaction. Migration N is migrations/NNN_*.sql. The
// transaction is BEGIN IMMEDIATE, on a connection of its own, so that of
// two processes opening the file together, one applies a migration and
// the other then finds it applied.
func (db *DB) migrate(ctx context.Context) (err error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("sqlite: read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	conn, err := db.sql.Conn(ctx)
	if err != nil {
		return fmt.Errorf("sqlite: connect: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("sqlite: begin migration: %w", err)
	}
	defer func() {
		if err != nil {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()

	var current int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("sqlite: read schema version: %w", err)
	}
	if current > len(names) {
		return fmt.Errorf("sqlite: database schema version %d is newer than this build (%d)", current, len(names))
	}
	for i := current; i < len(names); i++ {
		body, err := migrationFS.ReadFile("migrations/" + names[i])
		if err != nil {
			return fmt.Errorf("sqlite: read migration %s: %w", names[i], err)
		}
		if _, err := conn.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("sqlite: apply migration %s: %w", names[i], err)
		}
	}
	if current < len(names) {
		// PRAGMA does not accept bound parameters; the value is our own integer.
		if _, err := conn.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(len(names))); err != nil {
			return fmt.Errorf("sqlite: set schema version: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("sqlite: commit migration: %w", err)
	}
	return nil
}

// mapErr turns driver constraint violations into store.ErrConflict. Callers
// have already ruled out foreign-key failures by checking the session.
func mapErr(op string, err error) error {
	var se *msqlite.Error
	if errors.As(err, &se) && se.Code()&0xff == sqliteConstraint {
		return fmt.Errorf("%s: %w", op, store.ErrConflict)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// requireOne turns a zero-row update into ErrNotFound.
func requireOne(op string, res sql.Result, err error) error {
	if err != nil {
		return mapErr(op, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", op, store.ErrNotFound)
	}
	return nil
}
