package events

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// Every imported event carries the marker that says it happened at another
// deployment, whatever metadata it came with, and keeps that metadata.
func TestImportMarksEveryEvent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, err := s.ImportEvents(ctx, []Event{
		{ID: "a1", Aggregate: "order", AggregateID: "o1", Sequence: 1, Type: "OrderPlaced", Data: json.RawMessage(`{}`),
			Metadata: json.RawMessage(`{"actor":"u1"}`), Created: "2026-10-01 10:00:00.000Z"},
		{ID: "a2", Aggregate: "order", AggregateID: "o1", Sequence: 2, Type: "OrderShipped", Data: json.RawMessage(`{}`),
			Created: "2026-10-01 10:05:00.000Z"},
	})
	if err != nil {
		t.Fatal(err)
	}
	evs, err := s.Poll(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d", len(evs))
	}
	for _, ev := range evs {
		if !IsImported(ev) {
			t.Errorf("%s is not marked imported: %s", ev.ID, ev.Metadata)
		}
	}
	var meta map[string]any
	if err := json.Unmarshal(evs[0].Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["actor"] != "u1" {
		t.Errorf("the event's own metadata was lost: %s", evs[0].Metadata)
	}
}

// A native event is never marked, and can't be: Append and CommitBatch
// refuse metadata carrying the marker, so nothing written here can be
// mistaken for history from elsewhere and skipped by effect consumers.
func TestNativeEventsCannotCarryTheImportMarker(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	native, err := s.Append(ctx, "order", "o1", 0, []NewEvent{{Type: "OrderPlaced", Data: json.RawMessage(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if IsImported(native[0]) {
		t.Fatal("a native event reads as imported")
	}
	for _, meta := range []string{`{"imported":true}`, `{"imported":false}`, `{"actor":"u1","imported":"yes"}`} {
		_, err := s.Append(ctx, "order", "o2", 0, []NewEvent{{Type: "OrderPlaced", Data: json.RawMessage(`{}`), Metadata: json.RawMessage(meta)}})
		if !errors.Is(err, ErrImportMarker) {
			t.Errorf("Append with %s: want ErrImportMarker, got %v", meta, err)
		}
		_, err = s.CommitBatch(ctx, []DecidedCommand{{Aggregate: "order", AggregateID: "o3",
			NewEvents: []NewEvent{{Type: "OrderPlaced", Data: json.RawMessage(`{}`), Metadata: json.RawMessage(meta)}}}})
		if !errors.Is(err, ErrImportMarker) {
			t.Errorf("CommitBatch with %s: want ErrImportMarker, got %v", meta, err)
		}
	}
}
