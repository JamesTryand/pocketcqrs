package consumers

import (
	"context"
	"testing"
)

// ownRecorder keeps its position in its own store (OwnCheckpoints).
type ownRecorder struct {
	recorder
	store *MemoryCheckpoints
}

func (r *ownRecorder) Name() string                 { return "own" }
func (r *ownRecorder) Checkpoints() CheckpointStore { return r.store }

// TestOwnCheckpointsStartFromTheirSeedAndNeverTouchTheEngineStore: a
// consumer with its own checkpoint store starts at that store's seed, and
// its progress is reported by Engine.Checkpoint but never written to the
// engine's durable store.
func TestOwnCheckpointsStartFromTheirSeedAndNeverTouchTheEngineStore(t *testing.T) {
	var dir string
	store := openStore(t, &dir)
	defer store.Close()
	ctx := context.Background()
	for _, id := range []string{"t1", "t2", "t3"} {
		appendOne(t, store, id)
	}

	own := &ownRecorder{store: &MemoryCheckpoints{Start: 2}}
	e := NewEngine(store, nil)
	e.Register(own)
	if err := e.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if own.count() != 1 || own.seen[0].Position != 3 {
		t.Fatalf("want only position 3 after a seed of 2, got %d events", own.count())
	}
	if pos, _ := e.Checkpoint(ctx, "own"); pos != 3 {
		t.Fatalf("Engine.Checkpoint must report the consumer's own store: %d", pos)
	}
	if pos, _ := store.Checkpoint(ctx, "own"); pos != 0 {
		t.Fatalf("the durable store must never see a process-local consumer: %d", pos)
	}
}
