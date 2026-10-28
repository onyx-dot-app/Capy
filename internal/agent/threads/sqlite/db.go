// Package sqlite implements threads.Store on SQLite via modernc.org/sqlite
// (pure Go, no cgo), in agent.db, a file of Agent's own beside Capy's
// library. It is opened as the library is: WAL, a single connection,
// secure_delete, migrations in BEGIN IMMEDIATE.
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

	msqlite "modernc.org/sqlite"

	"github.com/onyx-dot-app/Capy/internal/agent/threads"
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

// DB is a SQLite-backed threads.Store.
type DB struct {
	sql *sql.DB
}

var _ threads.Store = (*DB)(nil)

// Open opens or creates the database at path and applies pending
// migrations. The parent directory is created with mode 0700 and the file
// with 0600 because it holds questions about private transcripts and the
// answers.
func Open(ctx context.Context, path string) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("threads: resolve path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, fmt.Errorf("threads: create directory: %w", err)
	}
	f, err := os.OpenFile(abs, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("threads: create file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("threads: create file: %w", err)
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
		// Zero freed pages, so erased threads do not linger in the file.
		"secure_delete(ON)",
	} {
		q.Add("_pragma", p)
	}
	dsn := (&url.URL{Scheme: "file", Path: abs, RawQuery: q.Encode()}).String()

	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("threads: open: %w", err)
	}
	// One connection serializes all access; busy_timeout covers a second
	// process.
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
	if err := db.finishPendingErases(ctx); err != nil && !errors.Is(err, errCheckpointBusy) {
		_ = sdb.Close()
		return nil, err
	}
	// A reader may still keep the WAL. Keep its retry markers, but allow
	// the app to open; deletion retries or the next open finish erasure.
	return db, nil
}

// finishPendingErases completes deletions interrupted by a crash or a
// reader that kept the WAL open. The markers contain no user content.
func (db *DB) finishPendingErases(ctx context.Context) error {
	const op = "finish pending thread erasures"
	rows, err := db.sql.QueryContext(ctx, `SELECT id FROM pending_thread_erases`)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("%s: %w", op, err)
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if len(ids) == 0 {
		return nil
	}
	if err := db.checkpoint(ctx, op); err != nil {
		return err
	}
	// Remove only the markers observed before the checkpoint: another
	// process may have begun deleting a different thread meanwhile.
	for _, id := range ids {
		if _, err := db.sql.ExecContext(ctx, `DELETE FROM pending_thread_erases WHERE id = ?`, id); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
	}
	return nil
}

// connect opens the pool's connection. A new file turns to WAL as its
// first connection opens, which another process's, opening it at the same
// time, cannot wait for as it waits for locks: it tries again instead,
// for as long as it would wait for a lock.
func connect(ctx context.Context, sdb *sql.DB) error {
	deadline := time.Now().Add(busyTimeout)
	for {
		err := sdb.PingContext(ctx)
		se, isSQLite := errors.AsType[*msqlite.Error](err)
		if err == nil || !isSQLite || se.Code()&0xff != sqliteBusy || time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("threads: connect: %w", err)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("threads: connect: %w", ctx.Err())
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
		return fmt.Errorf("threads: read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	conn, err := db.sql.Conn(ctx)
	if err != nil {
		return fmt.Errorf("threads: connect: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("threads: begin migration: %w", err)
	}
	defer func() {
		if err != nil {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()

	var current int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("threads: read schema version: %w", err)
	}
	if current > len(names) {
		return fmt.Errorf("threads: database schema version %d is newer than this build (%d)", current, len(names))
	}
	for i := current; i < len(names); i++ {
		body, err := migrationFS.ReadFile("migrations/" + names[i])
		if err != nil {
			return fmt.Errorf("threads: read migration %s: %w", names[i], err)
		}
		if _, err := conn.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("threads: apply migration %s: %w", names[i], err)
		}
	}
	if current < len(names) {
		// PRAGMA does not accept bound parameters; the value is our own integer.
		if _, err := conn.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(len(names))); err != nil {
			return fmt.Errorf("threads: set schema version: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("threads: commit migration: %w", err)
	}
	return nil
}

// mapErr turns driver constraint violations into threads.ErrConflict.
// Callers have already ruled out foreign-key failures by checking the
// thread.
func mapErr(op string, err error) error {
	if se, ok := errors.AsType[*msqlite.Error](err); ok && se.Code()&0xff == sqliteConstraint {
		return fmt.Errorf("%s: %w", op, threads.ErrConflict)
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
		return fmt.Errorf("%s: %w", op, threads.ErrNotFound)
	}
	return nil
}

// errCheckpointBusy is an erase whose text may remain in the write-ahead
// log until a later checkpoint.
var errCheckpointBusy = errors.New("another connection kept the write-ahead log")

// checkpoint moves erased pages out of the write-ahead log, which still
// holds their old content, into the zeroed database file. A reader in
// another process can keep the log from being emptied, which SQLite
// reports as a result, not an error: the erase is then reported failed,
// to be retried.
func (db *DB) checkpoint(ctx context.Context, op string) error {
	var busy, frames, moved int
	if err := db.sql.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &frames, &moved); err != nil {
		return fmt.Errorf("%s: checkpoint: %w", op, err)
	}
	if busy != 0 {
		return fmt.Errorf("%s: checkpoint: %w", op, errCheckpointBusy)
	}
	return nil
}
