package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenReadOnlyRejectsWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.db")

	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := writer.Append(ctx, "task", "t1", 0, []NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}
	writer.Close()

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ro.Close() })

	// reads work fine
	stream, err := ro.LoadStream(ctx, "task", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stream) != 1 {
		t.Fatalf("expected 1 event, got %d", len(stream))
	}

	// every write method fails fast with ErrReadOnly, not an opaque
	// SQLite error
	if _, err := ro.Append(ctx, "task", "t1", 1, []NewEvent{
		{Type: "TaskCompleted", Data: json.RawMessage(`{}`)},
	}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Append: expected ErrReadOnly, got %v", err)
	}
	if err := ro.SaveCheckpoint(ctx, "consumer", 1); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("SaveCheckpoint: expected ErrReadOnly, got %v", err)
	}
	if err := ro.SetMeta(ctx, "k", "v"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("SetMeta: expected ErrReadOnly, got %v", err)
	}
	if err := ro.SetMode(ctx, ModeMaintenance); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("SetMode: expected ErrReadOnly, got %v", err)
	}
}

func TestOpenReadOnlyDoesNotCreateMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist.db")

	_, err := OpenReadOnly(path)
	if err == nil {
		t.Fatal("expected an error opening a nonexistent database read-only")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("OpenReadOnly must not create the file it failed to open")
	}
}

func TestAppendAndLoadStream(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	appended, err := s.Append(ctx, "task", "t1", 0, []NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{"title":"a"}`)},
		{Type: "TaskCompleted", Data: json.RawMessage(`{}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(appended) != 2 {
		t.Fatalf("expected 2 events, got %d", len(appended))
	}
	if appended[0].Sequence != 1 || appended[1].Sequence != 2 {
		t.Fatalf("unexpected sequences: %d, %d", appended[0].Sequence, appended[1].Sequence)
	}
	if appended[0].Position <= 0 || appended[1].Position <= appended[0].Position {
		t.Fatalf("positions not increasing: %d, %d", appended[0].Position, appended[1].Position)
	}

	stream, err := s.LoadStream(ctx, "task", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stream) != 2 || stream[0].Type != "TaskCreated" || stream[1].Type != "TaskCompleted" {
		t.Fatalf("unexpected stream: %+v", stream)
	}

	// a different stream is independent
	if _, err := s.Append(ctx, "task", "t2", 0, []NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{"title":"b"}`)},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStoreUpcaster(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if _, err := s.Append(ctx, "note", "n1", 0, []NewEvent{
		{Type: "NoteCreated", Data: json.RawMessage(`{"text":"old"}`), Version: 1},
	}); err != nil {
		t.Fatal(err)
	}

	// no upcaster installed: reads pass through untouched
	stream, err := s.LoadStream(ctx, "note", "n1")
	if err != nil || stream[0].Version != 1 {
		t.Fatalf("unexpected stream: %+v (err=%v)", stream, err)
	}

	// install an upcaster: LoadStream and Poll both see the upcast view
	s.SetUpcaster(func(ev Event) (Event, error) {
		if ev.Type == "NoteCreated" && ev.Version == 1 {
			ev.Data = json.RawMessage(`{"text":"old","priority":0}`)
			ev.Version = 2
		}
		return ev, nil
	})

	stream, err = s.LoadStream(ctx, "note", "n1")
	if err != nil {
		t.Fatal(err)
	}
	if stream[0].Version != 2 || string(stream[0].Data) != `{"text":"old","priority":0}` {
		t.Fatalf("LoadStream not upcast: %+v", stream[0])
	}

	polled, err := s.Poll(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(polled) != 1 || polled[0].Version != 2 {
		t.Fatalf("Poll not upcast: %+v", polled)
	}

	// a failing upcaster fails the read (consumers must not see raw shapes)
	s.SetUpcaster(func(ev Event) (Event, error) {
		return ev, errors.New("boom")
	})
	if _, err := s.LoadStream(ctx, "note", "n1"); err == nil {
		t.Fatal("expected upcast error")
	}
	if _, err := s.Poll(ctx, 0, 10); err == nil {
		t.Fatal("expected upcast error")
	}

	// disabling restores pass-through
	s.SetUpcaster(nil)
	stream, err = s.LoadStream(ctx, "note", "n1")
	if err != nil || stream[0].Version != 1 {
		t.Fatalf("unexpected stream after disable: %+v (err=%v)", stream, err)
	}
}

func TestMetaAndSystemMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// meta defaults to absent; mode defaults to running
	if v, err := s.GetMeta(ctx, "nope"); err != nil || v != "" {
		t.Fatalf("expected empty meta, got %q (err=%v)", v, err)
	}
	if mode, err := s.Mode(ctx); err != nil || mode != ModeRunning {
		t.Fatalf("expected default mode %s, got %q (err=%v)", ModeRunning, mode, err)
	}

	// mode round-trips, and survives a reopen (durability)
	if err := s.SetMode(ctx, ModeMaintenance); err != nil {
		t.Fatal(err)
	}
	if mode, _ := s.Mode(ctx); mode != ModeMaintenance {
		t.Fatalf("expected %s, got %q", ModeMaintenance, mode)
	}
	if err := s.SetMode(ctx, "bogus"); err == nil {
		t.Fatal("expected invalid mode rejection")
	}
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if mode, _ := s2.Mode(ctx); mode != ModeMaintenance {
		t.Fatalf("mode not durable across reopen: %q", mode)
	}

	// generic kv upsert
	if err := s2.SetMeta(ctx, "k", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := s2.SetMeta(ctx, "k", "v2"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s2.GetMeta(ctx, "k"); v != "v2" {
		t.Fatalf("expected upsert to win, got %q", v)
	}
}

func TestAppendConcurrencyConflict(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if _, err := s.Append(ctx, "task", "t1", 0, []NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{"title":"a"}`)},
	}); err != nil {
		t.Fatal(err)
	}

	// stale expected sequence must be rejected
	_, err := s.Append(ctx, "task", "t1", 0, []NewEvent{
		{Type: "TaskCompleted", Data: json.RawMessage(`{}`)},
	})
	if !errors.Is(err, ErrConcurrency) {
		t.Fatalf("expected ErrConcurrency, got %v", err)
	}

	// correct sequence succeeds
	if _, err := s.Append(ctx, "task", "t1", 1, []NewEvent{
		{Type: "TaskCompleted", Data: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPollAndCheckpoints(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	for range 3 {
		if _, err := s.Append(ctx, "task", newID(), 0, []NewEvent{
			{Type: "TaskCreated", Data: json.RawMessage(`{"title":"x"}`)},
		}); err != nil {
			t.Fatal(err)
		}
	}

	batch, err := s.Poll(ctx, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 2 {
		t.Fatalf("expected 2 events, got %d", len(batch))
	}

	if err := s.SaveCheckpoint(ctx, "p1", batch[len(batch)-1].Position); err != nil {
		t.Fatal(err)
	}
	pos, err := s.Checkpoint(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}

	rest, err := s.Poll(ctx, pos, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 {
		t.Fatalf("expected 1 remaining event, got %d", len(rest))
	}
}

func TestListStreams(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	for _, id := range []string{"b1", "a1", "a2"} {
		if _, err := s.Append(ctx, "note", id, 0, []NewEvent{
			{Type: "NoteCreated", Data: json.RawMessage(`{}`)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// a different aggregate must not leak in
	if _, err := s.Append(ctx, "task", "t1", 0, []NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}

	ids, err := s.ListStreams(ctx, "note")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[0] != "a1" || ids[1] != "a2" || ids[2] != "b1" {
		t.Fatalf("unexpected streams: %v", ids)
	}
}

func TestQueryEvents(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	seed := []struct {
		aggregate, id, typ string
	}{
		{"task", "t1", "TaskCreated"},   // pos 1
		{"task", "t1", "TaskCompleted"}, // pos 2
		{"note", "n1", "NoteCreated"},   // pos 3
		{"task", "t2", "TaskCreated"},   // pos 4
	}
	seqs := map[string]int64{}
	for _, e := range seed {
		key := e.aggregate + "/" + e.id
		if _, err := s.Append(ctx, e.aggregate, e.id, seqs[key], []NewEvent{
			{Type: e.typ, Data: json.RawMessage(`{}`)},
		}); err != nil {
			t.Fatal(err)
		}
		seqs[key]++
	}

	positions := func(evs []Event) []int64 {
		out := make([]int64, len(evs))
		for i, ev := range evs {
			out[i] = ev.Position
		}
		return out
	}
	assert := func(q EventQuery, want []int64) {
		t.Helper()
		evs, err := s.QueryEvents(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		got := positions(evs)
		if len(got) != len(want) {
			t.Fatalf("query %+v: expected positions %v, got %v", q, want, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("query %+v: expected positions %v, got %v", q, want, got)
			}
		}
	}

	assert(EventQuery{After: 0, Limit: 10}, []int64{1, 2, 3, 4})
	assert(EventQuery{After: 0, Limit: 2}, []int64{1, 2})
	assert(EventQuery{After: 2, Limit: 2}, []int64{3, 4})
	assert(EventQuery{After: 4, Limit: 10}, nil)
	assert(EventQuery{After: 0, Limit: 10, Aggregate: "task"}, []int64{1, 2, 4})
	assert(EventQuery{After: 0, Limit: 10, Type: "TaskCreated"}, []int64{1, 4})
	assert(EventQuery{After: 0, Limit: 10, Aggregate: "task", Type: "TaskCreated"}, []int64{1, 4})
	// a zero limit falls back to the default batch size
	assert(EventQuery{}, []int64{1, 2, 3, 4})

	// AggregateID narrows to a single stream (t1 and t2 are both "task")
	assert(EventQuery{Limit: 10, Aggregate: "task", AggregateID: "t1"}, []int64{1, 2})
	assert(EventQuery{Limit: 10, Aggregate: "task", AggregateID: "t2"}, []int64{4})
	assert(EventQuery{Limit: 10, AggregateID: "n1"}, []int64{3})
	assert(EventQuery{Limit: 10, Aggregate: "task", AggregateID: "t1", Type: "TaskCompleted"}, []int64{2})
	assert(EventQuery{Limit: 10, AggregateID: "nope"}, nil)
}

// TestQueryEventsBefore covers backwards paging: Before takes the batch from
// the top of the range and the result still comes back ascending.
func TestQueryEventsBefore(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	seed := []struct{ aggregate, id, typ string }{
		{"task", "t1", "TaskCreated"},   // pos 1
		{"task", "t1", "TaskCompleted"}, // pos 2
		{"note", "n1", "NoteCreated"},   // pos 3
		{"task", "t2", "TaskCreated"},   // pos 4
		{"note", "n1", "NoteArchived"},  // pos 5
	}
	seqs := map[string]int64{}
	for _, e := range seed {
		key := e.aggregate + "/" + e.id
		if _, err := s.Append(ctx, e.aggregate, e.id, seqs[key], []NewEvent{
			{Type: e.typ, Data: json.RawMessage(`{}`)},
		}); err != nil {
			t.Fatal(err)
		}
		seqs[key]++
	}

	assert := func(q EventQuery, want []int64) {
		t.Helper()
		evs, err := s.QueryEvents(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]int64, len(evs))
		for i, ev := range evs {
			got[i] = ev.Position
		}
		if len(got) != len(want) {
			t.Fatalf("query %+v: expected positions %v, got %v", q, want, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("query %+v: expected positions %v, got %v", q, want, got)
			}
		}
	}

	// Before is exclusive, and the batch comes back ascending even though
	// the scan runs descending.
	assert(EventQuery{Before: 6, Limit: 10}, []int64{1, 2, 3, 4, 5})
	assert(EventQuery{Before: 3, Limit: 10}, []int64{1, 2})
	assert(EventQuery{Before: 1, Limit: 10}, nil)

	// the batch is taken from the Before end, not the start of the log
	assert(EventQuery{Before: 6, Limit: 2}, []int64{4, 5})
	assert(EventQuery{Before: 4, Limit: 2}, []int64{2, 3})

	// filters still apply while paging backwards
	assert(EventQuery{Before: 6, Limit: 2, Aggregate: "task"}, []int64{2, 4})
	assert(EventQuery{Before: 6, Limit: 10, AggregateID: "n1"}, []int64{3, 5})

	// both bounds set: After is a floor guard, NOT the start of the window —
	// the range 2..5 exceeds the limit, so the top of it wins.
	assert(EventQuery{After: 1, Before: 6, Limit: 2}, []int64{4, 5})
	// ...and After still excludes what is below it
	assert(EventQuery{After: 3, Before: 6, Limit: 10}, []int64{4, 5})
}

func TestListStreamInfos(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	appendN := func(aggregate, id string, n int) {
		t.Helper()
		for i := range n {
			if _, err := s.Append(ctx, aggregate, id, int64(i), []NewEvent{
				{Type: "SomethingHappened", Data: json.RawMessage(`{}`)},
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	appendN("note", "a1", 2) // pos 1,2
	appendN("note", "a2", 1) // pos 3
	appendN("task", "t1", 1) // pos 4

	all, err := s.ListStreamInfos(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 streams, got %d", len(all))
	}
	want := []StreamInfo{
		{Aggregate: "note", AggregateID: "a1", Events: 2, LastPosition: 2},
		{Aggregate: "note", AggregateID: "a2", Events: 1, LastPosition: 3},
		{Aggregate: "task", AggregateID: "t1", Events: 1, LastPosition: 4},
	}
	for i, w := range want {
		got := all[i]
		if got.Aggregate != w.Aggregate || got.AggregateID != w.AggregateID ||
			got.Events != w.Events || got.LastPosition != w.LastPosition {
			t.Fatalf("row %d: expected %+v, got %+v", i, w, got)
		}
		if got.Updated == "" {
			t.Fatalf("row %d: empty Updated", i)
		}
	}

	notes, err := s.ListStreamInfos(ctx, "note")
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 2 || notes[0].Aggregate != "note" || notes[1].Aggregate != "note" {
		t.Fatalf("unexpected filtered streams: %+v", notes)
	}
}

func TestStoreMigratesPreVersioningDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")

	// create a pre-versioning schema (no version column) with one row
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE events (
			position     INTEGER PRIMARY KEY AUTOINCREMENT,
			id           TEXT NOT NULL UNIQUE,
			aggregate    TEXT NOT NULL,
			aggregate_id TEXT NOT NULL,
			sequence     INTEGER NOT NULL,
			type         TEXT NOT NULL,
			data         TEXT NOT NULL,
			metadata     TEXT NOT NULL DEFAULT '{}',
			created      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%fZ', 'now')),
			UNIQUE (aggregate, aggregate_id, sequence)
		);
		INSERT INTO events (id, aggregate, aggregate_id, sequence, type, data)
		VALUES ('e1', 'note', 'n1', 1, 'NoteCreated', '{}');
	`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// opening migrates it: old rows read as version 1, new appends work
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	stream, err := s.LoadStream(ctx, "note", "n1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stream) != 1 || stream[0].Version != 1 {
		t.Fatalf("expected migrated row at version 1, got %+v", stream)
	}

	appended, err := s.Append(ctx, "note", "n1", 1, []NewEvent{
		{Type: "NoteArchived", Data: json.RawMessage(`{}`), Version: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if appended[0].Version != 2 {
		t.Fatalf("expected version 2, got %d", appended[0].Version)
	}
}
