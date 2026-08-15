package commandqueue

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "commandqueue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestEnqueueCommandAssignsIDAndPosition(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	qc, err := s.EnqueueCommand(ctx, "task", "t1", "Create", json.RawMessage(`{"title":"x"}`), json.RawMessage(`{"now":"2026-01-01"}`), 5)
	if err != nil {
		t.Fatal(err)
	}
	if qc.ID == "" {
		t.Fatal("expected a non-empty id")
	}
	if qc.Position <= 0 {
		t.Fatalf("expected a positive position, got %d", qc.Position)
	}
	if qc.Aggregate != "task" || qc.AggregateID != "t1" || qc.Command != "Create" {
		t.Fatalf("unexpected row: %+v", qc)
	}
	if qc.EnqueuedAtPosition != 5 {
		t.Fatalf("expected enqueuedAtPosition 5, got %d", qc.EnqueuedAtPosition)
	}
	if qc.Enqueued == "" {
		t.Fatal("expected a non-empty enqueued timestamp")
	}
}

func TestEnqueueCommandDefaultsEmptyPayloadAndMeta(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	qc, err := s.EnqueueCommand(ctx, "task", "t1", "Create", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(qc.Payload) != "{}" {
		t.Fatalf("expected empty payload to default to {}, got %q", qc.Payload)
	}
	if string(qc.Meta) != "{}" {
		t.Fatalf("expected empty meta to default to {}, got %q", qc.Meta)
	}
}

func TestPendingCommandsOrderedOldestFirstAndExcludesDone(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	a, err := s.EnqueueCommand(ctx, "task", "t1", "Create", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.EnqueueCommand(ctx, "task", "t2", "Create", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.EnqueueCommand(ctx, "task", "t3", "Create", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	pending, err := s.PendingCommands(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("expected 3 pending, got %d", len(pending))
	}
	if pending[0].ID != a.ID || pending[1].ID != b.ID || pending[2].ID != c.ID {
		t.Fatalf("expected oldest-first order a,b,c, got %s,%s,%s", pending[0].ID, pending[1].ID, pending[2].ID)
	}

	if err := s.MarkDone(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	pending, err = s.PendingCommands(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("expected 2 pending after marking one done, got %d", len(pending))
	}
	for _, qc := range pending {
		if qc.ID == b.ID {
			t.Fatal("marked-done command must not appear in PendingCommands")
		}
	}
}

func TestPendingCommandsRespectsLimit(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := s.EnqueueCommand(ctx, "task", "t1", "Create", nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := s.PendingCommands(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("expected limit of 2, got %d", len(pending))
	}
}

func TestMarkDoneIsIdempotentAndAcceptsUnknownIDs(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	qc, err := s.EnqueueCommand(ctx, "task", "t1", "Create", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDone(ctx, qc.ID); err != nil {
		t.Fatal(err)
	}
	// repeated, and alongside an id that was never enqueued -- both must be no-ops, not errors
	if err := s.MarkDone(ctx, qc.ID, "does-not-exist"); err != nil {
		t.Fatal(err)
	}
}

func TestMarkDoneEmptyIsNoop(t *testing.T) {
	s := openTest(t)
	if err := s.MarkDone(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// BenchmarkEnqueueCommand answers, with a number rather than a guess,
// whether the enqueue step itself needs batching too (Stage 0's own
// question -- see the plan). Run with: go test ./commandqueue/... -bench=. -benchtime=3s
func BenchmarkEnqueueCommand(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "commandqueue.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	payload := json.RawMessage(`{"title":"x"}`)
	meta := json.RawMessage(`{"now":"2026-01-01T00:00:00.000Z","actor":"bench"}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.EnqueueCommand(ctx, "task", "t1", "Create", payload, meta, 0); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkEnqueueCommandParallel is the same write load offered
// concurrently, to see whether goroutine contention on top of SQLite's own
// single-writer limit adds meaningful overhead beyond the sequential floor.
func BenchmarkEnqueueCommandParallel(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "commandqueue.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	payload := json.RawMessage(`{"title":"x"}`)
	meta := json.RawMessage(`{"now":"2026-01-01T00:00:00.000Z","actor":"bench"}`)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		ctx := context.Background()
		for pb.Next() {
			if _, err := s.EnqueueCommand(ctx, "task", "t1", "Create", payload, meta, 0); err != nil {
				b.Fatal(err)
			}
		}
	})
}
