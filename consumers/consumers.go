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

// ReadModel is an optional interface a Consumer implements to say whether
// queries read its output. A read model's lag, and whether it is blocked,
// count toward the node's readiness (health/telemetry contract section 4.5);
// reactors and effect functions only delay side effects, so they are reported
// as metrics instead. A consumer without it is a read model exactly when it
// owns collections (Collections() []string, as every Go and JS projection
// does).
type ReadModel interface {
	IsReadModel() bool
}

// IsReadModel reports whether c counts toward readiness (see ReadModel).
func IsReadModel(c Consumer) bool {
	if r, ok := c.(ReadModel); ok {
		return r.IsReadModel()
	}
	_, owns := c.(interface{ Collections() []string })
	return owns
}

// HeadSource is an optional interface a PollSource implements to report the
// newest committed position, for each consumer's lag in positions.
// *events.Store satisfies it. Without it that lag is unknown.
type HeadSource interface {
	MaxPosition(ctx context.Context) (int64, error)
}

// State is a consumer's state, as the health/telemetry contract names it
// (STATE-MACHINES.md, machine 2, ReadModelConsumer). Stopped needs no value:
// an unregistered consumer is simply not listed.
type State int

const (
	// Current: lag within the engine's LagThreshold.
	Current State = iota
	// Behind: lag over the threshold, or not yet measured (the consumer has
	// not run a pass).
	Behind
	// Blocked: stuck on a failing event (or failing to read), retrying
	// every pass.
	Blocked
)

func (s State) String() string {
	switch s {
	case Current:
		return "current"
	case Behind:
		return "behind"
	default:
		return "blocked"
	}
}

// Status is one consumer's progress, from Engine.Status.
type Status struct {
	Name string
	// ReadModel: counts toward readiness (see ReadModel).
	ReadModel bool
	State     State
	// Checkpoint is the last position it applied; nil before its first pass.
	Checkpoint *int64
	// LagPositions is how far behind the log head it was as of its last
	// pass; nil when the source has no HeadSource, or before its first pass.
	LagPositions *int64
	// LagSeconds is the age of the oldest event it has not yet applied (0
	// when caught up); nil before its first pass.
	LagSeconds *float64
}

// DefaultLagThreshold is Engine.LagThreshold's default.
const DefaultLagThreshold = 5 * time.Second

// progress is a consumer's progress as of its latest pass: its checkpoint,
// the log head that pass saw (nil: unknown), when the oldest event it has not
// applied was committed (zero: caught up), and whether it is blocked.
type progress struct {
	checkpoint   int64
	head         *int64
	pendingSince time.Time
	blocked      bool
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

	// LagThreshold is how old the oldest unapplied event may be before a
	// consumer counts as Behind rather than Current. Set it before Start.
	LagThreshold time.Duration

	// progressMu guards progress: each consumer's progress as of its latest
	// pass, for Status. Written only by the pass.
	progressMu sync.Mutex
	progress   map[string]progress
	now        func() time.Time
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
		source:       source,
		checkpoints:  checkpoints,
		nudge:        make(chan struct{}, 1),
		tick:         time.Second,
		logger:       logger,
		LagThreshold: DefaultLagThreshold,
		progress:     map[string]progress{},
		now:          time.Now,
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
			break
		}
	}
	e.progressMu.Lock()
	delete(e.progress, name)
	e.progressMu.Unlock()
}

// Status returns every registered consumer's state and lag
// (health/telemetry contract sections 4.4 and 4.5), sorted by name. It is
// read from what the passes last recorded, so it never touches the store: the
// lag in seconds is measured now, against the oldest event each consumer has
// not yet applied. A consumer that has not run a pass yet is Behind with
// unknown lag.
func (e *Engine) Status() []Status {
	e.mu.RLock()
	consumers := append([]Consumer(nil), e.consumers...)
	e.mu.RUnlock()

	now := e.now()
	e.progressMu.Lock()
	defer e.progressMu.Unlock()
	out := make([]Status, 0, len(consumers))
	for _, c := range consumers {
		s := Status{Name: c.Name(), ReadModel: IsReadModel(c), State: Behind}
		if p, ok := e.progress[c.Name()]; ok {
			checkpoint, lag := p.checkpoint, 0.0
			if !p.pendingSince.IsZero() {
				lag = max(0, now.Sub(p.pendingSince).Seconds())
			}
			s.Checkpoint, s.LagSeconds = &checkpoint, &lag
			if p.head != nil {
				behind := max(0, *p.head-p.checkpoint)
				s.LagPositions = &behind
			}
			switch {
			case p.blocked:
				s.State = Blocked
			case lag <= e.LagThreshold.Seconds():
				s.State = Current
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (e *Engine) record(name string, p progress) {
	e.progressMu.Lock()
	e.progress[name] = p
	e.progressMu.Unlock()
}

func (e *Engine) recorded(name string) (progress, bool) {
	e.progressMu.Lock()
	defer e.progressMu.Unlock()
	p, ok := e.progress[name]
	return p, ok
}

// createdAt is when ev was committed. A timestamp that does not parse counts
// from now, so its lag still grows while it waits.
func (e *Engine) createdAt(ev events.Event) time.Time {
	// the store writes "2006-01-02 15:04:05.000Z"; imported events may carry
	// RFC 3339
	for _, layout := range []string{"2006-01-02 15:04:05Z07:00", time.RFC3339Nano} {
		if t, err := time.Parse(layout, ev.Created); err == nil {
			return t
		}
	}
	return e.now()
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
	// read once per pass, for each consumer's lag in positions; a failure
	// here is the store being unreachable, which every consumer is about to
	// report as blocked anyway
	var head *int64
	if hs, ok := e.source.(HeadSource); ok {
		if h, err := hs.MaxPosition(ctx); err == nil {
			head = &h
		}
	}
	var errs []error
	for _, c := range consumers {
		if stop.Err() != nil {
			break
		}
		if err := e.runOnceFor(ctx, stop, c, head); err != nil {
			errs = append(errs, fmt.Errorf("consumer %s: %w", c.Name(), err))
		}
	}
	return errors.Join(errs...)
}

// runOnceFor applies every pending event to a single consumer until caught
// up, or until the consumer's own checkpoint/poll/apply fails, recording its
// progress for Status as it goes.
func (e *Engine) runOnceFor(ctx, stop context.Context, c Consumer, head *int64) error {
	name := c.Name()
	before, _ := e.recorded(name)
	// a blocked consumer stays blocked while it retries, until an event applies
	blocked, pendingSince := before.blocked, before.pendingSince
	var pos int64
	fail := func(err error) error {
		if pendingSince.IsZero() {
			pendingSince = e.now()
		}
		e.record(name, progress{checkpoint: pos, head: head, pendingSince: pendingSince, blocked: true})
		return err
	}
	pos, err := e.checkpoints.Checkpoint(ctx, name)
	if err != nil {
		return fail(err)
	}
	for {
		batch, err := e.source.Poll(ctx, pos, 100)
		if err != nil {
			return fail(err)
		}
		if len(batch) == 0 {
			break
		}
		for _, ev := range batch {
			// the event about to be applied is the oldest one not yet applied
			pendingSince = e.createdAt(ev)
			e.record(name, progress{checkpoint: pos, head: head, pendingSince: pendingSince, blocked: blocked})
			if err := c.Apply(ctx, ev); err != nil {
				e.logger("consumer apply error",
					"consumer", name, "position", ev.Position, "error", err)
				return fail(err)
			}
			if err := e.checkpoints.SaveCheckpoint(ctx, name, ev.Position); err != nil {
				return fail(err)
			}
			pos = ev.Position
			blocked = false
			if stop.Err() != nil {
				return nil
			}
		}
	}
	e.record(name, progress{checkpoint: pos, head: head})
	return nil
}
