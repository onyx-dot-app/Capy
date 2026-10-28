package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/onyx-dot-app/Capy/internal/store"
)

// PurgeHook erases what a part of Capy keeps of a session apart from the
// library, as an add-on's data about it. It runs on the goroutine that
// purges, never the main thread, and its error must hold none of that
// data: it is logged.
type PurgeHook func(ctx context.Context, sessionID string) error

// Purger erases sessions: Delete Now, and the sessions Recently Deleted
// has kept for longer than store.TrashRetention. Every erase runs the
// hooks after durably claiming the session, then purges the library. A
// claimed session cannot be restored, including after a hook failure or
// restart; the next erase retries it. It is safe for
// concurrent use.
type Purger struct {
	store store.Store
	mu    sync.Mutex
	hooks []PurgeHook
}

// NewPurger returns a Purger of st's sessions.
func NewPurger(st store.Store) *Purger { return &Purger{store: st} }

// OnPurge adds a hook that every erase runs.
func (p *Purger) OnPurge(hook PurgeHook) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hooks = append(p.hooks, hook)
}

// Purge erases session id, which must be in Recently Deleted.
func (p *Purger) Purge(ctx context.Context, id string) error {
	if err := p.store.ClaimPurge(ctx, id); err != nil {
		return err
	}
	p.mu.Lock()
	hooks := append([]PurgeHook(nil), p.hooks...)
	p.mu.Unlock()
	for _, hook := range hooks {
		if err := hook(ctx, id); err != nil {
			return fmt.Errorf("session: purge hook: %w", err)
		}
	}
	return p.store.PurgeSession(ctx, id)
}

// PurgeExpired erases the sessions trashed more than store.TrashRetention
// before now, and returns how many it erased. One restored meanwhile is
// left; one it fails to erase does not stop the others, and the errors
// are joined.
func (p *Purger) PurgeExpired(ctx context.Context, now time.Time) (int, error) {
	ids, err := p.store.ListExpired(ctx, now)
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, id := range ids {
		switch err := p.Purge(ctx, id); {
		case errors.Is(err, store.ErrNotFound):
			continue // restored since it was listed
		case err != nil:
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}
