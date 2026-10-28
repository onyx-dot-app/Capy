package ask

import (
	"context"
	"errors"
	"strings"

	"github.com/onyx-dot-app/Capy/internal/agent/threads"
	"github.com/onyx-dot-app/Capy/internal/store"
)

// ErrSessionThread is a thread of a session, which goes with its session
// rather than alone.
var ErrSessionThread = errors.New("ask: a session's thread goes with its session")

// Edit is edit id, as its chip shows it: store.ErrNotFound once its
// session is purged.
func (a *Ask) Edit(ctx context.Context, id string) (store.Edit, error) {
	return a.o.Library.GetEdit(ctx, id)
}

// Said is a turn of a session as a citation of it names it: when it was
// said, and its first words.
type Said struct {
	StartMs int64
	Opening string
}

// openingWords is how many of a turn's words a citation's tooltip shows.
const openingWords = 8

// Turns are session id's turns, for its citations, in order.
func (a *Ask) Turns(ctx context.Context, id string) ([]Said, error) {
	d, err := a.o.Library.GetSession(ctx, id)
	if err != nil {
		return nil, err
	}
	turns := make([]Said, 0, len(d.Segments))
	for _, s := range d.Segments {
		if s.Lang == store.GapLang {
			continue
		}
		words := strings.Fields(s.Text)
		opening := strings.Join(words[:min(len(words), openingWords)], " ")
		if len(words) > openingWords {
			opening += "…"
		}
		turns = append(turns, Said{StartMs: s.StartMs, Opening: opening})
	}
	return turns, nil
}

// Session is a session as an answer names it.
type Session struct{ ID, Title string }

// maxSessions bounds the sessions whose titles an answer may link.
const maxSessions = 200

// Sessions are the newest sessions with a title, which a cross-session
// answer may name.
func (a *Ask) Sessions(ctx context.Context) ([]Session, error) {
	list, err := a.o.Library.ListSessions(ctx, store.ListQuery{Limit: maxSessions})
	if err != nil {
		return nil, err
	}
	var out []Session
	for _, s := range list {
		if s.Title != "" {
			out = append(out, Session{ID: s.ID, Title: s.Title})
		}
	}
	return out, nil
}

// DeleteThread erases cross-session thread id at once; a session's
// thread is ErrSessionThread.
func (a *Ask) DeleteThread(ctx context.Context, id string) error {
	// Hold the thread idle across both authorization and deletion.
	if err := a.begin(id); err != nil {
		return err
	}
	defer a.finish(id)
	d, err := a.o.Threads.GetThread(ctx, id)
	if err != nil && !errors.Is(err, threads.ErrErasing) {
		return err
	}
	if d.Capability != Capability {
		return threads.ErrNotFound
	}
	if d.SessionID != "" {
		return ErrSessionThread
	}
	return a.o.Threads.DeleteThread(ctx, id)
}
