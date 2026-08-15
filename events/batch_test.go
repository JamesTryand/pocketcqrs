package events

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestCommitBatchAllSuccessDifferentStreams(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	batch := []DecidedCommand{
		{CommandID: "cmd-1", Aggregate: "task", AggregateID: "t1", ExpectedSequence: 0,
			NewEvents: []NewEvent{{Type: "TaskCreated", Data: json.RawMessage(`{}`)}}},
		{CommandID: "cmd-2", Aggregate: "task", AggregateID: "t2", ExpectedSequence: 0,
			NewEvents: []NewEvent{{Type: "TaskCreated", Data: json.RawMessage(`{}`)}}},
	}
	results, err := s.CommitBatch(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for i, r := range results {
		if r.Err != nil {
			t.Fatalf("result %d: unexpected error %v", i, r.Err)
		}
		if len(r.Events) != 1 || r.Events[0].Sequence != 1 {
			t.Fatalf("result %d: expected 1 event at sequence 1, got %+v", i, r.Events)
		}
	}

	stream1, err := s.LoadStream(ctx, "task", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stream1) != 1 {
		t.Fatalf("expected t1 to have 1 event, got %d", len(stream1))
	}
}

func TestCommitBatchSameStreamSuccession(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// two commands for the SAME stream in one batch -- staggered
	// ExpectedSequence, exactly what the writer's overlay would produce by
	// deciding the second against the first's not-yet-committed effect
	batch := []DecidedCommand{
		{CommandID: "cmd-1", Aggregate: "task", AggregateID: "t1", ExpectedSequence: 0,
			NewEvents: []NewEvent{{Type: "TaskCreated", Data: json.RawMessage(`{}`)}}},
		{CommandID: "cmd-2", Aggregate: "task", AggregateID: "t1", ExpectedSequence: 1,
			NewEvents: []NewEvent{{Type: "TaskCompleted", Data: json.RawMessage(`{}`)}}},
	}
	results, err := s.CommitBatch(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Err != nil || results[1].Err != nil {
		t.Fatalf("unexpected errors: %v, %v", results[0].Err, results[1].Err)
	}
	if results[0].Events[0].Sequence != 1 || results[1].Events[0].Sequence != 2 {
		t.Fatalf("expected sequences 1 then 2, got %d then %d",
			results[0].Events[0].Sequence, results[1].Events[0].Sequence)
	}

	stream, err := s.LoadStream(ctx, "task", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stream) != 2 || stream[0].Type != "TaskCreated" || stream[1].Type != "TaskCompleted" {
		t.Fatalf("unexpected stream: %+v", stream)
	}
}

func TestCommitBatchConflictRollsBackEntireBatch(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// simulate a concurrent direct Append (e.g. a reactor bypassing the
	// queue) landing on t1 between when a queued command was decided
	// (against an empty stream, ExpectedSequence 0) and when its batch
	// tries to commit
	if _, err := s.Append(ctx, "task", "t1", 0, []NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}

	batch := []DecidedCommand{
		// stale: decided when t1 was still empty
		{CommandID: "cmd-1", Aggregate: "task", AggregateID: "t1", ExpectedSequence: 0,
			NewEvents: []NewEvent{{Type: "TaskCompleted", Data: json.RawMessage(`{}`)}}},
		// otherwise perfectly valid, different stream
		{CommandID: "cmd-2", Aggregate: "task", AggregateID: "t2", ExpectedSequence: 0,
			NewEvents: []NewEvent{{Type: "TaskCreated", Data: json.RawMessage(`{}`)}}},
	}
	results, err := s.CommitBatch(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(results[0].Err, ErrConcurrency) {
		t.Fatalf("expected ErrConcurrency for the conflicting command, got %v", results[0].Err)
	}
	// the non-conflicting batch-mate must NOT look like "committed with
	// zero events" (Events nil, Err nil) -- it must carry ErrBatchAborted,
	// or a caller can't tell the two apart
	if !errors.Is(results[1].Err, ErrBatchAborted) {
		t.Fatalf("expected ErrBatchAborted for the non-conflicting batch-mate, got %v", results[1].Err)
	}

	// the WHOLE transaction rolled back -- t2's otherwise-valid command was
	// not applied either
	stream2, err := s.LoadStream(ctx, "task", "t2")
	if err != nil {
		t.Fatal(err)
	}
	if len(stream2) != 0 {
		t.Fatalf("expected t2 untouched by the rolled-back batch, found %d events", len(stream2))
	}
	// and t1 still only has the direct Append's one event, not a second
	// TaskCompleted from the conflicting command
	stream1, err := s.LoadStream(ctx, "task", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stream1) != 1 {
		t.Fatalf("expected t1 to still have exactly 1 event, got %d", len(stream1))
	}
}

func TestCommitBatchZeroEventCommandIsNoopNotConflict(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	batch := []DecidedCommand{
		{CommandID: "cmd-1", Aggregate: "task", AggregateID: "t1", ExpectedSequence: 0, NewEvents: nil},
		{CommandID: "cmd-2", Aggregate: "task", AggregateID: "t2", ExpectedSequence: 0,
			NewEvents: []NewEvent{{Type: "TaskCreated", Data: json.RawMessage(`{}`)}}},
	}
	results, err := s.CommitBatch(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Err != nil || len(results[0].Events) != 0 {
		t.Fatalf("expected a harmless no-op result for the zero-event command, got %+v", results[0])
	}
	if results[1].Err != nil || len(results[1].Events) != 1 {
		t.Fatalf("expected the other command in the batch to still succeed, got %+v", results[1])
	}
}

func TestCommitBatchStampsCommandIDPreservingExistingMetadata(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	batch := []DecidedCommand{
		{CommandID: "cmd-1", Aggregate: "task", AggregateID: "t1", ExpectedSequence: 0,
			NewEvents: []NewEvent{{Type: "TaskCreated", Data: json.RawMessage(`{}`),
				Metadata: json.RawMessage(`{"actor":"user123"}`)}}},
	}
	results, err := s.CommitBatch(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(results[0].Events[0].Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["commandId"] != "cmd-1" {
		t.Fatalf("expected commandId stamped, got %v", meta)
	}
	if meta["actor"] != "user123" {
		t.Fatalf("expected existing metadata preserved, got %v", meta)
	}
}

func TestCommitBatchEmptyIsNoop(t *testing.T) {
	s := openTest(t)
	results, err := s.CommitBatch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if results != nil {
		t.Fatalf("expected nil results for an empty batch, got %v", results)
	}
}
