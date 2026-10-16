package consumers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/events"
)

// positionStore is a consumer's own store that may hold no position yet.
type positionStore struct {
	pos   map[string]int64
	saves int
}

func (s *positionStore) Checkpoint(_ context.Context, name string) (int64, error) {
	p, ok := s.pos[name]
	if !ok {
		return 0, ErrNoCheckpoint
	}
	return p, nil
}

func (s *positionStore) SaveCheckpoint(_ context.Context, name string, position int64) error {
	s.saves++
	s.pos[name] = position
	return nil
}

// committer applies an event and records its position together: the
// engine calls Commit, never Apply and then SaveCheckpoint.
type committer struct {
	recorder
	store   *positionStore
	applied int
	fail    int64
}

func (c *committer) Name() string                 { return "committer" }
func (c *committer) Checkpoints() CheckpointStore { return c.store }
func (c *committer) Apply(context.Context, events.Event) error {
	c.applied++
	return nil
}
func (c *committer) Commit(ctx context.Context, ev events.Event) error {
	if ev.Position == c.fail {
		return errors.New("rolled back")
	}
	c.recorder.Apply(ctx, ev)
	c.store.pos[c.Name()] = ev.Position // in the same transaction as the effects
	return nil
}

func TestACommitterRecordsItsOwnPosition(t *testing.T) {
	var dir string
	store := openStore(t, &dir)
	defer store.Close()
	ctx := context.Background()
	for _, id := range []string{"t1", "t2", "t3", "t4"} {
		appendOne(t, store, id)
	}
	// its old checkpoint, in the engine's store, from before it committed
	if err := store.SaveCheckpoint(ctx, "committer", 1); err != nil {
		t.Fatal(err)
	}
	c := &committer{store: &positionStore{pos: map[string]int64{}}, fail: 4}
	e := NewEngine(store, nil)
	e.Register(c)
	if err := e.RunOnce(ctx); err == nil {
		t.Fatal("the failing event was not reported")
	}
	if c.applied != 0 || c.store.saves != 0 {
		t.Fatalf("the engine called Apply (%d) or SaveCheckpoint (%d) on a committer", c.applied, c.store.saves)
	}
	if c.count() != 2 || c.seen[0].Position != 2 {
		t.Fatalf("want positions 2 and 3, from the old checkpoint on, got %d events", c.count())
	}
	if pos, _ := e.Checkpoint(ctx, "committer"); pos != 3 {
		t.Fatalf("its position is its own store's: %d", pos)
	}
	if pos, _ := store.Checkpoint(ctx, "committer"); pos != 1 {
		t.Fatalf("the engine's store must not move for a committer: %d", pos)
	}
}

// Nudge wakes a running engine for a pass now, not at its next tick: a
// secondary's LiteFS watcher calls it when the log has new transactions.
func TestNudgeWakesTheEngine(t *testing.T) {
	var dir string
	store := openStore(t, &dir)
	defer store.Close()
	r := &recorder{}
	ro, err := events.OpenReadOnly(dir + "/events.db")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	e := NewEngineWithCheckpoints(ro, &MemoryCheckpoints{}, nil)
	e.Register(r)
	e.tick = time.Hour // only the nudge can wake it in time
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.Run(ctx)
	time.Sleep(100 * time.Millisecond) // a first pass, then waiting for a tick
	appendOne(t, store, "t1")          // another process's write: no in-process notice
	e.Nudge()
	deadline := time.Now().Add(3 * time.Second)
	for r.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if r.count() != 1 {
		t.Fatal("the nudge didn't wake the engine")
	}
}
