package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
