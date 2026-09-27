// Package consumers provides the shared checkpointed-consumption engine:
// named consumers follow the event log in position order with durable
// checkpoints, so delivery survives restarts. Checkpoints normally live in
// the same event store being polled (NewEngine); a read-only replica polls
// one store but checkpoints to a different, locally writable one
// (NewEngineWithCheckpoints).
//
// Projections and event-triggered functions are both consumers. Delivery is
// at-least-once: a consumer's Apply must be idempotent, or accept that a
// crash between Apply and checkpoint advance replays the event.
package consumers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/jamestryand/pocketcqrs/events"
)

// Consumer processes events from the log in position order.
type Consumer interface {
	// Name is the durable checkpoint key.
	Name() string
	// Apply handles one event. Must be idempotent.
	Apply(ctx context.Context, ev events.Event) error
}

// PollSource is the event feed an Engine follows: Poll for catch-up batches,
// Subscribe for the in-process nudge that shortens the usual tick latency.
// *events.Store satisfies this whether opened with Open or OpenReadOnly.
type PollSource interface {
	Poll(ctx context.Context, after int64, limit int) ([]events.Event, error)
	Subscribe(fn func(events.Event))
}

// CheckpointStore durably persists consumer progress. *events.Store
// satisfies this, but only when opened with Open — a Store opened with
// OpenReadOnly returns events.ErrReadOnly from SaveCheckpoint, which is why
// a secondary polling a read-only replica needs a *different*,
// locally-writable CheckpointStore (see NewEngineWithCheckpoints).
type CheckpointStore interface {
	Checkpoint(ctx context.Context, name string) (int64, error)
	SaveCheckpoint(ctx context.Context, name string, position int64) error
}

// Engine polls an event source and feeds every registered consumer
// independently, each with its own durable checkpoint.
type Engine struct {
	source      PollSource
	checkpoints CheckpointStore

	// mu guards consumers so the set can be swapped (hot reload) while
	// the poll loop runs.
	mu        sync.RWMutex
	consumers []Consumer

	nudge  chan struct{}
	tick   time.Duration
	logger func(msg string, args ...any)
}

// NewEngine creates an Engine that both polls store and checkpoints
// against it — the ordinary single-node/master shape. logger may be nil
// (defaults to no-op).
func NewEngine(store *events.Store, logger func(string, ...any)) *Engine {
	return NewEngineWithCheckpoints(store, store, logger)
}

// NewEngineWithCheckpoints creates an Engine that polls source but saves
// checkpoints to a separate checkpoints store. Use this on a
// single-writer/multi-reader secondary: source is a Store opened with
// events.OpenReadOnly against the replicated events.db, and checkpoints is
// a normal, locally writable *events.Store (or anything else satisfying
// CheckpointStore) the secondary owns outright. logger may be nil (defaults
// to no-op).
func NewEngineWithCheckpoints(source PollSource, checkpoints CheckpointStore, logger func(string, ...any)) *Engine {
	if logger == nil {
		logger = func(string, ...any) {}
	}
	return &Engine{
		source:      source,
		checkpoints: checkpoints,
		nudge:       make(chan struct{}, 1),
		tick:        time.Second,
		logger:      logger,
	}
}

// Register adds a consumer.
func (e *Engine) Register(c Consumer) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.consumers = append(e.consumers, c)
}

// Unregister drops the consumer with name (no-op if absent). The durable
// checkpoint is kept, so re-registering later resumes where it left off.
func (e *Engine) Unregister(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, c := range e.consumers {
		if c.Name() == name {
			e.consumers = append(e.consumers[:i], e.consumers[i+1:]...)
			return
		}
	}
}

// Names returns the registered consumer names, sorted (a snapshot).
func (e *Engine) Names() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]string, 0, len(e.consumers))
	for _, c := range e.consumers {
		out = append(out, c.Name())
	}
	sort.Strings(out)
	return out
}

// Checkpoint returns the named consumer's durable position from THIS
// engine's checkpoint store — the one it actually advances. On a secondary
// that is the local checkpoints store, not the replicated source, whose
// checkpoint table holds the master's progress (see
// NewEngineWithCheckpoints). 0 for a consumer that has never run.
func (e *Engine) Checkpoint(ctx context.Context, name string) (int64, error) {
	return e.checkpoints.Checkpoint(ctx, name)
}

// Start runs the catch-up loop in the background until ctx is done:
// immediately on every committed event (in-process nudge) and on a slow
// ticker fallback (covers restarts and missed nudges). Run is the blocking
// form, for a caller that needs to wait for the loop to exit.
func (e *Engine) Start(ctx context.Context) {
	e.subscribe()
	go e.loop(ctx)
}

// Run is Start, blocking until the loop has exited. Cancelling ctx is a
// stop request, not an abort: the delivery in flight finishes and is
// checkpointed, no further event is started, and Run returns — the shape a
// shutdown hook waits on.
func (e *Engine) Run(ctx context.Context) {
	e.subscribe()
	e.loop(ctx)
}

func (e *Engine) subscribe() {
	e.source.Subscribe(func(events.Event) {
		select {
		case e.nudge <- struct{}{}:
		default:
		}
	})
}

func (e *Engine) loop(stop context.Context) {
	// deliveries run on a context the stop does not cancel, so a stop never
	// cuts an Apply/SaveCheckpoint pair in half; runOnce checks stop
	// between events instead
	work := context.WithoutCancel(stop)
	ticker := time.NewTicker(e.tick)
	defer ticker.Stop()
	for {
		if stop.Err() != nil {
			return
		}
		if err := e.runOnce(work, stop); err != nil {
			e.logger("consumer run error", "error", err)
		}
		select {
		case <-stop.Done():
			return
		case <-e.nudge:
		case <-ticker.C:
		}
	}
}

// RunOnce applies every pending event to every consumer until caught up.
// A failing consumer stops at the failing event and retries next pass;
// other consumers are unaffected — their turn in this same pass, and every
// future pass, is unaffected by an earlier consumer's failure. The consumer
// set is snapshotted first, so reload-driven swaps apply cleanly to the next
// pass. Returns an aggregated error (via errors.Join) covering every
// consumer that failed this pass, or nil if all succeeded.
func (e *Engine) RunOnce(ctx context.Context) error {
	return e.runOnce(ctx, ctx)
}

// runOnce is RunOnce with the stop signal separated from the work context:
// once stop is done, no further event is started (the one in flight has
// already finished, checkpoint included).
func (e *Engine) runOnce(ctx, stop context.Context) error {
	e.mu.RLock()
	consumers := append([]Consumer(nil), e.consumers...)
	e.mu.RUnlock()
	var errs []error
	for _, c := range consumers {
		if stop.Err() != nil {
			break
		}
		if err := e.runOnceFor(ctx, stop, c); err != nil {
			errs = append(errs, fmt.Errorf("consumer %s: %w", c.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// runOnceFor applies every pending event to a single consumer until caught
// up, or until the consumer's own checkpoint/poll/apply fails.
func (e *Engine) runOnceFor(ctx, stop context.Context, c Consumer) error {
	pos, err := e.checkpoints.Checkpoint(ctx, c.Name())
	if err != nil {
		return err
	}
	for {
		batch, err := e.source.Poll(ctx, pos, 100)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		for _, ev := range batch {
			if err := c.Apply(ctx, ev); err != nil {
				e.logger("consumer apply error",
					"consumer", c.Name(), "position", ev.Position, "error", err)
				return err
			}
			if err := e.checkpoints.SaveCheckpoint(ctx, c.Name(), ev.Position); err != nil {
				return err
			}
			pos = ev.Position
			if stop.Err() != nil {
				return nil
			}
		}
	}
	return nil
}
