// Package projections folds events from the event store into ordinary
// PocketBase collections, so the stock PocketBase REST/realtime/auth API
// serves the query side unchanged.
//
// Delivery is at-least-once: projection Apply implementations must be
// idempotent. Progress is checkpointed in the event store; a rebuild is
// a checkpoint reset (plus optionally wiping the target collection).
package projections

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"pocketcqrs/events"
	"pocketcqrs/writeguard"
)

// Projection materializes events into PocketBase collections.
type Projection interface {
	// Name is the durable checkpoint key.
	Name() string
	// Collections lists the guarded collections this projection owns.
	Collections() []string
	// Apply handles one event. Must be idempotent.
	Apply(ctx context.Context, app core.App, ev events.Event) error
}

// Engine polls the event store and feeds projections in position order.
type Engine struct {
	app    core.App
	store  *events.Store
	projs  []Projection
	nudge  chan struct{}
	tick   time.Duration
	logger func(msg string, args ...any)
}

// NewEngine creates an Engine. logger may be nil (defaults to no-op).
func NewEngine(app core.App, store *events.Store, logger func(string, ...any)) *Engine {
	if logger == nil {
		logger = func(string, ...any) {}
	}
	return &Engine{
		app:    app,
		store:  store,
		nudge:  make(chan struct{}, 1),
		tick:   time.Second,
		logger: logger,
	}
}

// Register adds a projection.
func (e *Engine) Register(p Projection) {
	e.projs = append(e.projs, p)
}

// GuardedCollections returns the union of all projections' collections,
// for registering with the writeguard.
func (e *Engine) GuardedCollections() []string {
	var out []string
	for _, p := range e.projs {
		out = append(out, p.Collections()...)
	}
	return out
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
				e.logger("projection run error", "error", err)
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

// RunOnce applies every pending event to every projection until caught up.
func (e *Engine) RunOnce(ctx context.Context) error {
	for _, p := range e.projs {
		pos, err := e.store.Checkpoint(ctx, p.Name())
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
				// the internal marker lets projection writes pass the writeguard
				if err := p.Apply(writeguard.MarkInternal(ctx), e.app, ev); err != nil {
					e.logger("projection apply error",
						"projection", p.Name(), "position", ev.Position, "error", err)
					return err
				}
				if err := e.store.SaveCheckpoint(ctx, p.Name(), ev.Position); err != nil {
					return err
				}
				pos = ev.Position
			}
		}
	}
	return nil
}

// Tasks projects task events into the "tasks" collection.
func Tasks() Projection { return tasksProjection{} }

type tasksProjection struct{}

type taskEventData struct {
	Title string `json:"title"`
}

func (tasksProjection) Name() string         { return "tasks" }
func (tasksProjection) Collections() []string { return []string{"tasks"} }

func (tasksProjection) Apply(ctx context.Context, app core.App, ev events.Event) error {
	switch ev.Type {
	case "TaskCreated":
		var data taskEventData
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			return err
		}
		rec, err := findTask(app, ev.AggregateID)
		if err != nil {
			return err
		}
		if rec != nil {
			return nil // already applied
		}
		col, err := app.FindCollectionByNameOrId("tasks")
		if err != nil {
			return err
		}
		rec = core.NewRecord(col)
		rec.Set("taskId", ev.AggregateID)
		rec.Set("title", data.Title)
		rec.Set("completed", false)
		return app.SaveWithContext(ctx, rec)

	case "TaskCompleted":
		rec, err := findTask(app, ev.AggregateID)
		if err != nil {
			return err
		}
		if rec == nil || rec.GetBool("completed") {
			return nil // not yet created (out of order replay) or already applied
		}
		rec.Set("completed", true)
		return app.SaveWithContext(ctx, rec)
	}
	return nil
}

func findTask(app core.App, taskID string) (*core.Record, error) {
	rec, err := app.FindFirstRecordByData("tasks", "taskId", taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return rec, err
}
