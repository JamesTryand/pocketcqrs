// Package consumers provides the shared checkpointed-consumption engine:
// named consumers follow the event log in position order with durable
// checkpoints (stored in the event store), so delivery survives restarts.
//
// Projections and event-triggered functions are both consumers. Delivery is
// at-least-once: a consumer's Apply must be idempotent, or accept that a
// crash between Apply and checkpoint advance replays the event.
package consumers

import (
	"context"
	"time"

	"pocketcqrs/events"
)

// Consumer processes events from the log in position order.
type Consumer interface {
	// Name is the durable checkpoint key.
	Name() string
	// Apply handles one event. Must be idempotent.
	Apply(ctx context.Context, ev events.Event) error
}

// Engine polls the event store and feeds every registered consumer
// independently, each with its own durable checkpoint.
type Engine struct {
	store     *events.Store
	consumers []Consumer
	nudge     chan struct{}
	tick      time.Duration
	logger    func(msg string, args ...any)
}

// NewEngine creates an Engine. logger may be nil (defaults to no-op).
func NewEngine(store *events.Store, logger func(string, ...any)) *Engine {
	if logger == nil {
		logger = func(string, ...any) {}
	}
	return &Engine{
		store:  store,
		nudge:  make(chan struct{}, 1),
		tick:   time.Second,
		logger: logger,
	}
}

// Register adds a consumer.
func (e *Engine) Register(c Consumer) {
	e.consumers = append(e.consumers, c)
}

// Start runs the catch-up loop until ctx is done: immediately on every
// committed event (in-process nudge) and on a slow ticker fallback
// (covers restarts and missed nudges).
func (e *Engine) Start(ctx context.Context) {
	e.store.Subscribe(func(events.Event) {
		select {
		case e.nudge <- struct{}{}:
		default:
		}
	})

	go func() {
		ticker := time.NewTicker(e.tick)
		defer ticker.Stop()
		for {
			if err := e.RunOnce(ctx); err != nil {
				e.logger("consumer run error", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-e.nudge:
			case <-ticker.C:
			}
		}
	}()
}

// RunOnce applies every pending event to every consumer until caught up.
// A failing consumer stops at the failing event and retries next pass;
// other consumers are unaffected.
func (e *Engine) RunOnce(ctx context.Context) error {
	for _, c := range e.consumers {
		pos, err := e.store.Checkpoint(ctx, c.Name())
		if err != nil {
			return err
		}
		for {
			batch, err := e.store.Poll(ctx, pos, 100)
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
				if err := e.store.SaveCheckpoint(ctx, c.Name(), ev.Position); err != nil {
					return err
				}
				pos = ev.Position
			}
		}
	}
	return nil
}
