package consumers

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jamestryand/pocketcqrs/events"
)

type recorder struct {
	mu   sync.Mutex
	seen []events.Event
}

func (r *recorder) Name() string { return "rec" }
func (r *recorder) Apply(ctx context.Context, ev events.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, ev)
	return nil
}
func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

func openStore(t *testing.T, dir *string) *events.Store {
	t.Helper()
	if *dir == "" {
		*dir = t.TempDir()
	}
	s, err := events.Open(filepath.Join(*dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func appendOne(t *testing.T, s *events.Store, id string) {
	t.Helper()
	_, err := s.Append(context.Background(), "task", id, 0,
		[]events.NewEvent{{Type: "TaskCreated", Data: json.RawMessage(`{"title":"x"}`)}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunOnceDeliversInOrderWithCheckpoint(t *testing.T) {
	var dir string
	store := openStore(t, &dir)
	defer store.Close()
	ctx := context.Background()

	appendOne(t, store, "t1")
	appendOne(t, store, "t2")

	rec := &recorder{}
	engine := NewEngine(store, nil)
	engine.Register(rec)

	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 2 {
		t.Fatalf("expected 2 delivered, got %d", rec.count())
	}
	if rec.seen[0].Position >= rec.seen[1].Position {
		t.Fatal("events delivered out of order")
	}

	// checkpoint advanced: a second pass delivers nothing new
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 2 {
		t.Fatalf("expected still 2 after second pass, got %d", rec.count())
	}
}

func TestUnregisterStopsDeliveryAndCheckpointResumes(t *testing.T) {
	var dir string
	store := openStore(t, &dir)
	defer store.Close()
	ctx := context.Background()

	rec := &recorder{}
	engine := NewEngine(store, nil)
	engine.Register(rec)

	appendOne(t, store, "t1")
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 1 {
		t.Fatalf("expected 1 delivered, got %d", rec.count())
	}

	// unregistered consumers get no deliveries...
	engine.Unregister(rec.Name())
	engine.Unregister("no-such-consumer") // absent: no-op
	appendOne(t, store, "t2")
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 1 {
		t.Fatalf("expected no delivery while unregistered, got %d", rec.count())
	}

	// ...and re-registering resumes from the durable checkpoint
	engine.Register(rec)
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 2 || rec.seen[1].AggregateID != "t2" {
		t.Fatalf("expected checkpoint resume with t2, got %+v", rec.seen)
	}
}

func TestDeliverySurvivesRestart(t *testing.T) {
	var dir string
	ctx := context.Background()

	// first "process": consume one event
	store := openStore(t, &dir)
	appendOne(t, store, "t1")
	rec1 := &recorder{}
	engine := NewEngine(store, nil)
	engine.Register(rec1)
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	store.Close()

	// second event committed "while down"
	store2 := openStore(t, &dir)
	defer store2.Close()
	appendOne(t, store2, "t2")

	// second "process": only the missed event is delivered
	rec2 := &recorder{}
	engine2 := NewEngine(store2, nil)
	engine2.Register(rec2)
	if err := engine2.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if rec2.count() != 1 {
		t.Fatalf("expected 1 replayed event, got %d", rec2.count())
	}
	if rec2.seen[0].AggregateID != "t2" {
		t.Fatalf("replayed wrong event: %+v", rec2.seen[0])
	}
}
