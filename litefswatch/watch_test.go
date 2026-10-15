package litefswatch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Every transaction LiteFS applies to the watched database is a nudge;
// another database's, and the init event, are not. A dropped stream is
// subscribed to again.
func TestWatchNudgesOnEachTxAndResubscribes(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := conns.Add(1)
		enc := json.NewEncoder(w)
		enc.Encode(map[string]any{"type": "init", "data": map[string]any{"isPrimary": false}})
		enc.Encode(map[string]any{"type": "tx", "db": "data.db", "data": map[string]any{"txID": "1"}})
		enc.Encode(map[string]any{"type": "tx", "db": "events.db", "data": map[string]any{"txID": "2"}})
		w.(http.Flusher).Flush()
		if n == 1 {
			return // the stream drops
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var nudges atomic.Int32
	go Watch(ctx, Config{URL: srv.URL + "/events", DB: "events.db", Notify: func() { nudges.Add(1) }, MinWait: 10 * time.Millisecond})

	deadline := time.Now().Add(5 * time.Second)
	for nudges.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := nudges.Load(); got != 2 {
		t.Fatalf("nudges %d, want one per connection's events.db tx (2)", got)
	}
	if conns.Load() != 2 {
		t.Fatalf("connections %d, want 2 (one resubscribe)", conns.Load())
	}
}

// Watch stops when its context does.
func TestWatchStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Watch(ctx, Config{URL: srv.URL, DB: "events.db", Notify: func() {}}); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Watch didn't stop")
	}
}

// It stops while waiting to subscribe again, too.
func TestWatchStopsWhileWaiting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Watch(ctx, Config{URL: srv.URL, DB: "events.db", Notify: func() {}, MinWait: time.Hour})
		close(done)
	}()
	time.Sleep(200 * time.Millisecond) // refused, now waiting an hour
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Watch didn't stop while waiting")
	}
}
