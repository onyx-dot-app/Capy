package ask

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/agent/gateway"
	"github.com/onyx-dot-app/Capy/internal/agent/gateway/fake"
	"github.com/onyx-dot-app/Capy/internal/agent/threads"
)

func TestDeleteThreadRetriesOnlyItsOwnCrossSessionErasure(t *testing.T) {
	f := newFixture(t, fake.Script{}, gateway.Options{})
	ctx := context.Background()
	path := filepath.Join(f.work, "agent.db")
	reader, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, c := range []struct {
		capability, session string
		want                error
	}{
		{Capability, "", nil},
		{Capability, f.session, ErrSessionThread},
		{"another-capability", "", threads.ErrNotFound},
	} {
		scope := threads.ScopeAll
		if c.session != "" {
			scope = threads.ScopeSession
		}
		th, err := f.threads.CreateThread(ctx, threads.Thread{ID: uuid.NewString(), Capability: c.capability,
			SessionID: c.session, Scope: scope, Agent: "fake", Title: "private retry question"})
		if err != nil {
			t.Fatal(err)
		}
		tx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err := tx.QueryRow("SELECT COUNT(*) FROM threads").Scan(&n); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if c.want == nil {
			err = f.ask.DeleteThread(ctx, th.ID)
		} else {
			// A different capability or a session purge can leave a
			// marker too. Ask must preserve its authorization checks.
			err = f.threads.DeleteThread(ctx, th.ID)
		}
		if err == nil {
			tx.Rollback()
			t.Fatal("expected a blocked checkpoint")
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if err := f.ask.DeleteThread(ctx, th.ID); !errors.Is(err, c.want) {
			t.Fatalf("retry of capability %q, session %q: %v, want %v", c.capability, c.session, err, c.want)
		}
		if c.want == nil {
			raw, err := os.ReadFile(path + "-wal")
			if err != nil || bytes.Contains(raw, []byte("private retry question")) {
				t.Fatalf("successful retry retained question text: %v", err)
			}
		}
	}
	if err := f.ask.DeleteThread(ctx, uuid.NewString()); !errors.Is(err, threads.ErrNotFound) {
		t.Fatalf("unknown thread: %v", err)
	}
}
