package batching

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"github.com/jamestryand/pocketcqrs/events"
)

// errSimulatedCrash stands in for a real process death: events.Store's
// CommitBatchFault hook fires at the exact point a real crash between
// validating a batch and durably writing it would land -- immediately
// before tx.Commit() -- so this exercises the real code path, not a mock
// of it.
var errSimulatedCrash = errors.New("chaos: simulated crash before commit")

// TestChaosPreCommitCrashReplayIsDeterministic is the fast, deterministic
// half of Stage 5's fault-injection requirement: hundreds of randomized
// iterations, every normal `go test` run, no real process involved. It
// exercises exactly the two crash windows the whole design depends on
// closing (see the package doc comment):
//
//   - roughly half the iterations simulate a crash before the batch
//     commits -- nothing must be written, and a subsequent real RunOnce
//     must reproduce byte-identical events, in the same order, as if the
//     crash never happened (determinism);
//   - every iteration (crashed or not) ends by resuming: a fresh RunOnce
//     pass must never re-decide a command whose events are already
//     durable, whether or not a crash happened on the way there.
//
// Batches deliberately include a same-aggregate collision (two commands
// for the same stream in one window) on every iteration, since that is the
// path most likely to break under either crash handling or the overlay.
func TestChaosPreCommitCrashReplayIsDeterministic(t *testing.T) {
	const iterations = 300
	rng := rand.New(rand.NewSource(1)) // fixed seed: failures must reproduce

	for iter := 0; iter < iterations; iter++ {
		iter := iter
		t.Run(fmt.Sprintf("iter-%d", iter), func(t *testing.T) {
			store, _, _, w := setup(t)
			ctx := context.Background()

			type step struct{ agg, id, cmd string }
			// task/t1 gets two commands in one window (Create then
			// Complete) -- the same-stream collision case -- alongside two
			// independent single-command streams.
			steps := []step{
				{"task", "t1", "Create"},
				{"task", "t2", "Create"},
				{"task", "t1", "Complete"},
				{"task", "t3", "Create"},
			}

			waits := make([]<-chan Outcome, len(steps))
			for i, st := range steps {
				_, wait, err := w.Enqueue(ctx, st.agg, st.id, st.cmd, json.RawMessage(`{}`), metaNow())
				if err != nil {
					t.Fatal(err)
				}
				waits[i] = wait
			}

			simulateCrash := rng.Intn(2) == 0
			if simulateCrash {
				store.CommitBatchFault = func() error { return errSimulatedCrash }
			}

			_, runErr := w.RunOnce(ctx)

			if simulateCrash {
				if !errors.Is(runErr, errSimulatedCrash) {
					t.Fatalf("expected the simulated crash to surface, got %v", runErr)
				}
				store.CommitBatchFault = nil // "restart": the fault fires once per process, not forever

				// nothing committed -- the crash landed before tx.Commit()
				for _, st := range steps {
					stream, err := store.LoadStream(ctx, st.agg, st.id)
					if err != nil {
						t.Fatal(err)
					}
					if len(stream) != 0 {
						t.Fatalf("expected nothing committed after the simulated crash, found %d events for %s/%s",
							len(stream), st.agg, st.id)
					}
				}

				// "restart": run again for real -- must reproduce exactly
				// what would have happened without the crash
				if _, err := w.RunOnce(ctx); err != nil {
					t.Fatalf("replay after simulated crash failed: %v", err)
				}
			} else if runErr != nil {
				t.Fatalf("unexpected error with no simulated crash: %v", runErr)
			}

			// whether or not a crash was simulated, every command must now
			// be resolved exactly once
			for i, wait := range waits {
				select {
				case outcome := <-wait:
					if outcome.Err != nil {
						t.Fatalf("step %d: unexpected error %v", i, outcome.Err)
					}
				default:
					t.Fatalf("step %d: expected a resolved outcome after replay", i)
				}
			}

			t1, err := store.LoadStream(ctx, "task", "t1")
			if err != nil {
				t.Fatal(err)
			}
			if len(t1) != 2 || t1[0].Type != "TaskCreated" || t1[1].Type != "TaskCompleted" ||
				t1[0].Sequence != 1 || t1[1].Sequence != 2 {
				t.Fatalf("unexpected t1 stream: %+v", t1)
			}
			for _, id := range []string{"t2", "t3"} {
				stream, err := store.LoadStream(ctx, "task", id)
				if err != nil {
					t.Fatal(err)
				}
				if len(stream) != 1 || stream[0].Type != "TaskCreated" {
					t.Fatalf("unexpected %s stream: %+v", id, stream)
				}
			}

			// resuming again (a SECOND restart, or just the next tick)
			// must be a genuine no-op: nothing left pending, nothing
			// double-applied
			processed, err := w.RunOnce(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if processed != 0 {
				t.Fatalf("expected a no-op RunOnce after everything settled, processed %d", processed)
			}
			t1Again, err := store.LoadStream(ctx, "task", "t1")
			if err != nil {
				t.Fatal(err)
			}
			if len(t1Again) != 2 {
				t.Fatalf("expected t1 unchanged by the extra RunOnce, got %d events", len(t1Again))
			}
		})
	}
}

// TestChaosCommandAppliedNeverMissesAStampedEvent is a narrower, higher
// -iteration companion focused purely on the CommandApplied restart-check
// itself under randomized position floors and commandIds, independent of
// the full RunOnce pipeline -- a belt-and-braces check that the query
// convention (see events.CommandApplied's doc comment) holds up, not just
// the one scenario the writer-level test happens to construct.
func TestChaosCommandAppliedNeverMissesAStampedEvent(t *testing.T) {
	store, _, _, _ := setup(t)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(2))

	var lastPosition int64
	for i := 0; i < 200; i++ {
		before := lastPosition
		id := fmt.Sprintf("cmd-%d", i)
		results, err := store.CommitBatch(ctx, []events.DecidedCommand{
			{CommandID: id, Aggregate: "task", AggregateID: fmt.Sprintf("t%d", i), ExpectedSequence: 0,
				NewEvents: []events.NewEvent{{Type: "TaskCreated", Data: json.RawMessage(`{}`)}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		lastPosition = results[0].Events[0].Position

		applied, err := store.CommandApplied(ctx, "task", fmt.Sprintf("t%d", i), id, before)
		if err != nil {
			t.Fatal(err)
		}
		if !applied {
			t.Fatalf("iter %d: expected CommandApplied to find its own just-committed event", i)
		}

		// a random EARLIER command's id must never be found on THIS stream
		if i > 0 {
			randomEarlier := fmt.Sprintf("cmd-%d", rng.Intn(i))
			applied, err := store.CommandApplied(ctx, "task", fmt.Sprintf("t%d", i), randomEarlier, before)
			if err != nil {
				t.Fatal(err)
			}
			if applied {
				t.Fatalf("iter %d: found an unrelated command's id on the wrong stream", i)
			}
		}
	}
}
