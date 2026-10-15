package extcaller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/outbound"
	"github.com/jamestryand/pocketcqrs/outbox"
)

func newTestOutbox(t *testing.T) *outbox.Store {
	t.Helper()
	s, err := outbox.Open(filepath.Join(t.TempDir(), "outbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// applyAndRun is the engine delivering ev, then the outbox worker taking
// what is due.
func applyAndRun(t *testing.T, c *Consumer, ev events.Event) error {
	t.Helper()
	if err := c.Apply(context.Background(), ev); err != nil {
		return err
	}
	if _, err := c.Worker().RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	return nil
}

// counted is a test server counting its calls by path.
type counted struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *counted) hit(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[path]++
	return c.calls[path]
}

func (c *counted) n(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[path]
}

func twoFollowUps(thirdHost string) Rule {
	return Rule{
		EventType: "order.Confirmed",
		BuildRequest: func(ev events.Event) (outbound.Request, error) {
			return outbound.Request{Method: "GET", URL: "http://" + thirdHost + "/lookup"}, nil
		},
		HandleResponse: func(ev events.Event, resp *outbound.Response) ([]FollowUp, error) {
			return []FollowUp{
				{Aggregate: "task", ID: "a", Name: "Create", Payload: json.RawMessage(`{}`)},
				{Aggregate: "task", ID: "b", Name: "Create", Payload: json.RawMessage(`{}`)},
			}, nil
		},
	}
}

// Apply only records the work: the call is the worker's, off the engine's
// pass.
func TestApplyOnlyEnqueues(t *testing.T) {
	third := &counted{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { third.hit(r.URL.Path) }))
	defer srv.Close()
	host := srv.Listener.Addr().String()
	box := newTestOutbox(t)
	c, err := New(Config{Name: "t", Rules: []Rule{twoFollowUps(host)}, Outbound: newTestOutbound(t, hostOnly(t, host)),
		Gateway: NewGatewayClient("http://unused.invalid", "tok", time.Second), DeadLetters: newTestDeadLetters(t), Outbox: box})
	if err != nil {
		t.Fatal(err)
	}
	ev := testEvent("order.Confirmed")
	for range 2 { // a redelivery asks for the same work once
		if err := c.Apply(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	if third.n("/lookup") != 0 {
		t.Fatal("Apply called out")
	}
	if pending, _ := box.Pending(context.Background()); len(pending) != 1 {
		t.Fatalf("pending %d, want 1", len(pending))
	}
}

// A failed call is due again after the backoff, never a pause; once the
// attempts are spent it dead-letters.
func TestAFailedCallIsRetriedByDueTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	host := srv.Listener.Addr().String()
	srv.Close() // nothing listens: the call fails
	now := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	box := newTestOutbox(t)
	dead := newTestDeadLetters(t)
	c, err := New(Config{Name: "t", Rules: []Rule{twoFollowUps(host)}, Outbound: newTestOutbound(t, hostOnly(t, host)),
		Gateway: NewGatewayClient("http://unused.invalid", "tok", time.Second), DeadLetters: dead, Outbox: box,
		Retry: RetryPolicy{MaxAttempts: 3, Backoff: time.Minute}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Apply(ctx, testEvent("order.Confirmed")); err != nil {
		t.Fatal(err)
	}
	w := c.Worker()
	for attempt := 1; attempt <= 2; attempt++ {
		start := time.Now()
		w.RunOnce(ctx)
		if time.Since(start) > 10*time.Second {
			t.Fatal("the worker paused")
		}
		pending, _ := box.Pending(ctx)
		if len(pending) != 1 || !pending[0].Due.Equal(now.Add(time.Minute)) {
			t.Fatalf("attempt %d: %+v", attempt, pending)
		}
		if letters, _ := dead.DeadLetters(ctx, false); len(letters) != 0 {
			t.Fatalf("dead-lettered after %d attempt(s)", attempt)
		}
		now = now.Add(time.Minute)
	}
	w.RunOnce(ctx)
	if pending, _ := box.Pending(ctx); len(pending) != 0 {
		t.Fatalf("still pending after the last attempt: %+v", pending)
	}
	if letters, _ := dead.DeadLetters(ctx, false); len(letters) != 1 {
		t.Fatalf("dead letters %d, want 1", len(letters))
	}
}

// A conflict on a follow-up is tried again by due time, and the gateway
// re-decides it; the third party isn't called again, and the follow-up
// already dispatched isn't dispatched again.
func TestAConflictIsDispatchedAgain(t *testing.T) {
	third := &counted{}
	tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { third.hit(r.URL.Path) }))
	defer tsrv.Close()
	gw := &counted{}
	gsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gw.hit(r.URL.Path) == 1 && r.URL.Path == "/api/cqrs/task/b/Create" {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"concurrency conflict"}`))
			return
		}
		w.Write([]byte(`{"events":[]}`))
	}))
	defer gsrv.Close()
	host := tsrv.Listener.Addr().String()
	now := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	box := newTestOutbox(t)
	dead := newTestDeadLetters(t)
	c, err := New(Config{Name: "t", Rules: []Rule{twoFollowUps(host)}, Outbound: newTestOutbound(t, hostOnly(t, host)),
		Gateway: NewGatewayClient(gsrv.URL, "tok", time.Second), DeadLetters: dead, Outbox: box,
		Retry: RetryPolicy{MaxAttempts: 3, Backoff: time.Second}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := applyAndRun(t, c, testEvent("order.Confirmed")); err != nil {
		t.Fatal(err)
	}
	if pending, _ := box.Pending(ctx); len(pending) != 1 {
		t.Fatal("the conflict wasn't kept to try again")
	}
	now = now.Add(time.Second)
	c.Worker().RunOnce(ctx)
	if pending, _ := box.Pending(ctx); len(pending) != 0 {
		t.Fatalf("still pending: %+v", pending)
	}
	if third.n("/lookup") != 1 || gw.n("/api/cqrs/task/a/Create") != 1 || gw.n("/api/cqrs/task/b/Create") != 2 {
		t.Fatalf("third %d, a %d, b %d", third.n("/lookup"), gw.n("/api/cqrs/task/a/Create"), gw.n("/api/cqrs/task/b/Create"))
	}
	if letters, _ := dead.DeadLetters(ctx, false); len(letters) != 0 {
		t.Fatalf("dead-lettered: %+v", letters)
	}
}

// A domain rejection isn't retried: it dead-letters, as before.
func TestARejectionDeadLetters(t *testing.T) {
	tsrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tsrv.Close()
	gsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"no"}`))
	}))
	defer gsrv.Close()
	host := tsrv.Listener.Addr().String()
	box := newTestOutbox(t)
	dead := newTestDeadLetters(t)
	c, err := New(Config{Name: "t", Rules: []Rule{twoFollowUps(host)}, Outbound: newTestOutbound(t, hostOnly(t, host)),
		Gateway: NewGatewayClient(gsrv.URL, "tok", time.Second), DeadLetters: dead, Outbox: box,
		Retry: RetryPolicy{MaxAttempts: 3, Backoff: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	if err := applyAndRun(t, c, testEvent("order.Confirmed")); err != nil {
		t.Fatal(err)
	}
	if pending, _ := box.Pending(context.Background()); len(pending) != 0 {
		t.Fatal("a rejection was kept to retry")
	}
	if letters, _ := dead.DeadLetters(context.Background(), false); len(letters) != 1 {
		t.Fatalf("dead letters %d, want 1", len(letters))
	}
}
