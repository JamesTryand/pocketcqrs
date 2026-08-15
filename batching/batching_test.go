package batching

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/commandqueue"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

// taskState/taskDecider is a minimal decider with a real domain rule
// (create-once, complete-once) so tests can exercise genuine rejections
// and genuine same-stream successions, not just "always succeeds".
type taskState struct{ Exists, Completed bool }

func taskDecider() *decider.Decider[taskState] {
	return &decider.Decider[taskState]{
		InitialState: func() taskState { return taskState{} },
		Decide: func(cmd decider.Command, s taskState) ([]events.NewEvent, error) {
			switch cmd.Name {
			case "Create":
				if s.Exists {
					return nil, fmt.Errorf("task already exists")
				}
				return []events.NewEvent{{Type: "TaskCreated", Data: json.RawMessage(`{}`)}}, nil
			case "Complete":
				if !s.Exists {
					return nil, fmt.Errorf("task does not exist")
				}
				if s.Completed {
					return nil, nil
				}
				return []events.NewEvent{{Type: "TaskCompleted", Data: json.RawMessage(`{}`)}}, nil
			}
			return nil, fmt.Errorf("unknown command %q", cmd.Name)
		},
		Evolve: func(s taskState, ev events.Event) (taskState, error) {
			switch ev.Type {
			case "TaskCreated":
				s.Exists = true
			case "TaskCompleted":
				s.Completed = true
			}
			return s, nil
		},
	}
}

func setup(t *testing.T) (*events.Store, *commandqueue.Store, *decider.Registry, *Writer) {
	t.Helper()
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	queue, err := commandqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { queue.Close() })

	registry := decider.NewRegistry(store)
	decider.Register(registry, "task", taskDecider())

	w := NewWriter(store, queue, registry, nil)
	return store, queue, registry, w
}

func metaNow() map[string]any {
	return map[string]any{"now": time.Now().UTC().Format("2006-01-02 15:04:05.000Z")}
}

func TestRunOnceDecidesAndCommitsSingleCommand(t *testing.T) {
	_, _, _, w := setup(t)
	ctx := context.Background()

	_, wait, err := w.Enqueue(ctx, "task", "t1", "Create", json.RawMessage(`{}`), metaNow())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	select {
	case outcome := <-wait:
		if outcome.Err != nil {
			t.Fatal(outcome.Err)
		}
		if len(outcome.Events) != 1 || outcome.Events[0].Type != "TaskCreated" {
			t.Fatalf("unexpected outcome: %+v", outcome)
		}
	default:
		t.Fatal("expected the waiter to be signaled after RunOnce")
	}
}

func TestRunOnceHandlesSameStreamSuccessionInOneWindow(t *testing.T) {
	_, _, _, w := setup(t)
	ctx := context.Background()

	_, waitCreate, err := w.Enqueue(ctx, "task", "t1", "Create", json.RawMessage(`{}`), metaNow())
	if err != nil {
		t.Fatal(err)
	}
	_, waitComplete, err := w.Enqueue(ctx, "task", "t1", "Complete", json.RawMessage(`{}`), metaNow())
	if err != nil {
		t.Fatal(err)
	}

	processed, err := w.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if processed != 2 {
		t.Fatalf("expected 2 processed, got %d", processed)
	}

	create := <-waitCreate
	complete := <-waitComplete
	if create.Err != nil || complete.Err != nil {
		t.Fatalf("unexpected errors: create=%v complete=%v", create.Err, complete.Err)
	}
	if create.Events[0].Sequence != 1 {
		t.Fatalf("expected Create at sequence 1, got %d", create.Events[0].Sequence)
	}
	if complete.Events[0].Sequence != 2 {
		t.Fatalf("expected Complete at sequence 2 (decided against Create's not-yet-committed effect), got %d",
			complete.Events[0].Sequence)
	}
}

func TestRunOnceDomainRejectionSignalsErrorAndMarksDone(t *testing.T) {
	_, queue, _, w := setup(t)
	ctx := context.Background()

	// Complete before Create -- a domain rejection, no side effect
	id, wait, err := w.Enqueue(ctx, "task", "t1", "Complete", json.RawMessage(`{}`), metaNow())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	outcome := <-wait
	if outcome.Err == nil {
		t.Fatal("expected a domain rejection error")
	}

	pending, err := queue.PendingCommands(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pending {
		if p.ID == id {
			t.Fatal("expected the rejected command to be marked done, not left pending forever")
		}
	}
}

func TestEnqueueRequiresNowInMeta(t *testing.T) {
	_, _, _, w := setup(t)
	_, _, err := w.Enqueue(context.Background(), "task", "t1", "Create", json.RawMessage(`{}`), map[string]any{})
	if err == nil {
		t.Fatal("expected Enqueue to refuse meta without \"now\"")
	}
}

// TestDeterminismSameQueuedMetaReplayedProducesIdenticalNow is the
// determinism guarantee the whole crash-safety argument depends on: a
// command decided again using the SAME meta that was actually persisted at
// enqueue time (not a freshly-generated one) must produce the same "now",
// proving Enqueue's captured meta -- not DecideWithMeta's own fallback --
// is what a replay would see.
func TestDeterminismSameQueuedMetaReplayedProducesIdenticalNow(t *testing.T) {
	store, queue, registry, w := setup(t)
	ctx := context.Background()

	capturedMeta := metaNow()
	_, wait, err := w.Enqueue(ctx, "task", "t1", "Create", json.RawMessage(`{}`), capturedMeta)
	if err != nil {
		t.Fatal(err)
	}

	// read the row back exactly as a restarted process would -- this
	// proves what was actually PERSISTED, not just the in-memory map
	pendingBefore, err := queue.PendingCommands(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pendingBefore) != 1 {
		t.Fatalf("expected 1 pending command, got %d", len(pendingBefore))
	}
	persistedMetaJSON := pendingBefore[0].Meta

	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	original := <-wait
	if original.Err != nil {
		t.Fatal(original.Err)
	}
	var origMeta map[string]any
	if err := json.Unmarshal(original.Events[0].Metadata, &origMeta); err != nil {
		t.Fatal(err)
	}

	// simulate a replay on a DIFFERENT stream (so it can't collide with
	// the already-committed one) using ONLY the persisted meta bytes --
	// exactly what a resumed process reads back after a crash, with
	// nothing held over in memory
	var replayMeta map[string]any
	if err := json.Unmarshal(persistedMetaJSON, &replayMeta); err != nil {
		t.Fatal(err)
	}
	replayEvents, _, err := registry.DecideWithMeta(ctx, store, "task", "t1-replay",
		decider.Command{Name: "Create"}, replayMeta)
	if err != nil {
		t.Fatal(err)
	}
	var replayedMeta map[string]any
	if err := json.Unmarshal(replayEvents[0].Metadata, &replayedMeta); err != nil {
		t.Fatal(err)
	}

	if origMeta["now"] != replayedMeta["now"] {
		t.Fatalf("replay produced a different \"now\": original=%v replay=%v -- Enqueue's captured meta is not being honored",
			origMeta["now"], replayedMeta["now"])
	}
}

// TestRunOnceMatchesSequentialHandleWithMeta cross-checks the batched path
// against plain sequential HandleWithMeta calls over the same commands
// (including a same-stream succession within the batch window) -- the
// batching mechanism must be transparent, producing the same shape of
// outcome a caller would have gotten before item 4 existed.
func TestRunOnceMatchesSequentialHandleWithMeta(t *testing.T) {
	refStore, err := events.Open(filepath.Join(t.TempDir(), "ref.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer refStore.Close()
	refRegistry := decider.NewRegistry(refStore)
	decider.Register(refRegistry, "task", taskDecider())

	type step struct{ aggregateID, command string }
	steps := []step{
		{"t1", "Create"},
		{"t2", "Create"},
		{"t1", "Complete"}, // second command for t1 in the same window
		{"t3", "Create"},
	}

	ctx := context.Background()
	refEvents := make([][]events.Event, len(steps))
	for i, st := range steps {
		ev, err := refRegistry.HandleWithMeta(ctx, "task", st.aggregateID, decider.Command{Name: st.command}, nil)
		if err != nil {
			t.Fatal(err)
		}
		refEvents[i] = ev
	}

	_, _, _, w := setup(t)
	waits := make([]<-chan Outcome, len(steps))
	for i, st := range steps {
		_, wait, err := w.Enqueue(ctx, "task", st.aggregateID, st.command, json.RawMessage(`{}`), metaNow())
		if err != nil {
			t.Fatal(err)
		}
		waits[i] = wait
	}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	for i, wait := range waits {
		outcome := <-wait
		if outcome.Err != nil {
			t.Fatalf("step %d: unexpected error %v", i, outcome.Err)
		}
		if len(outcome.Events) != len(refEvents[i]) {
			t.Fatalf("step %d: expected %d events, got %d", i, len(refEvents[i]), len(outcome.Events))
		}
		for j := range outcome.Events {
			got, want := outcome.Events[j], refEvents[i][j]
			if got.Type != want.Type || got.Sequence != want.Sequence || got.AggregateID != want.AggregateID {
				t.Fatalf("step %d event %d: got {%s %s seq=%d}, want {%s %s seq=%d}",
					i, j, got.AggregateID, got.Type, got.Sequence, want.AggregateID, want.Type, want.Sequence)
			}
		}
	}
}

func TestRunOnceEmptyQueueIsNoop(t *testing.T) {
	_, _, _, w := setup(t)
	processed, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if processed != 0 {
		t.Fatalf("expected 0 processed on an empty queue, got %d", processed)
	}
}

func TestRunOnceResumesAlreadyAppliedCommandWithoutRedeciding(t *testing.T) {
	store, queue, registry, w := setup(t)
	ctx := context.Background()

	// simulate the post-commit-pre-mark crash window directly: commit a
	// command's events via CommitBatch (stamping commandId) as RunOnce
	// itself would, but WITHOUT going through Enqueue/MarkDone -- leaving
	// a queue row that looks "pending" even though its events are already
	// durable, exactly what a crash between commit and mark-done leaves
	// behind.
	head, err := store.MaxPosition(ctx)
	if err != nil {
		t.Fatal(err)
	}
	qc, err := queue.EnqueueCommand(ctx, "task", "t1", "Create", json.RawMessage(`{}`),
		mustJSON(t, metaNow()), head)
	if err != nil {
		t.Fatal(err)
	}
	newEvents, expectedSeq, err := registry.DecideWithMeta(ctx, store, "task", "t1",
		decider.Command{Name: "Create"}, mustUnmarshal(t, qc.Meta))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitBatch(ctx, []events.DecidedCommand{
		{CommandID: qc.ID, Aggregate: "task", AggregateID: "t1", ExpectedSequence: expectedSeq, NewEvents: newEvents},
	}); err != nil {
		t.Fatal(err)
	}
	// deliberately do NOT call queue.MarkDone -- qc.ID is still "pending"

	processed, err := w.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if processed != 1 {
		t.Fatalf("expected the resumed command to be processed (cleaned up), got %d", processed)
	}

	// it must NOT have been redecided -- only 1 TaskCreated exists, not 2
	stream, err := store.LoadStream(ctx, "task", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stream) != 1 {
		t.Fatalf("expected exactly 1 event (no double-apply on resume), got %d", len(stream))
	}

	pending, err := queue.PendingCommands(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected the resumed command to be marked done, got %d still pending", len(pending))
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustUnmarshal(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
