package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
)

func TestDeadLettersLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	ev := Event{Position: 7, ID: "e1", Aggregate: "task", AggregateID: "t1", Sequence: 1,
		Type: "TaskCreated", Data: json.RawMessage(`{"title":"x"}`)}

	if err := s.AddDeadLetter(ctx, "fn:poison.js", ev, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDeadLetter(ctx, "fn:other.js", ev, errors.New("bang")); err != nil {
		t.Fatal(err)
	}

	letters, err := s.DeadLetters(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(letters) != 2 {
		t.Fatalf("expected 2 pending, got %d", len(letters))
	}
	dl := letters[0]
	if dl.Consumer != "fn:poison.js" || dl.Attempts != 1 || dl.Resolved || dl.Error != "boom" {
		t.Fatalf("unexpected dead letter: %+v", dl)
	}
	if dl.Event.ID != "e1" || dl.Event.Aggregate != "task" {
		t.Fatalf("envelope not preserved: %+v", dl.Event)
	}

	// failed retry increments attempts and updates the error
	if err := s.FailDeadLetterRetry(ctx, dl.ID, errors.New("still broken")); err != nil {
		t.Fatal(err)
	}
	letters, _ = s.DeadLetters(ctx, false)
	if letters[0].Attempts != 2 || letters[0].Error != "still broken" {
		t.Fatalf("retry failure not recorded: %+v", letters[0])
	}

	// resolve hides from pending but keeps the record
	if err := s.ResolveDeadLetter(ctx, dl.ID); err != nil {
		t.Fatal(err)
	}
	letters, _ = s.DeadLetters(ctx, false)
	if len(letters) != 1 {
		t.Fatalf("expected 1 pending after resolve, got %d", len(letters))
	}
	all, _ := s.DeadLetters(ctx, true)
	if len(all) != 2 || !all[0].Resolved {
		t.Fatalf("expected resolved record kept: %+v", all)
	}

	// resolving an unknown id errors
	if err := s.ResolveDeadLetter(ctx, 999); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected no-rows error, got %v", err)
	}
}
