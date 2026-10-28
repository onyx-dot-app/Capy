package ask

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// CatchUpQuestion is Catch me up as the thread shows it.
const CatchUpQuestion = "Catch me up"

// catchUpWindow is how far back Catch me up goes in a session the user
// has not looked at yet; catchUpOverlap how far before the panel closed,
// so that the turn being spoken then, which starts before, is in.
const (
	catchUpWindow  = 10 * time.Minute
	catchUpOverlap = 5 * time.Second
)

// Seen records that the user stopped looking at live session id at at, as
// its panel closes: Catch me up covers what was said since. It is kept
// for Capy's run only.
func (a *Ask) Seen(id string, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seen == nil {
		a.seen = map[string]time.Time{}
	}
	a.seen[id] = at
}

// CatchUp asks Catch me up about live session id, in thread (or a new
// thread for ""): what was said since its panel was last open, else in
// the last 10 minutes.
func (a *Ask) CatchUp(ctx context.Context, scope Scope, thread string) (*Turn, error) {
	from, err := a.catchUpFrom(ctx, scope.SessionID, time.Now())
	if err != nil {
		return nil, err
	}
	hint := fmt.Sprintf("Cover what was said from [%s] on: call get_live_session with since_ms %d.", clock(from), from)
	if thread != "" {
		return a.followUp(ctx, thread, CatchUpQuestion, hint)
	}
	return a.ask(ctx, scope, CatchUpQuestion, hint)
}

// catchUpFrom is where Catch me up starts in session id, as ms into it.
func (a *Ask) catchUpFrom(ctx context.Context, id string, now time.Time) (int64, error) {
	s, err := a.o.Library.GetSession(ctx, id)
	if err != nil {
		return 0, err
	}
	a.mu.Lock()
	from, ok := a.seen[id]
	a.mu.Unlock()
	if ok {
		from = from.Add(-catchUpOverlap)
	} else {
		from = now.Add(-catchUpWindow)
	}
	return max(0, from.Sub(s.StartedAt).Milliseconds()), nil
}

// clock is ms into a session as citations give it: 00:42, or 1:02:03.
func clock(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	h, m, s := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	if h > 0 {
		return strconv.Itoa(h) + fmt.Sprintf(":%02d:%02d", m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// withHint is prompt with hint after it, a paragraph of its own.
func withHint(prompt, hint string) string {
	if hint == "" {
		return prompt
	}
	return prompt + "\n\n" + hint
}
