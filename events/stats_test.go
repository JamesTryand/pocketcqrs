package events

import (
	"context"
	"encoding/json"
	"testing"
)

func TestStats(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// two task streams, one note stream; a reactor-produced task event
	if _, err := s.Append(ctx, "task", "t1", 0, []NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{"title":"a"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, "note", "n1", 0, []NewEvent{
		{Type: "NoteCreated", Data: json.RawMessage(`{"text":"x"}`), Version: 1},
		{Type: "NoteTextChanged", Data: json.RawMessage(`{"text":"y"}`), Version: 2},
	}); err != nil {
		t.Fatal(err)
	}
	cause := []NewEvent{{Type: "OrderConfirmed", Data: json.RawMessage(`{}`)}}
	caused, err := s.Append(ctx, "order", "o1", 0, cause)
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]string{
		"actor": "reactor:fulfillment", "causationId": caused[0].ID, "correlationId": caused[0].ID,
	})
	if _, err := s.Append(ctx, "task", "fulfill-o1", 0, []NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{"title":"fulfil order o1"}`), Metadata: meta},
	}); err != nil {
		t.Fatal(err)
	}

	stats, err := s.EventTypeStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// note/NoteCreated, note/NoteTextChanged, order/OrderConfirmed, task/TaskCreated
	if len(stats) != 4 {
		t.Fatalf("expected 4 stat rows, got %+v", stats)
	}
	var noteText, taskCreatedRow *EventTypeStat
	for i := range stats {
		if stats[i].Type == "NoteTextChanged" {
			noteText = &stats[i]
		}
		if stats[i].Aggregate == "task" && stats[i].Type == "TaskCreated" {
			taskCreatedRow = &stats[i]
		}
	}
	if noteText == nil || noteText.Count != 1 || noteText.MinVersion != 2 || noteText.MaxVersion != 2 {
		t.Fatalf("unexpected NoteTextChanged stat: %+v", noteText)
	}
	// TaskCreated aggregates across both task streams (t1 + fulfill-o1)
	if taskCreatedRow == nil || taskCreatedRow.Count != 2 {
		t.Fatalf("TaskCreated should aggregate both streams: %+v", stats)
	}

	events, streams, err := s.LogTotals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if events != 5 || streams != 4 {
		t.Fatalf("unexpected totals: events=%d streams=%d", events, streams)
	}

	sc, err := s.StreamCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sc["task"] != 2 || sc["note"] != 1 || sc["order"] != 1 {
		t.Fatalf("unexpected stream counts: %v", sc)
	}

	// checkpoints
	if err := s.SaveCheckpoint(ctx, "tasks", 5); err != nil {
		t.Fatal(err)
	}
	cps, err := s.Checkpoints(ctx)
	if err != nil || cps["tasks"] != 5 {
		t.Fatalf("unexpected checkpoints: %v (err=%v)", cps, err)
	}

	// reactor flow: fulfill-o1's TaskCreated was caused by o1's OrderConfirmed
	flows, err := s.ReactorFlows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 1 {
		t.Fatalf("expected 1 flow, got %+v", flows)
	}
	f := flows[0]
	if f.Reactor != "reactor:fulfillment" || f.CauseAggregate != "order" || f.CauseType != "OrderConfirmed" ||
		f.TargetAggregate != "task" || f.TargetType != "TaskCreated" || f.Count != 1 {
		t.Fatalf("unexpected flow: %+v", f)
	}
}
