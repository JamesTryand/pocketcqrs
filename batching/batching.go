// Package batching accumulates decided-but-uncommitted commands into
// batches and commits each batch's events in one SQLite transaction,
// instead of one transaction per command (item 4). This raises the write
// throughput ceiling under load by amortizing SQLite's fixed
// per-transaction cost across many commands -- something a bounded
// concurrency cap alone cannot do.
//
// The gateway's response contract is preserved: a caller enqueues a
// command and blocks until its own command's batch commits, then gets the
// same synchronous (events, error) result HandleWithMeta always returned.
// No correlation id, no polling, no client-visible change -- just variable
// added latency under load, bounded by the batch cadence.
//
// Correctness across a crash has two windows. Pre-commit (the batch never
// wrote) closes for free: Decide is pure and the queue preserves order, so
// replaying the same still-pending commands in the same order against the
// same durable state reproduces the exact same events -- provided
// everything non-deterministic a decide needs (now, the actor) was
// captured at enqueue time, never re-derived on a replay (see Enqueue).
// Post-commit, pre-completion-record closes because commandId is stamped
// into the same transaction that writes the events (events.CommitBatch) --
// the event row itself is the durable proof the command ran, checked via
// events.Store.CommandApplied on resume, so the queue's own "done" marking
// is pure bookkeeping that can lag arbitrarily with zero correctness risk.
package batching

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/jamestryand/pocketcqrs/commandqueue"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

// Outcome is one command's result once its batch has been decided.
type Outcome struct {
	Events []events.Event
	Err    error
}

// Writer accumulates commands and commits their decided events in batches.
type Writer struct {
	store    *events.Store
	queue    *commandqueue.Store
	registry *decider.Registry
	logger   func(msg string, args ...any)
	interval time.Duration
	maxBatch int

	nudge     chan struct{}
	waitersMu sync.Mutex
	waiters   map[string]chan Outcome
}

// NewWriter creates a Writer. logger may be nil (defaults to no-op).
func NewWriter(store *events.Store, queue *commandqueue.Store, registry *decider.Registry, logger func(string, ...any)) *Writer {
	if logger == nil {
		logger = func(string, ...any) {}
	}
	return &Writer{
		store:    store,
		queue:    queue,
		registry: registry,
		logger:   logger,
		interval: 8 * time.Millisecond,
		maxBatch: 256,
		nudge:    make(chan struct{}, 1),
		waiters:  map[string]chan Outcome{},
	}
}

// Enqueue durably records the command and returns a channel that receives
// exactly one Outcome once its batch has been decided (buffered, so a
// caller that stops waiting -- e.g. on a timeout -- never blocks the
// writer). It nudges the writer so a lone command doesn't wait a full tick.
//
// meta must already contain "now" (and anything else the decide needs that
// isn't itself deterministic, e.g. the actor) -- Enqueue refuses meta
// without it, rather than silently letting DecideWithMeta fill it in fresh
// on every decide attempt, including a post-crash replay, which would make
// the replayed events NOT match the original and break the whole pre-commit
// crash-safety argument this package depends on.
func (w *Writer) Enqueue(ctx context.Context, aggregate, aggregateID, command string, payload json.RawMessage, meta map[string]any) (id string, wait <-chan Outcome, err error) {
	if _, ok := meta["now"]; !ok {
		return "", nil, fmt.Errorf("batching: enqueue %s/%s/%s: meta must include \"now\", captured by the caller before enqueueing", aggregate, aggregateID, command)
	}

	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return "", nil, fmt.Errorf("batching: marshal meta: %w", err)
	}
	head, err := w.store.MaxPosition(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("batching: read head position: %w", err)
	}
	qc, err := w.queue.EnqueueCommand(ctx, aggregate, aggregateID, command, payload, metaJSON, head)
	if err != nil {
		return "", nil, err
	}

	ch := make(chan Outcome, 1)
	w.waitersMu.Lock()
	w.waiters[qc.ID] = ch
	w.waitersMu.Unlock()

	select {
	case w.nudge <- struct{}{}:
	default:
	}

	return qc.ID, ch, nil
}

// Start runs the batch-forming loop until ctx is done: immediately on
// every Enqueue's nudge, and on a fallback ticker (covers a burst that
// arrives while a batch is already forming, and any missed nudge).
func (w *Writer) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			if _, err := w.RunOnce(ctx); err != nil {
				w.logger("batching run error", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-w.nudge:
			case <-ticker.C:
			}
		}
	}()
}

// RunOnce drains up to maxBatch pending commands once: resumes any whose
// events already landed in a prior run that crashed before marking them
// done, decides the rest sequentially against a fresh per-window overlay,
// commits their events in one transaction, and signals every waiter. The
// unit a fault-injection test drives directly.
func (w *Writer) RunOnce(ctx context.Context) (processed int, err error) {
	pending, err := w.queue.PendingCommands(ctx, w.maxBatch)
	if err != nil {
		return 0, err
	}
	if len(pending) == 0 {
		return 0, nil
	}

	var toDecide []commandqueue.QueuedCommand
	for _, qc := range pending {
		applied, err := w.store.CommandApplied(ctx, qc.Aggregate, qc.AggregateID, qc.ID, qc.EnqueuedAtPosition)
		if err != nil {
			return processed, err
		}
		if applied {
			// a prior run committed this command's events but crashed
			// before marking it done -- no live waiter to signal (that
			// request's connection is long gone), just clean up, marked
			// immediately rather than deferred to a later step that a
			// subsequent failure in this same pass could skip.
			w.markDoneOne(ctx, qc.ID)
			processed++
			continue
		}
		toDecide = append(toDecide, qc)
	}

	ov := newOverlay(w.store)
	batch := make([]events.DecidedCommand, 0, len(toDecide))
	for _, qc := range toDecide {
		var meta map[string]any
		if len(qc.Meta) > 0 {
			if err := json.Unmarshal(qc.Meta, &meta); err != nil {
				w.signal(qc.ID, Outcome{Err: fmt.Errorf("batching: invalid queued meta: %w", err)})
				w.markDoneOne(ctx, qc.ID)
				processed++
				continue
			}
		}

		newEvents, expectedSeq, err := w.registry.DecideWithMeta(ctx, ov, qc.Aggregate, qc.AggregateID,
			decider.Command{Name: qc.Command, Payload: qc.Payload}, meta)
		if err != nil {
			// a domain rejection (or unknown-aggregate) has no side effect
			// -- exactly like today's synchronous HandleWithMeta, it is
			// surfaced to the caller and not automatically retried. Marked
			// done immediately, independent of whatever a LATER command's
			// CommitBatch call in this same pass does -- a batch-commit
			// failure elsewhere in this RunOnce call must not leave an
			// already-signaled rejection stuck pending forever.
			w.signal(qc.ID, Outcome{Err: err})
			w.markDoneOne(ctx, qc.ID)
			processed++
			continue
		}

		batch = append(batch, events.DecidedCommand{
			CommandID:        qc.ID,
			Aggregate:        qc.Aggregate,
			AggregateID:      qc.AggregateID,
			ExpectedSequence: expectedSeq,
			NewEvents:        newEvents,
		})
		ov.record(qc.Aggregate, qc.AggregateID, newEvents, expectedSeq)
	}

	if len(batch) > 0 {
		results, err := w.store.CommitBatch(ctx, batch)
		if err != nil {
			return processed, err
		}
		var doneIDs []string
		for _, r := range results {
			if r.Err != nil {
				// ErrConcurrency or ErrBatchAborted: nothing committed for
				// this command. Leave it pending -- no signal, the caller
				// (a blocked gateway request, if this process is still the
				// one holding it) keeps waiting for a future pass to
				// redecide it against then-current state.
				w.logger("batching command not committed, will redecide next pass",
					"commandId", r.CommandID, "error", r.Err)
				continue
			}
			doneIDs = append(doneIDs, r.CommandID)
			w.signal(r.CommandID, Outcome{Events: r.Events})
		}
		if len(doneIDs) > 0 {
			if err := w.queue.MarkDone(ctx, doneIDs...); err != nil {
				w.logger("batching mark done failed", "error", err)
			}
			processed += len(doneIDs)
		}
	}

	return processed, nil
}

// markDoneOne marks a single command done, logging (not failing the
// caller) on error -- used for the rare, individually-resolved paths
// (resume cleanup, decide-time rejection) where correctness never depends
// on this succeeding promptly, only eventually.
func (w *Writer) markDoneOne(ctx context.Context, id string) {
	if err := w.queue.MarkDone(ctx, id); err != nil {
		w.logger("batching mark done failed", "error", err, "commandId", id)
	}
}

func (w *Writer) signal(id string, outcome Outcome) {
	w.waitersMu.Lock()
	ch, ok := w.waiters[id]
	if ok {
		delete(w.waiters, id)
	}
	w.waitersMu.Unlock()
	if ok {
		ch <- outcome
	}
}
