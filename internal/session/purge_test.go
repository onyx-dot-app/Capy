package session_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/onyx-dot-app/Capy/internal/session"
	"github.com/onyx-dot-app/Capy/internal/store"
	"github.com/onyx-dot-app/Capy/internal/store/sqlite"
)

// trashed stores an ended session and moves it to Recently Deleted.
func trashed(t *testing.T, st store.Store) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	if err := st.CreateSession(ctx, store.Session{ID: id, Mode: store.ModeCaptions, Source: store.SourceMic,
		LangA: "en-US", LangB: "zh-CN", StartedAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := st.EndSession(ctx, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TrashSession(ctx, id, "user", ""); err != nil {
		t.Fatal(err)
	}
	return id
}

func openLibrary(t *testing.T) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "capy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func inTrash(t *testing.T, st store.Store) int {
	t.Helper()
	list, err := st.ListTrashed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

func TestPurgeRunsTheHooksThenErases(t *testing.T) {
	db := openLibrary(t)
	p := session.NewPurger(db)
	id := trashed(t, db)
	var erased []string
	p.OnPurge(func(_ context.Context, sessionID string) error {
		// The session is still there for the hook to look at.
		if n := inTrash(t, db); n != 1 {
			t.Errorf("hook ran after the purge: %d in Recently Deleted", n)
		}
		erased = append(erased, sessionID)
		return nil
	})
	if err := p.Purge(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if len(erased) != 1 || erased[0] != id || inTrash(t, db) != 0 {
		t.Fatalf("hooks erased %v; %d left in Recently Deleted", erased, inTrash(t, db))
	}
}

func TestAFailingHookStopsThePurge(t *testing.T) {
	db := openLibrary(t)
	p := session.NewPurger(db)
	id := trashed(t, db)
	fail := errors.New("threads locked")
	p.OnPurge(func(context.Context, string) error { return fail })
	if err := p.Purge(context.Background(), id); !errors.Is(err, fail) {
		t.Fatalf("purge: %v, want the hook's error", err)
	}
	if inTrash(t, db) != 1 {
		t.Fatal("the session was purged though its hook failed: its other data would outlive it")
	}
}

func TestPurgeExpiredErasesOnlyWhatOutlivedTheRetention(t *testing.T) {
	db := openLibrary(t)
	p := session.NewPurger(db)
	old, other := trashed(t, db), trashed(t, db)
	hooked := map[string]bool{}
	p.OnPurge(func(_ context.Context, id string) error {
		hooked[id] = true
		if id == other {
			return errors.New("busy")
		}
		return nil
	})
	if n, err := p.PurgeExpired(context.Background(), time.Now()); n != 0 || err != nil {
		t.Fatalf("just trashed: purged %d, %v", n, err)
	}
	// A month on, both are due; one's hook fails, the other is erased.
	n, err := p.PurgeExpired(context.Background(), time.Now().Add(store.TrashRetention+time.Minute))
	if n != 1 || err == nil || !hooked[old] || !hooked[other] {
		t.Fatalf("purged %d, %v, hooks ran for %v; want one erased and the failure reported", n, err, hooked)
	}
	list, err := db.ListTrashed(context.Background())
	if err != nil || len(list) != 1 || list[0].ID != other {
		t.Fatalf("left in Recently Deleted: %+v, %v; want the one whose hook failed, to retry", list, err)
	}
}

// restoreOnExpiry models a user restoring after the expiry query, before
// the purger processes its result. Storage still goes through SQLite.
type restoreOnExpiry struct{ store.Store }

func (s restoreOnExpiry) ListExpired(ctx context.Context, now time.Time) ([]string, error) {
	ids, err := s.Store.ListExpired(ctx, now)
	if err == nil {
		for _, id := range ids {
			if _, err := s.Store.RestoreSession(ctx, id, "user", ""); err != nil {
				return nil, err
			}
		}
	}
	return ids, err
}

func TestRestoreBeforePurgePreservesTheHooksData(t *testing.T) {
	db := openLibrary(t)
	id := trashed(t, db)
	p := session.NewPurger(restoreOnExpiry{db})
	p.OnPurge(func(context.Context, string) error {
		t.Fatal("a restored session's history must not be erased")
		return nil
	})
	if n, err := p.PurgeExpired(context.Background(), time.Now().Add(store.TrashRetention+time.Minute)); n != 0 || err != nil {
		t.Fatalf("purge restored session: %d, %v", n, err)
	}
	if _, err := db.GetSession(context.Background(), id); err != nil {
		t.Fatalf("restored session: %v", err)
	}
}

func TestClaimPreventsRestoreAndSurvivesFailedHookAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "capy.db")
	db, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	id := trashed(t, db)
	// Another connection is how Restore from another process races.
	other, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	p := session.NewPurger(db)
	fail := errors.New("history erasure interrupted")
	p.OnPurge(func(ctx context.Context, sessionID string) error {
		if _, err := other.RestoreSession(ctx, sessionID, "user", ""); !errors.Is(err, store.ErrConflict) {
			t.Errorf("restore during erasure: %v, want conflict", err)
		}
		return fail
	})
	if err := p.Purge(ctx, id); !errors.Is(err, fail) {
		t.Fatalf("failed hook: %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.RestoreSession(ctx, id, "user", ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("restore after restart: %v, want conflict", err)
	}
	// Even a just-trashed session must be retried once its erase began.
	ids, err := db.ListExpired(ctx, time.Now())
	if err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("pending erase after restart: %v, %v", ids, err)
	}
	p = session.NewPurger(db)
	retried := false
	p.OnPurge(func(context.Context, string) error { retried = true; return nil })
	if n, err := p.PurgeExpired(ctx, time.Now()); n != 1 || err != nil || !retried {
		t.Fatalf("retry: %d, %v; hook ran: %v", n, err, retried)
	}
	if inTrash(t, db) != 0 {
		t.Fatal("completed erase still appears in Recently Deleted")
	}
}
