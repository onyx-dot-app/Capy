package gateway

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slowAgent is an agent whose probe takes a while and counts its calls.
type slowAgent struct {
	id     string
	delay  time.Duration
	probes atomic.Int32
	found  atomic.Bool
}

func (a *slowAgent) Info() Info { return Info{ID: a.id, Name: a.id} }

func (a *slowAgent) Run(context.Context, Query) (Run, error) { return nil, ErrUnavailable }

func (a *slowAgent) Probe(context.Context) Probe {
	a.probes.Add(1)
	time.Sleep(a.delay)
	return Probe{Found: a.found.Load(), Path: "/bin/" + a.id, Version: "1.0.0"}
}

func TestAgentsProbesEveryAgentAtOnceAndKeepsTheResult(t *testing.T) {
	a := &slowAgent{id: "a", delay: 300 * time.Millisecond}
	b := &slowAgent{id: "b", delay: 300 * time.Millisecond}
	a.found.Store(true)
	g := New(Options{}, a, b)

	start := time.Now()
	got := g.Agents(context.Background())
	if d := time.Since(start); d > 550*time.Millisecond {
		t.Fatalf("probing two agents took %v: one after the other", d)
	}
	if len(got) != 2 || got[0].ID != "a" || !got[0].Probe.Found || got[1].ID != "b" || got[1].Probe.Found || got[0].Agent != a {
		t.Fatalf("agents %+v, want both in order with their probes", got)
	}
	// Kept until Refresh, even when callers come at once.
	b.found.Store(true)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { g.Agents(context.Background()) })
	}
	wg.Wait()
	if n := b.probes.Load(); n != 1 {
		t.Fatalf("b probed %d times before Refresh, want once", n)
	}
	if got := g.Agents(context.Background()); got[1].Probe.Found {
		t.Fatal("Agents probed again")
	}
	if got := g.Refresh(context.Background()); !got[1].Probe.Found || b.probes.Load() != 2 {
		t.Fatalf("Refresh: %+v, %d probes", got[1], b.probes.Load())
	}
	if got := g.Agents(context.Background()); !got[1].Probe.Found {
		t.Fatal("Agents does not keep what Refresh found")
	}
}

func TestFirstCallersWaitForOneProbe(t *testing.T) {
	a := &slowAgent{id: "a", delay: 100 * time.Millisecond}
	g := New(Options{}, a)
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() { g.Agents(context.Background()) })
	}
	wg.Wait()
	if n := a.probes.Load(); n != 1 {
		t.Fatalf("five first callers probed %d times, want once", n)
	}
}

func TestAgentFindsARegisteredAgentByID(t *testing.T) {
	a, b := &slowAgent{id: "claude-code"}, &slowAgent{id: "codex"}
	g := New(Options{}, a, b)
	if got, ok := g.Agent("codex"); !ok || got != b {
		t.Fatalf("Agent(codex) = %v, %v", got, ok)
	}
	if _, ok := g.Agent("cursor"); ok {
		t.Fatal("an agent never registered was found")
	}
}

func TestUsableNeedsFoundAndRecentEnough(t *testing.T) {
	for p, want := range map[Probe]bool{
		{}:                             false,
		{Found: true}:                  true,
		{Found: true, TooOld: "2.1.0"}: false,
	} {
		if p.Usable() != want {
			t.Errorf("%+v usable = %v, want %v", p, p.Usable(), want)
		}
	}
}

// TestAProbeCutShortIsNotKept probes again after a probe whose time ran
// out, rather than keep what it found for the app's life.
func TestAProbeCutShortIsNotKept(t *testing.T) {
	a := &slowAgent{id: "a"}
	g := New(Options{}, a)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g.Agents(ctx)
	g.Agents(context.Background())
	g.Agents(context.Background())
	if n := a.probes.Load(); n != 2 {
		t.Fatalf("probed %d times, want again after the cut-short one", n)
	}
}
