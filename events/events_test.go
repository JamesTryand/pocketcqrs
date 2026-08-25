package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// TestOpenReadOnlyAgainstDeleteModeCopy reproduces what a Litestream
// `restore -f`-followed secondary copy of events.db actually looks like on
// disk: DELETE journal mode (bytes 18-19 = 0x01,0x01) with a randomized
// schema-change counter (bytes 24-27), the exact header rewrite
// litestream/replica.go's applyLTXFile performs as its own concurrent-reader
// safety mechanism (litestream-vfs-scope.md, "Why DELETE mode, not WAL").
//
// Confirmed live against a real litestream replicate + restore -f cycle
// before this fix landed: without it, OpenReadOnly's old unconditional
// `_pragma=journal_mode(WAL)` failed outright against a file in this shape
// with "attempt to write a readonly database (8)" -- not a silent no-op.
// This test pins that regression without needing a real litestream binary
// in CI.
func TestOpenReadOnlyAgainstDeleteModeCopy(t *testing.T) {
	dir := t.TempDir()
	writerPath := filepath.Join(dir, "events.db")

	writer, err := Open(writerPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := writer.Append(ctx, "task", "t1", 0, []NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}
	// checkpoint the WAL into the main file so a raw byte copy below carries
	// the full, current state -- mirrors what Litestream's own snapshot
	// restore produces (a single self-contained file, no separate -wal).
	if _, err := writer.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	writer.Close()

	raw, err := os.ReadFile(writerPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 28 {
		t.Fatalf("events.db too small to carry a SQLite header: %d bytes", len(raw))
	}
	// the exact rewrite applyLTXFile performs (litestream/replica.go:974-976)
	raw[18], raw[19] = 0x01, 0x01
	raw[24], raw[25], raw[26], raw[27] = 0xe3, 0x2e, 0x37, 0x82

	followedPath := filepath.Join(dir, "followed-events.db")
	if err := os.WriteFile(followedPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	ro, err := OpenReadOnly(followedPath)
	if err != nil {
		t.Fatalf("OpenReadOnly against a DELETE-mode Litestream-followed copy: %v", err)
	}
	t.Cleanup(func() { ro.Close() })

	stream, err := ro.LoadStream(ctx, "task", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stream) != 1 {
		t.Fatalf("expected 1 event, got %d", len(stream))
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

// TestLoadStreamRawBypassesUpcaster is the test that would catch export
// silently going through LoadStream instead of LoadStreamRaw: with an
// upcaster installed, LoadStream must see the upcast shape and
// LoadStreamRaw must still see the original, stored shape.
func TestLoadStreamRawBypassesUpcaster(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if _, err := s.Append(ctx, "note", "n1", 0, []NewEvent{
		{Type: "NoteCreated", Data: json.RawMessage(`{"text":"old"}`), Version: 1},
	}); err != nil {
		t.Fatal(err)
	}

	s.SetUpcaster(func(ev Event) (Event, error) {
		if ev.Type == "NoteCreated" && ev.Version == 1 {
			ev.Data = json.RawMessage(`{"text":"old","priority":0}`)
			ev.Version = 2
		}
		return ev, nil
	})

	upcast, err := s.LoadStream(ctx, "note", "n1")
	if err != nil {
		t.Fatal(err)
	}
	if upcast[0].Version != 2 {
		t.Fatalf("LoadStream: expected upcast version 2, got %+v", upcast[0])
	}

	raw, err := s.LoadStreamRaw(ctx, "note", "n1")
	if err != nil {
		t.Fatal(err)
	}
	if raw[0].Version != 1 || string(raw[0].Data) != `{"text":"old"}` {
		t.Fatalf("LoadStreamRaw: expected untouched stored row, got %+v", raw[0])
	}

	// a FAILING upcaster must not affect LoadStreamRaw at all — it never
	// calls the installed upcaster in the first place.
	s.SetUpcaster(func(ev Event) (Event, error) {
		return ev, errors.New("boom")
	})
	if _, err := s.LoadStreamRaw(ctx, "note", "n1"); err != nil {
		t.Fatalf("LoadStreamRaw must not go through the upcaster: %v", err)
	}
}

func TestImportEventsOnCleanTarget(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// a pre-existing, unrelated stream that must be left untouched
	if _, err := s.Append(ctx, "order", "o1", 0, []NewEvent{
		{Type: "OrderPlaced", Data: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}

	batch := []Event{
		{ID: "imported-1", Aggregate: "task", AggregateID: "t1", Sequence: 1,
			Type: "TaskCreated", Data: json.RawMessage(`{"title":"a"}`), Version: 1, Created: "2020-01-01 00:00:00.000Z"},
		{ID: "imported-2", Aggregate: "task", AggregateID: "t1", Sequence: 2,
			Type: "TaskCompleted", Data: json.RawMessage(`{}`), Version: 1, Created: "2020-01-01 00:00:01.000Z"},
	}
	imported, err := s.ImportEvents(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(imported) != 2 {
		t.Fatalf("expected 2 imported events, got %d", len(imported))
	}
	if imported[0].ID != "imported-1" || imported[0].Sequence != 1 || imported[0].Created != "2020-01-01 00:00:00.000Z" {
		t.Fatalf("id/sequence/created not preserved: %+v", imported[0])
	}
	if imported[0].Position == 0 || imported[1].Position <= imported[0].Position {
		t.Fatalf("expected fresh, increasing positions, got %+v / %+v", imported[0], imported[1])
	}

	stream, err := s.LoadStreamRaw(ctx, "task", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stream) != 2 {
		t.Fatalf("expected 2 events in the imported stream, got %d", len(stream))
	}

	// the pre-existing, unrelated stream is untouched
	orders, err := s.LoadStreamRaw(ctx, "order", "o1")
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != 1 {
		t.Fatalf("expected the pre-existing order stream untouched, got %d events", len(orders))
	}
}

func TestImportEventsRefusesOnStreamKeyCollision(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if _, err := s.Append(ctx, "task", "t1", 0, []NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}

	// a batch with one colliding stream (task/t1) and one clean one
	// (widget/w1, an aggregate name the target has never seen) — the
	// refusal must be all-or-nothing: the clean stream must NOT be
	// partially inserted, and the error must name the collision.
	batch := []Event{
		{ID: "x1", Aggregate: "task", AggregateID: "t1", Sequence: 1,
			Type: "TaskRenamed", Data: json.RawMessage(`{}`), Version: 1, Created: "2020-01-01 00:00:00.000Z"},
		{ID: "x2", Aggregate: "widget", AggregateID: "w1", Sequence: 1,
			Type: "WidgetCreated", Data: json.RawMessage(`{}`), Version: 1, Created: "2020-01-01 00:00:00.000Z"},
	}
	_, err := s.ImportEvents(ctx, batch)
	if err == nil {
		t.Fatal("expected a collision refusal")
	}
	if !strings.Contains(err.Error(), "task/t1") {
		t.Fatalf("expected the error to name the colliding stream, got: %v", err)
	}

	widgets, lerr := s.LoadStreamRaw(ctx, "widget", "w1")
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(widgets) != 0 {
		t.Fatalf("expected the clean stream NOT partially inserted, got %d events", len(widgets))
	}
}

func TestImportEventsRefusesOnAggregateNameCollision(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// target already has a "task" stream under a DIFFERENT id than the
	// one being imported — the name-level check must still refuse.
	if _, err := s.Append(ctx, "task", "t1", 0, []NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}

	batch := []Event{
		{ID: "y1", Aggregate: "task", AggregateID: "t2", Sequence: 1,
			Type: "TaskCreated", Data: json.RawMessage(`{}`), Version: 1, Created: "2020-01-01 00:00:00.000Z"},
	}
	_, err := s.ImportEvents(ctx, batch)
	if err == nil {
		t.Fatal("expected an aggregate-name-level collision refusal even though the id (t2) differs")
	}
	if !strings.Contains(err.Error(), `"task"`) {
		t.Fatalf("expected the error to name the colliding aggregate, got: %v", err)
	}
}

func TestImportEventsOnReadOnlyStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	writer.Close()

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ro.Close() })

	batch := []Event{
		{ID: "z1", Aggregate: "task", AggregateID: "t1", Sequence: 1,
			Type: "TaskCreated", Data: json.RawMessage(`{}`), Version: 1, Created: "2020-01-01 00:00:00.000Z"},
	}
	if _, err := ro.ImportEvents(context.Background(), batch); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("expected ErrReadOnly, got %v", err)
	}
}

// TestImportEventsIDCollisionRollsBackAtomically is the practically-never
// case (80 bits of randomness, newID()) where two independently exported
// logs share an event id — the store's own UNIQUE(id) constraint must
// still fail the whole transaction, not partially write.
func TestImportEventsIDCollisionRollsBackAtomically(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	appended, err := s.Append(ctx, "task", "t1", 0, []NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	existingID := appended[0].ID

	// a distinct, non-colliding stream key (so the pre-write collision
	// checks pass) whose id nonetheless reuses an existing one
	batch := []Event{
		{ID: existingID, Aggregate: "widget", AggregateID: "w1", Sequence: 1,
			Type: "WidgetCreated", Data: json.RawMessage(`{}`), Version: 1, Created: "2020-01-01 00:00:00.000Z"},
	}
	if _, err := s.ImportEvents(ctx, batch); err == nil {
		t.Fatal("expected the UNIQUE(id) constraint to fail the import")
	}

	widgets, err := s.LoadStreamRaw(ctx, "widget", "w1")
	if err != nil {
		t.Fatal(err)
	}
	if len(widgets) != 0 {
		t.Fatalf("expected nothing written after the id-collision rollback, got %+v", widgets)
	}
}
