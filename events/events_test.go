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
