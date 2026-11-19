package consumers

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jamestryand/pocketcqrs/events"
)

// counting is a checkpoint store that counts the saves per consumer.
type counting struct {
	inner *MemoryCheckpoints
	mu    sync.Mutex
	saves map[string]int
}

func newCounting() *counting { return &counting{inner: &MemoryCheckpoints{}, saves: map[string]int{}} }

func (c *counting) Checkpoint(ctx context.Context, name string) (int64, error) {
	return c.inner.Checkpoint(ctx, name)
}

func (c *counting) SaveCheckpoint(ctx context.Context, name string, position int64) error {
	c.mu.Lock()
	c.saves[name]++
	c.mu.Unlock()
	return c.inner.SaveCheckpoint(ctx, name, position)
}

func (c *counting) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saves[name]
}

func logOf(t *testing.T, n int) (*events.Store, []events.Event) {
	t.Helper()
	var dir string
	store := openStore(t, &dir)
	t.Cleanup(func() { store.Close() })
	for i := 0; i < n; i++ {
		appendOne(t, store, "t"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	all, err := store.Poll(context.Background(), 0, n+1)
	if err != nil || len(all) != n {
		t.Fatalf("log of %d: got %d (%v)", n, len(all), err)
	}
	return store, all
}

// A plain consumer saves its position once per batch (the engine polls 100 at a time), an effect once per event:
// a replay into an effect would act on the world again.
func TestAPlainConsumerSavesItsPositionOncePerBatchAndAnEffectOncePerEvent(t *testing.T) {
	store, all := logOf(t, 250)
	cp := newCounting()
	plain := &recorder{}
	effect := &tiered{name: "effect", effect: true}
	engine := NewEngineWithCheckpoints(store, cp, nil)
	engine.Register(plain)
	engine.Register(effect)
	if err := engine.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := all[len(all)-1].Position
	for _, name := range []string{"rec", "effect"} {
		if pos, _ := cp.Checkpoint(context.Background(), name); pos != last {
			t.Fatalf("%s's checkpoint is %d, want %d", name, pos, last)
		}
	}
	if n := cp.count("rec"); n != 3 {
		t.Fatalf("the plain consumer saved its position %d times for 250 events in batches of 100, want 3", n)
	}
	if n := cp.count("effect"); n != 250 {
		t.Fatalf("the effect saved its position %d times for 250 events, want 250 (one per event)", n)
	}
}

// failsAt fails on the event at one position until it is told not to.
type failsAt struct {
	recorder
	at   int64
	fail bool
}

func (f *failsAt) Apply(ctx context.Context, ev events.Event) error {
	if f.fail && ev.Position == f.at {
		return errors.New("boom")
	}
	return f.recorder.Apply(ctx, ev)
}

// A failure part way through a batch keeps the position before the failing event: not the batch's start (the
// events before it applied again: harmless, but not what was promised), and never the batch's end (the failing
// event and those after it skipped for good).
func TestAFailurePartWayThroughABatchKeepsThePositionBeforeIt(t *testing.T) {
	store, all := logOf(t, 150)
	cp := newCounting()
	c := &failsAt{at: all[120].Position, fail: true}
	engine := NewEngineWithCheckpoints(store, cp, nil)
	engine.Register(c)
	if err := engine.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the failing event's error")
	}
	if pos, _ := cp.Checkpoint(context.Background(), "rec"); pos != all[119].Position {
		t.Fatalf("after a failure at %d the checkpoint is %d, want %d (the event before it)", all[120].Position, pos, all[119].Position)
	}
	c.fail = false
	before := c.count()
	if err := engine.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	resumed := c.seen[before].Position
	c.mu.Unlock()
	if resumed != all[120].Position {
		t.Fatalf("the retry resumed at %d, want the failing event %d", resumed, all[120].Position)
	}
	if got := c.count(); got != 150 {
		t.Fatalf("%d events applied in all, want 150 (each once)", got)
	}
}

// stopsAt cancels the engine's run once it has applied a given number of events.
type stopsAt struct {
	recorder
	after  int
	cancel context.CancelFunc
}

func (s *stopsAt) Apply(ctx context.Context, ev events.Event) error {
	err := s.recorder.Apply(ctx, ev)
	if s.count() == s.after {
		s.cancel()
	}
	return err
}

// A stop part way through a batch saves the position reached before Run returns, so nothing applied is applied
// again on the next start (the in-flight event finishes and is checkpointed, as before).
func TestAStopPartWayThroughABatchSavesThePositionReached(t *testing.T) {
	store, all := logOf(t, 80)
	cp := newCounting()
	ctx, cancel := context.WithCancel(context.Background())
	c := &stopsAt{after: 30, cancel: cancel}
	engine := NewEngineWithCheckpoints(store, cp, nil)
	engine.Register(c)
	engine.Run(ctx)
	if got := c.count(); got != 30 {
		t.Fatalf("%d events applied before the stop took, want 30", got)
	}
	if pos, _ := cp.Checkpoint(context.Background(), "rec"); pos != all[29].Position {
		t.Fatalf("after a stop at the 30th event the checkpoint is %d, want %d", pos, all[29].Position)
	}
}
