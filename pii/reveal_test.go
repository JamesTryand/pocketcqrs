package pii

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/kms"
)

func sealed(t *testing.T, m kms.Client, subject, v string) Value[string] {
	t.Helper()
	enc, err := NewFresh(v).Encrypt(context.Background(), m, subject, nil)
	if err != nil {
		t.Fatal(err)
	}
	return FromCiphertext[string](enc.SubjectID(), enc.Ciphertext())
}

func TestNothingIsSentBeforeFlushAndOneBatchPerSubject(t *testing.T) {
	ctx := context.Background()
	m := kms.NewMemory()
	a1, a2, b1 := sealed(t, m, "a", "a1"), sealed(t, m, "a", "a2"), sealed(t, m, "b", "b1")
	buf := NewBuffer(m, nil)
	for _, p := range []*Value[string]{&a1, &a2, &b1} {
		if err := Reveal(buf, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, calls, _ := m.Calls(); calls != 0 || buf.Len() != 3 {
		t.Fatalf("nothing may reach the facade before Flush: %d calls", calls)
	}
	if err := buf.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if _, calls, _ := m.Calls(); calls != 2 {
		t.Fatalf("want one decrypt-batch per subject (2), got %d", calls)
	}
	for want, p := range map[string]Value[string]{"a1": a1, "a2": a2, "b1": b1} {
		if got, err := p.Get(); err != nil || got != want {
			t.Fatalf("want %q, got %q %v", want, got, err)
		}
	}
	if err := buf.Flush(ctx); err != nil {
		t.Fatal("an empty flush is a no-op")
	}
}

func TestDestroyedKeyRevealsAsRedactedAndMarksTheCache(t *testing.T) {
	ctx := context.Background()
	m := kms.NewMemory()
	v := sealed(t, m, "s1", "x")
	_ = m.DestroyKey(ctx, "s1")
	cache := NewCache(0)
	buf := NewBuffer(m, cache)
	_ = Reveal(buf, &v)
	if err := buf.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if v.State() != Redacted || !cache.IsErased("s1") {
		t.Fatalf("want redacted and erased, got %v", v.State())
	}
	// the next reveal is answered by the cache, with no round trip
	w := FromCiphertext[string]("s1", "anything")
	_ = Reveal(buf, &w)
	if w.State() != Redacted || buf.Len() != 0 {
		t.Fatal("an erased subject must settle at once from the cache")
	}
}

func TestNonPendingRevealsCostNothing(t *testing.T) {
	m := kms.NewMemory()
	buf := NewBuffer(m, nil)
	k, _ := NewFresh("x").Encrypt(context.Background(), m, "s1", nil)
	r := NewRedacted[string]("s1")
	_ = Reveal(buf, &k)
	_ = Reveal(buf, &r)
	if buf.Len() != 0 {
		t.Fatal("known and redacted values need no reveal")
	}
}

// failingItem answers one decrypt-batch item with an error.
type failingItem struct{ *kms.Memory }

func (f failingItem) DecryptBatch(ctx context.Context, s string, c []string) ([]kms.BatchItem, kms.KeyState, error) {
	items, st, err := f.Memory.DecryptBatch(ctx, s, c)
	if err == nil && st == kms.KeyFound {
		items[0] = kms.BatchItem{Err: "bad ciphertext"}
	}
	return items, st, err
}

func TestPerItemErrorFailsOnlyThatValueAndIsUnavailable(t *testing.T) {
	m := kms.NewMemory()
	a, b := sealed(t, m, "s1", "a"), sealed(t, m, "s1", "b")
	buf := NewBuffer(failingItem{m}, nil)
	_ = Reveal(buf, &a)
	_ = Reveal(buf, &b)
	err := buf.Flush(context.Background())
	if err == nil || !kms.IsUnavailable(err) {
		t.Fatalf("a per-item error is a protocol error (503, retry): %v", err)
	}
	if a.State() != Pending {
		t.Fatalf("the failed value stays pending, got %v", a.State())
	}
	if got, _ := b.Get(); got != "b" {
		t.Fatal("the other value still reveals")
	}
}

func TestChunksAtTheFacadeLimit(t *testing.T) {
	m := kms.NewMemory()
	ctx := context.Background()
	_ = m.EnsureKey(ctx, "s1")
	c, _ := m.Encrypt(ctx, "s1", []byte(`"x"`))
	vals := make([]Value[string], kms.MaxBatchItems+1)
	buf := NewBuffer(m, nil)
	for i := range vals {
		vals[i] = FromCiphertext[string]("s1", c)
		_ = Reveal(buf, &vals[i])
	}
	if err := buf.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if _, calls, _ := m.Calls(); calls != 2 {
		t.Fatalf("1001 reveals = 2 chunks, got %d calls", calls)
	}
}

func TestCacheHitNeedsNoFlushAndIsExactCiphertext(t *testing.T) {
	m := kms.NewMemory()
	cache := NewCache(0)
	v := sealed(t, m, "s1", "x")
	cache.Put("s1", v.Ciphertext(), []byte(`"x"`))
	buf := NewBuffer(m, cache)
	_ = Reveal(buf, &v)
	if got, _ := v.Get(); got != "x" || buf.Len() != 0 {
		t.Fatal("a cache hit settles at once")
	}
	other := FromCiphertext[string]("s1", "different")
	_ = Reveal(buf, &other)
	if buf.Len() != 1 {
		t.Fatal("the cache answers only the exact ciphertext it was filled with")
	}
}

func TestEncryptPrimesTheCache(t *testing.T) {
	m := kms.NewMemory()
	cache := NewCache(0)
	k, _ := NewFresh("x").Encrypt(context.Background(), m, "s1", cache)
	if p, ok := cache.Get("s1", k.Ciphertext()); !ok || string(p) != `"x"` {
		t.Fatal("encrypting must prime the cache with what it just sealed")
	}
}

func TestMarkErasedDropsTheSubjectAndRefusesLateFills(t *testing.T) {
	c := NewCache(0)
	c.Put("s1", "c1", []byte("1"))
	c.Put("s2", "c2", []byte("2"))
	c.MarkErased("s1")
	if _, ok := c.Get("s1", "c1"); ok || c.Len() != 1 {
		t.Fatal("the erased subject's entries must go, only theirs")
	}
	c.Put("s1", "c1", []byte("1")) // a flush that lost the race with the erasure
	if _, ok := c.Get("s1", "c1"); ok {
		t.Fatal("a late fill for an erased subject must be ignored")
	}
}

func TestCacheIsLRUBounded(t *testing.T) {
	c := NewCache(2)
	c.Put("s", "a", nil)
	c.Put("s", "b", nil)
	c.Get("s", "a") // a is now most recent
	c.Put("s", "c", nil)
	if _, ok := c.Get("s", "b"); ok || c.Len() != 2 {
		t.Fatal("the least recently used entry must be evicted")
	}
}

// ---- data subject lifecycle ----

func openStore(t *testing.T) *events.Store {
	t.Helper()
	s, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func erase(t *testing.T, r *decider.Registry, id string) []events.Event {
	t.Helper()
	evs, err := r.Handle(context.Background(), SubjectAggregate, id, decider.Command{Name: EraseSubjectCommand, Payload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestEraseSubjectIsIdempotentAndStatusSeesIt(t *testing.T) {
	store := openStore(t)
	r := decider.NewRegistry(store)
	RegisterDataSubjects(r)
	status := StoreSubjectStatus{Loader: store}
	ctx := context.Background()
	if gone, _ := status.IsErased(ctx, "s1"); gone {
		t.Fatal("not erased yet")
	}
	if evs := erase(t, r, "s1"); len(evs) != 1 || evs[0].Type != SubjectErasedEvent {
		t.Fatalf("want one SubjectErased, got %v", evs)
	}
	if evs := erase(t, r, "s1"); len(evs) != 0 {
		t.Fatal("erasing twice must append nothing")
	}
	if gone, _ := status.IsErased(ctx, "s1"); !gone {
		t.Fatal("status must see the erasure")
	}
	if _, err := r.Handle(ctx, SubjectAggregate, "s1", decider.Command{Name: "Reactivate"}); !decider.IsRejection(err) {
		t.Fatalf("an unknown command is a rejection: %v", err)
	}
}

func TestKeyDestroyerAndEvictorFollowTheLog(t *testing.T) {
	store := openStore(t)
	r := decider.NewRegistry(store)
	RegisterDataSubjects(r)
	m := kms.NewMemory()
	ctx := context.Background()
	_ = m.EnsureKey(ctx, "s1")
	_ = m.EnsureKey(ctx, "s2")

	engine := consumers.NewEngine(store, nil)
	engine.Register(KeyDestroyer{KMS: m})
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	erase(t, r, "s1")
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if !kms.IsSubjectErased(m.EnsureKey(ctx, "s1")) {
		t.Fatal("the destroyer must destroy the erased subject's key")
	}
	if err := m.EnsureKey(ctx, "s2"); err != nil {
		t.Fatal("and only theirs")
	}

	// The evictor starts at the destroyer's checkpoint: s1's erasure is
	// behind it (its key is gone, so a reveal would say redacted anyway),
	// s2's is ahead and must be seen.
	cache := NewCache(0)
	cache.Put("s1", "c", []byte("1")) // would have been refused by the facade; proves it isn't rescanned
	cache.Put("s2", "c", []byte("2"))
	if _, err := RegisterCacheEvictor(ctx, engine, cache, store, nil); err != nil {
		t.Fatal(err)
	}
	engine.Unregister(KeyDestroyerName) // the destroyer lags behind the erasure below
	erase(t, r, "s2")
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if !cache.IsErased("s2") {
		t.Fatal("an erasure past the destroyer's checkpoint must reach the cache")
	}
	if cache.IsErased("s1") {
		t.Fatal("the evictor must start at the destroyer's checkpoint, not position 0")
	}
	if pos, _ := store.Checkpoint(ctx, "pii:cache-evictor"); pos != 0 {
		t.Fatal("the evictor's position is the process's, never the store's")
	}
}

func TestRevealedPendingStateIsWhatDecideNeeds(t *testing.T) {
	// Get on Pending wraps decider.ErrRevealRequired and is not a rejection
	// by itself — the shell (G2) decides what to do with it.
	_, err := FromCiphertext[string]("s1", "c").Get()
	var rr *RevealRequiredError
	if !errors.As(err, &rr) || rr.SubjectID != "s1" || !errors.Is(err, decider.ErrRevealRequired) {
		t.Fatalf("got %v", err)
	}
}
