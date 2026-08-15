package batching

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jamestryand/pocketcqrs/events"
)

func openOverlayTestStore(t *testing.T) *events.Store {
	t.Helper()
	s, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOverlayLoadStreamReturnsDurableWhenNothingPending(t *testing.T) {
	store := openOverlayTestStore(t)
	ctx := context.Background()
	if _, err := store.Append(ctx, "task", "t1", 0, []events.NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}

	ov := newOverlay(store)
	stream, err := ov.LoadStream(ctx, "task", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stream) != 1 || stream[0].Type != "TaskCreated" {
		t.Fatalf("unexpected stream: %+v", stream)
	}
}

func TestOverlayLoadStreamMergesDurableAndPending(t *testing.T) {
	store := openOverlayTestStore(t)
	ctx := context.Background()
	if _, err := store.Append(ctx, "task", "t1", 0, []events.NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}

	ov := newOverlay(store)
	ov.record("task", "t1", []events.NewEvent{{Type: "TaskCompleted", Data: json.RawMessage(`{}`)}}, 1)

	stream, err := ov.LoadStream(ctx, "task", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stream) != 2 {
		t.Fatalf("expected durable + pending = 2 events, got %d", len(stream))
	}
	if stream[0].Type != "TaskCreated" || stream[1].Type != "TaskCompleted" {
		t.Fatalf("unexpected order: %+v", stream)
	}
	if stream[1].Sequence != 2 {
		t.Fatalf("expected the pending event's sequence to follow the durable one, got %d", stream[1].Sequence)
	}
}

func TestOverlayRecordAccumulatesAcrossMultipleCalls(t *testing.T) {
	store := openOverlayTestStore(t)
	ctx := context.Background()

	ov := newOverlay(store)
	ov.record("task", "t1", []events.NewEvent{{Type: "TaskCreated", Data: json.RawMessage(`{}`)}}, 0)
	ov.record("task", "t1", []events.NewEvent{{Type: "TaskCompleted", Data: json.RawMessage(`{}`)}}, 1)

	stream, err := ov.LoadStream(ctx, "task", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stream) != 2 || stream[0].Sequence != 1 || stream[1].Sequence != 2 {
		t.Fatalf("unexpected accumulated stream: %+v", stream)
	}
}

func TestOverlayRecordEmptyIsNoop(t *testing.T) {
	store := openOverlayTestStore(t)
	ctx := context.Background()

	ov := newOverlay(store)
	ov.record("task", "t1", nil, 0)

	stream, err := ov.LoadStream(ctx, "task", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stream) != 0 {
		t.Fatalf("expected no events, got %d", len(stream))
	}
}

func TestOverlayDoesNotLeakBetweenStreams(t *testing.T) {
	store := openOverlayTestStore(t)
	ctx := context.Background()

	ov := newOverlay(store)
	ov.record("task", "t1", []events.NewEvent{{Type: "TaskCreated", Data: json.RawMessage(`{}`)}}, 0)

	stream, err := ov.LoadStream(ctx, "task", "t2")
	if err != nil {
		t.Fatal(err)
	}
	if len(stream) != 0 {
		t.Fatalf("expected t2 unaffected by t1's pending events, got %d", len(stream))
	}
}
