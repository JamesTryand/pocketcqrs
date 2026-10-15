package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "outbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var t0 = time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)

// An entry is enqueued once per id (a redelivered event asks again), and
// only due entries are taken, earliest first.
func TestEnqueueAndDue(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for _, e := range []struct {
		id  string
		due time.Time
	}{{"b", t0.Add(2 * time.Second)}, {"a", t0}, {"later", t0.Add(time.Hour)}, {"a", t0.Add(time.Minute)}} {
		if err := s.Enqueue(ctx, e.id, "c", json.RawMessage(`{"n":1}`), e.due); err != nil {
			t.Fatal(err)
		}
	}
	due, err := s.Due(ctx, t0.Add(5*time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 || due[0].ID != "a" || due[1].ID != "b" || !due[0].Due.Equal(t0) {
		t.Fatalf("due: %+v", due)
	}
	next, ok, err := s.NextDue(ctx)
	if err != nil || !ok || !next.Equal(t0) {
		t.Fatalf("next due %v %v %v", next, ok, err)
	}
}

// A handler that retries reschedules the entry with its attempt counted,
// its new state and the error, and it isn't taken again before it is due.
// Done takes it off.
func TestWorkerRetriesByDueTime(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := t0
	calls := 0
	w := &Worker{Store: s, Now: func() time.Time { return now }, Handlers: map[string]Handler{
		"c": func(_ context.Context, e Entry) Outcome {
			calls++
			if e.Attempts < 2 {
				return Outcome{RetryAt: now.Add(time.Minute), Payload: json.RawMessage(`{"n":2}`), Err: errors.New("busy")}
			}
			return Outcome{Done: true}
		},
	}}
	if err := s.Enqueue(ctx, "x", "c", json.RawMessage(`{"n":1}`), now); err != nil {
		t.Fatal(err)
	}
	if n, err := w.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("first pass: %d, %v", n, err)
	}
	pending, err := s.Pending(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending %v %v", pending, err)
	}
	if e := pending[0]; e.Attempts != 1 || string(e.Payload) != `{"n":2}` || e.LastError != "busy" || !e.Due.Equal(t0.Add(time.Minute)) {
		t.Fatalf("rescheduled as %+v", e)
	}
	now = t0.Add(30 * time.Second)
	if n, _ := w.RunOnce(ctx); n != 0 {
		t.Fatal("taken before it was due")
	}
	now = t0.Add(time.Minute)
	w.RunOnce(ctx)
	now = t0.Add(2 * time.Minute)
	w.RunOnce(ctx)
	if calls != 3 {
		t.Fatalf("calls %d, want 3", calls)
	}
	if pending, _ := s.Pending(ctx); len(pending) != 0 {
		t.Fatalf("done but still there: %+v", pending)
	}
}

// An entry for a consumer with no handler waits; it isn't dropped.
func TestEntryWithoutAHandlerWaits(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	w := &Worker{Store: s, Now: func() time.Time { return t0 }, Handlers: map[string]Handler{}}
	if err := s.Enqueue(ctx, "x", "gone", json.RawMessage(`{}`), t0); err != nil {
		t.Fatal(err)
	}
	if n, _ := w.RunOnce(ctx); n != 0 {
		t.Fatal("handled with no handler")
	}
	if pending, _ := s.Pending(ctx); len(pending) != 1 {
		t.Fatal("dropped")
	}
}

// Run wakes when the next entry is due, and at once when nudged: it waits
// for the due time, never a fixed pause.
func TestRunWakesWhenDueAndWhenNudged(t *testing.T) {
	s := open(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	handled := map[string]time.Time{}
	w := &Worker{Store: s, Handlers: map[string]Handler{
		"c": func(_ context.Context, e Entry) Outcome {
			mu.Lock()
			handled[e.ID] = time.Now()
			mu.Unlock()
			return Outcome{Done: true}
		},
	}}
	go w.Run(ctx)
	time.Sleep(200 * time.Millisecond) // the worker is idle, waiting: only a nudge can wake it now
	start := time.Now()
	// nothing nudges the worker but the enqueue itself
	if err := s.Enqueue(ctx, "later", "c", json.RawMessage(`{}`), start.Add(300*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(ctx, "now", "c", json.RawMessage(`{}`), start); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(handled)
		mu.Unlock()
		if n == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond) // the test waits on the worker; the worker never sleeps
	}
	mu.Lock()
	defer mu.Unlock()
	if len(handled) != 2 {
		t.Fatalf("handled %v", handled)
	}
	if handled["now"].Sub(start) > 200*time.Millisecond {
		t.Errorf("the nudged entry waited %v", handled["now"].Sub(start))
	}
	if d := handled["later"].Sub(start); d < 300*time.Millisecond || d > 2*time.Second {
		t.Errorf("the later entry ran after %v, want about 300ms", d)
	}
}

// An entry no handler here can take doesn't mask the next one that can.
func TestNextDueIsAmongTheWorkersOwn(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	_ = s.Enqueue(ctx, "orphan", "gone", json.RawMessage(`{}`), t0)
	_ = s.Enqueue(ctx, "mine", "c", json.RawMessage(`{}`), t0.Add(time.Hour))
	next, ok, err := s.NextDue(ctx, "c")
	if err != nil || !ok || !next.Equal(t0.Add(time.Hour)) {
		t.Fatalf("next due for c: %v %v %v", next, ok, err)
	}
}
