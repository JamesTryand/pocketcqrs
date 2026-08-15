package events

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCommandAppliedFindsStampedEvent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	results, err := s.CommitBatch(ctx, []DecidedCommand{
		{CommandID: "cmd-1", Aggregate: "task", AggregateID: "t1", ExpectedSequence: 0,
			NewEvents: []NewEvent{{Type: "TaskCreated", Data: json.RawMessage(`{}`)}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = results

	applied, err := s.CommandApplied(ctx, "task", "t1", "cmd-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("expected CommandApplied to find the stamped event")
	}
}

func TestCommandAppliedFalseWhenNeverApplied(t *testing.T) {
	s := openTest(t)
	applied, err := s.CommandApplied(context.Background(), "task", "t1", "no-such-command", 0)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("expected false for a command that never ran")
	}
}

func TestCommandAppliedFalseForDifferentStream(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if _, err := s.CommitBatch(ctx, []DecidedCommand{
		{CommandID: "cmd-1", Aggregate: "task", AggregateID: "t1", ExpectedSequence: 0,
			NewEvents: []NewEvent{{Type: "TaskCreated", Data: json.RawMessage(`{}`)}}},
	}); err != nil {
		t.Fatal(err)
	}

	// same commandId, wrong stream -- must not match
	applied, err := s.CommandApplied(ctx, "task", "t2", "cmd-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("expected false: the stamped event belongs to a different stream")
	}
}

func TestCommandAppliedRespectsPositionFloor(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	results, err := s.CommitBatch(ctx, []DecidedCommand{
		{CommandID: "cmd-1", Aggregate: "task", AggregateID: "t1", ExpectedSequence: 0,
			NewEvents: []NewEvent{{Type: "TaskCreated", Data: json.RawMessage(`{}`)}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pos := results[0].Events[0].Position

	// asking "after my own position" (or later) must exclude the event
	// even though its commandId matches -- proves the floor is applied,
	// not decorative
	applied, err := s.CommandApplied(ctx, "task", "t1", "cmd-1", pos)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("expected false: the event is not strictly after the given position")
	}

	// but asking from before it is found correctly
	applied, err = s.CommandApplied(ctx, "task", "t1", "cmd-1", pos-1)
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("expected true: the event is after pos-1")
	}
}

// TestCommandAppliedQueryPlanUsesTheIndex is a smoke check that
// idx_events_command_id is actually consulted, not just present -- a wrong
// key name or shape on either the write or read side would still compile
// and pass the tests above on a small table, but silently fall back to a
// full scan at real scale.
func TestCommandAppliedQueryPlanUsesTheIndex(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.CommitBatch(ctx, []DecidedCommand{
		{CommandID: "cmd-1", Aggregate: "task", AggregateID: "t1", ExpectedSequence: 0,
			NewEvents: []NewEvent{{Type: "TaskCreated", Data: json.RawMessage(`{}`)}}},
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.db.QueryContext(ctx,
		`EXPLAIN QUERY PLAN SELECT 1 FROM events
		 WHERE aggregate = ? AND aggregate_id = ? AND position > ?
		   AND json_extract(metadata, '$.commandId') = ? LIMIT 1`,
		"task", "t1", int64(0), "cmd-1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.String(), "idx_events_command_id") {
		t.Fatalf("expected the query plan to use idx_events_command_id, got:\n%s", plan.String())
	}
}
