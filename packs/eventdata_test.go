package packs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/events"
)

func openEventStore(t *testing.T) *events.Store {
	t.Helper()
	s, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestExportEventsWritesNdjsonWithoutPosition(t *testing.T) {
	ctx := context.Background()
	store := openEventStore(t)

	if _, err := store.Append(ctx, "task", "t1", 0, []events.NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{"title":"a"}`)},
		{Type: "TaskCompleted", Data: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, "task", "t2", 0, []events.NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{"title":"b"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	// a different aggregate, NOT named in the export selection — must be
	// absent from the file entirely
	if _, err := store.Append(ctx, "order", "o1", 0, []events.NewEvent{
		{Type: "OrderPlaced", Data: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}

	outFile := filepath.Join(t.TempDir(), eventsFile)
	count, err := ExportEvents(ctx, store, []string{"task"}, outFile)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("expected 3 exported events, got %d", count)
	}

	raw, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"position"`) {
		t.Fatalf("expected position to be entirely absent from the export, got:\n%s", raw)
	}
	if strings.Contains(string(raw), `"order"`) {
		t.Fatalf("expected the non-selected aggregate to be absent, got:\n%s", raw)
	}

	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 ndjson lines, got %d", len(lines))
	}
	var first, second exportedEvent
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	// stable order: t1's own two events stay in sequence order and land
	// before t2's single event (streams sorted by id within the aggregate)
	if first.AggregateID != "t1" || first.Sequence != 1 || second.AggregateID != "t1" || second.Sequence != 2 {
		t.Fatalf("unexpected export order: %+v, %+v", first, second)
	}
}

func TestExportEventsRefusesEmptyAggregates(t *testing.T) {
	store := openEventStore(t)
	if _, err := ExportEvents(context.Background(), store, nil, filepath.Join(t.TempDir(), eventsFile)); err == nil {
		t.Fatal("expected a refusal for an empty aggregates list")
	}
}

func TestExportEventsReadsRaw(t *testing.T) {
	ctx := context.Background()
	store := openEventStore(t)
	if _, err := store.Append(ctx, "note", "n1", 0, []events.NewEvent{
		{Type: "NoteCreated", Data: json.RawMessage(`{"text":"old"}`), Version: 1},
	}); err != nil {
		t.Fatal(err)
	}
	store.SetUpcaster(func(ev events.Event) (events.Event, error) {
		if ev.Type == "NoteCreated" && ev.Version == 1 {
			ev.Data = json.RawMessage(`{"text":"old","priority":0}`)
			ev.Version = 2
		}
		return ev, nil
	})

	outFile := filepath.Join(t.TempDir(), eventsFile)
	if _, err := ExportEvents(ctx, store, []string{"note"}, outFile); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	var ev exportedEvent
	if err := json.Unmarshal(raw[:len(raw)-1], &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Version != 1 || string(ev.Data) != `{"text":"old"}` {
		t.Fatalf("expected the exported event to be the RAW (pre-upcast) row, got %+v", ev)
	}
}

// writeEventsFile is a test helper mirroring what ExportEvents produces,
// for tests that want to construct a fixture directly rather than round-trip
// through a real store first.
func writeEventsFile(t *testing.T, path string, evs []exportedEvent) {
	t.Helper()
	var sb strings.Builder
	for _, ev := range evs {
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		sb.Write(raw)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestImportEventsAdvancesEffectTierNotProjections(t *testing.T) {
	ctx := context.Background()
	store := openEventStore(t)

	// seed every kind of consumer's checkpoint at 0 before the import
	for _, name := range []string{"reactor:fulfillment", "fn-reactor:notify", "fn:audit", "tasks", "js-projection:orders_by_customer"} {
		if err := store.SaveCheckpoint(ctx, name, 0); err != nil {
			t.Fatal(err)
		}
	}

	evtsPath := filepath.Join(t.TempDir(), eventsFile)
	writeEventsFile(t, evtsPath, []exportedEvent{
		{ID: "e1", Aggregate: "task", AggregateID: "t1", Sequence: 1, Type: "TaskCreated",
			Data: json.RawMessage(`{}`), Version: 1, Created: "2020-01-01 00:00:00.000Z"},
		{ID: "e2", Aggregate: "task", AggregateID: "t1", Sequence: 2, Type: "TaskCompleted",
			Data: json.RawMessage(`{}`), Version: 1, Created: "2020-01-01 00:00:01.000Z"},
	})

	consumerNames := []string{"reactor:fulfillment", "fn-reactor:notify", "fn:audit", "tasks", "js-projection:orders_by_customer"}
	result, err := ImportEvents(ctx, store, store, consumerNames, evtsPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 2 {
		t.Fatalf("expected 2 imported events, got %d", result.Imported)
	}
	if len(result.Streams) != 1 || result.Streams[0] != "task/t1" {
		t.Fatalf("expected Streams=[task/t1], got %+v", result.Streams)
	}

	maxPos, err := store.MaxPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if maxPos == 0 {
		t.Fatal("expected a nonzero max position after import")
	}

	for _, name := range []string{"reactor:fulfillment", "fn-reactor:notify", "fn:audit"} {
		got, ok := result.AdvancedCheckpoints[name]
		if !ok {
			t.Fatalf("expected %s to be listed as advanced", name)
		}
		if got != maxPos {
			t.Fatalf("expected %s advanced to %d, got %d", name, maxPos, got)
		}
		stored, err := store.Checkpoint(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if stored != maxPos {
			t.Fatalf("expected %s's DURABLE checkpoint to be %d, got %d", name, maxPos, stored)
		}
	}

	for _, name := range []string{"tasks", "js-projection:orders_by_customer"} {
		if _, ok := result.AdvancedCheckpoints[name]; ok {
			t.Fatalf("expected projection consumer %s to be left OUT of AdvancedCheckpoints", name)
		}
		stored, err := store.Checkpoint(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if stored != 0 {
			t.Fatalf("expected projection consumer %s's checkpoint untouched at 0, got %d", name, stored)
		}
	}
}

func TestImportEventsDryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	store := openEventStore(t)
	if err := store.SaveCheckpoint(ctx, "fn:audit", 0); err != nil {
		t.Fatal(err)
	}

	evtsPath := filepath.Join(t.TempDir(), eventsFile)
	writeEventsFile(t, evtsPath, []exportedEvent{
		{ID: "e1", Aggregate: "task", AggregateID: "t1", Sequence: 1, Type: "TaskCreated",
			Data: json.RawMessage(`{}`), Version: 1, Created: "2020-01-01 00:00:00.000Z"},
	})

	dry, err := ImportEvents(ctx, store, store, []string{"fn:audit"}, evtsPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || dry.Imported != 1 || dry.AdvancedCheckpoints["fn:audit"] == 0 {
		t.Fatalf("unexpected dry-run result: %+v", dry)
	}

	// nothing written: no stream, checkpoint untouched
	streams, err := store.ListStreamInfos(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) != 0 {
		t.Fatalf("expected no streams after a dry run, got %+v", streams)
	}
	cp, err := store.Checkpoint(ctx, "fn:audit")
	if err != nil {
		t.Fatal(err)
	}
	if cp != 0 {
		t.Fatalf("expected fn:audit's checkpoint untouched at 0 after a dry run, got %d", cp)
	}

	// a REAL import right after reports the identical counts the dry run
	// predicted
	real, err := ImportEvents(ctx, store, store, []string{"fn:audit"}, evtsPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if real.Imported != dry.Imported || real.AdvancedCheckpoints["fn:audit"] != dry.AdvancedCheckpoints["fn:audit"] {
		t.Fatalf("expected the real import to match the dry run's prediction: dry=%+v real=%+v", dry, real)
	}
}

func TestImportEventsDryRunReportsCollisions(t *testing.T) {
	ctx := context.Background()
	store := openEventStore(t)
	if _, err := store.Append(ctx, "task", "t1", 0, []events.NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}

	evtsPath := filepath.Join(t.TempDir(), eventsFile)
	writeEventsFile(t, evtsPath, []exportedEvent{
		{ID: "e1", Aggregate: "task", AggregateID: "t2", Sequence: 1, Type: "TaskCreated",
			Data: json.RawMessage(`{}`), Version: 1, Created: "2020-01-01 00:00:00.000Z"},
	})

	if _, err := ImportEvents(ctx, store, store, nil, evtsPath, true); err == nil {
		t.Fatal("expected the dry run to report the aggregate-name collision, not silently succeed")
	}
}

func TestImportEventsEmptyFileIsNoOp(t *testing.T) {
	ctx := context.Background()
	store := openEventStore(t)
	evtsPath := filepath.Join(t.TempDir(), eventsFile)
	if err := os.WriteFile(evtsPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := ImportEvents(ctx, store, store, []string{"fn:audit"}, evtsPath, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 0 || len(result.AdvancedCheckpoints) != 0 {
		t.Fatalf("expected a no-op for an empty events file, got %+v", result)
	}
}

func TestExportImportEventDataRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := openEventStore(t)
	if _, err := src.Append(ctx, "task", "t1", 0, []events.NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{"title":"a"}`)},
		{Type: "TaskCompleted", Data: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}

	packDir := t.TempDir()
	outFile := filepath.Join(packDir, eventsFile)
	count, err := ExportEvents(ctx, src, []string{"task"}, outFile)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected 2 exported events, got %d", count)
	}

	dst := openEventStore(t)
	if _, err := ImportEvents(ctx, dst, dst, nil, outFile, false); err != nil {
		t.Fatal(err)
	}

	reExported := filepath.Join(t.TempDir(), eventsFile)
	if _, err := ExportEvents(ctx, dst, []string{"task"}, reExported); err != nil {
		t.Fatal(err)
	}

	original, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	roundTripped, err := os.ReadFile(reExported)
	if err != nil {
		t.Fatal(err)
	}
	if string(original) != string(roundTripped) {
		t.Fatalf("expected byte-identical round-trip (both sides export raw, and position is dropped on both):\noriginal:\n%s\nround-tripped:\n%s",
			original, roundTripped)
	}
}
