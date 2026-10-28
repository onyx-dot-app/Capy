package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/agent/threads"
	"github.com/onyx-dot-app/Capy/internal/agent/threads/storetest"
)

func openAt(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db
}

func TestContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) threads.Store {
		db := openAt(t, filepath.Join(t.TempDir(), "agent.db"))
		t.Cleanup(func() { _ = db.Close() })
		return db
	})
}

func newThread(t *testing.T, db *DB, sessionID string) threads.Thread {
	t.Helper()
	th, err := db.CreateThread(context.Background(), threads.Thread{ID: uuid.NewString(), Capability: "ask",
		SessionID: sessionID, Scope: threads.ScopeSession, Agent: "claude-code", Title: "confidential question"})
	if err != nil {
		t.Fatal(err)
	}
	return th
}

func TestReopenKeepsThreads(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent", "agent.db")
	db := openAt(t, path)
	th := newThread(t, db, uuid.NewString())
	if _, err := db.AppendMessage(ctx, threads.Message{ID: uuid.NewString(), ThreadID: th.ID, Role: threads.RoleUser, Text: "Q"}); err != nil {
		t.Fatal(err)
	}
	// Model the schema of an existing installation, before pending erase
	// markers were introduced. Reopening must migrate without losing it.
	if _, err := db.sql.Exec("DROP TABLE pending_thread_erases; PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openAt(t, path)
	defer db.Close()
	d, err := db.GetThread(ctx, th.ID)
	if err != nil || len(d.Messages) != 1 {
		t.Fatalf("after reopen: %+v, %v", d, err)
	}
	var version int
	if err := db.sql.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatalf("user_version = %d, %v", version, err)
	}
}

func TestPrivateFilePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	path := filepath.Join(dir, "agent.db")
	db := openAt(t, path)
	defer db.Close()
	for p, want := range map[string]os.FileMode{dir: 0o700, path: 0o600} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", p, got, want)
		}
	}
}

// TestEraseSessionLeavesNoText checks the files themselves: a backup or
// strings agent.db must not reveal a purged session's questions and
// answers.
func TestEraseSessionLeavesNoText(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	db := openAt(t, path)
	defer db.Close()
	session := uuid.NewString()
	th := newThread(t, db, session)
	for _, m := range []threads.Message{
		{ID: uuid.NewString(), ThreadID: th.ID, Role: threads.RoleUser, Text: "what did the auditor say"},
		{ID: uuid.NewString(), ThreadID: th.ID, Role: threads.RoleAgent, Text: "the merger closes in May"},
	} {
		if _, err := db.AppendMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetConversation(ctx, th.ID, "conv-secret-id"); err != nil {
		t.Fatal(err)
	}
	if err := db.EraseSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{path, path + "-wal"} {
		raw, err := os.ReadFile(f)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"confidential question", "what did the auditor say", "the merger closes in May", "conv-secret-id"} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Errorf("%s still contains %q after the erase", filepath.Base(f), secret)
			}
		}
	}
}

// TestEraseReportsTextLeftInTheLog keeps a reader open in another
// connection, so that the checkpoint cannot empty the write-ahead log: the
// erase must say so, for the purge to be retried.
func TestEraseReportsTextLeftInTheLog(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "agent.db")
	db, reader := openAt(t, path), openAt(t, path)
	defer db.Close()
	defer reader.Close()
	session := uuid.NewString()
	newThread(t, db, session)
	tx, err := reader.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM threads").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := db.EraseSession(ctx, session); !errors.Is(err, errCheckpointBusy) {
		t.Fatalf("erase with a reader open: %v, want errCheckpointBusy", err)
	}
}

func TestDeleteThreadRetriesErasingItsFiles(t *testing.T) {
	for _, recover := range []string{"retry", "reopen", "blocked reopen"} {
		t.Run(recover, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "agent.db")
			db, reader := openAt(t, path), openAt(t, path)
			defer db.Close()
			defer reader.Close()
			if _, err := db.sql.Exec("PRAGMA busy_timeout = 20"); err != nil {
				t.Fatal(err)
			}
			secrets := []string{"private cross-session question", "private answer from the recording"}
			th, err := db.CreateThread(ctx, threads.Thread{ID: uuid.NewString(), Capability: "ask",
				Scope: threads.ScopeAll, Agent: "codex", Title: secrets[0]})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.AppendMessage(ctx, threads.Message{ID: uuid.NewString(), ThreadID: th.ID,
				Role: threads.RoleAgent, Text: secrets[1]}); err != nil {
				t.Fatal(err)
			}
			tx, err := reader.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			var n int
			if err := tx.QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil {
				t.Fatal(err)
			}
			if err := db.DeleteThread(ctx, th.ID); !errors.Is(err, errCheckpointBusy) {
				t.Fatalf("deletion with a reader: %v", err)
			}
			pending, err := db.GetThread(ctx, th.ID)
			if !errors.Is(err, threads.ErrErasing) || pending.ID != th.ID || pending.Capability != "ask" ||
				pending.Title != "" || len(pending.Messages) != 0 {
				t.Fatalf("pending erasure exposes more than retry metadata: %+v, %v", pending, err)
			}
			raw, err := os.ReadFile(path + "-wal")
			if err != nil || !bytes.Contains(raw, []byte(secrets[1])) {
				t.Fatalf("expected deleted text in the blocked WAL: %v", err)
			}
			if recover == "blocked reopen" {
				opened := openAt(t, path)
				defer opened.Close()
				if _, err := opened.GetThread(ctx, th.ID); !errors.Is(err, threads.ErrErasing) {
					t.Fatalf("open with a reader lost the erasure marker: %v", err)
				}
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if recover != "reopen" {
				if err := db.DeleteThread(ctx, th.ID); err != nil {
					t.Fatalf("retry: %v", err)
				}
			} else {
				// Leave the original connection open so its normal close
				// cannot erase the WAL for the recovery under test.
				reopened := openAt(t, path)
				defer reopened.Close()
			}
			if _, err := db.GetThread(ctx, th.ID); !errors.Is(err, threads.ErrNotFound) {
				t.Fatalf("finished deletion: %v", err)
			}
			for _, file := range []string{path, path + "-wal"} {
				raw, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				for _, secret := range secrets {
					if bytes.Contains(raw, []byte(secret)) {
						t.Fatalf("%s still contains deleted text", filepath.Base(file))
					}
				}
			}
		})
	}
}
