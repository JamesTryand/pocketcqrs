package pii

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/kms"
	"github.com/jamestryand/pocketcqrs/outbox"
)

// flaky fails DestroyKey the first n times.
type flaky struct {
	*kms.Memory
	n     int
	calls int
}

func (f *flaky) DestroyKey(ctx context.Context, subject string) error {
	f.calls++
	if f.calls <= f.n {
		return errors.New("facade unavailable")
	}
	return f.Memory.DestroyKey(ctx, subject)
}

// With an outbox the destroyer only records the erasure on the engine's
// pass (R4); the worker destroys the key, a failure due again later (a
// due time, never a pause) and never given up. An evictor registered while
// a destruction is still pending starts before it, so the erasure reaches
// the cache though the destroyer's checkpoint is past it.
func TestKeyDestroyerWorksFromTheOutbox(t *testing.T) {
	store := openStore(t)
	r := decider.NewRegistry(store)
	RegisterDataSubjects(r)
	ctx := context.Background()
	m := &flaky{Memory: kms.NewMemory(), n: 2}
	_ = m.EnsureKey(ctx, "s1")
	box, err := outbox.Open(filepath.Join(t.TempDir(), "outbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	d := KeyDestroyer{KMS: m, Outbox: box}
	engine := consumers.NewEngine(store, nil)
	engine.Register(d)
	erase(t, r, "s1")
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if m.calls != 0 {
		t.Fatal("the key was destroyed on the engine's pass")
	}

	cache := NewCache(0)
	cache.Put("s1", "c", []byte("1"))
	if _, err := RegisterCacheEvictor(ctx, engine, cache, store, box); err != nil {
		t.Fatal(err)
	}
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if !cache.IsErased("s1") {
		t.Fatal("the evictor started past an erasure whose key isn't destroyed yet")
	}

	first, _ := box.Pending(ctx)
	if len(first) != 1 {
		t.Fatalf("pending %+v", first)
	}
	now := first[0].Due // the erasure is due as soon as it is recorded
	w := &outbox.Worker{Store: box, Now: func() time.Time { return now }, Handlers: map[string]outbox.Handler{KeyDestroyerName: d.Destroy}}
	for i := 0; i < 3; i++ {
		w.RunOnce(ctx)
		pending, _ := box.Pending(ctx)
		if i < 2 && (len(pending) != 1 || !pending[0].Due.After(now)) {
			t.Fatalf("try %d: %+v", i+1, pending)
		}
		if len(pending) == 1 {
			now = pending[0].Due
		}
	}
	if pending, _ := box.Pending(ctx); len(pending) != 0 {
		t.Fatalf("still pending: %+v", pending)
	}
	if !kms.IsSubjectErased(m.EnsureKey(ctx, "s1")) {
		t.Fatal("the key wasn't destroyed")
	}
}
