// Package projections folds events from the event store into ordinary
// PocketBase collections, so the stock PocketBase REST/realtime/auth API
// serves the query side unchanged.
//
// Projections are consumers (see package consumers): delivery is
// at-least-once, Apply implementations must be idempotent, and progress is
// checkpointed in the event store. A rebuild is a checkpoint reset plus
// optionally wiping the target collection.
package projections

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/pocketbase/pocketbase/core"

	"github.com/JamesTryand/pocketcqrs/consumers"
	"github.com/JamesTryand/pocketcqrs/events"
	"github.com/JamesTryand/pocketcqrs/writeguard"
)

// Projection materializes events into PocketBase collections.
type Projection interface {
	consumers.Consumer
	// Collections lists the guarded collections this projection owns.
	Collections() []string
}

// GuardedCollections returns the union of the given projections' collections,
// for registering with the writeguard.
func GuardedCollections(ps ...Projection) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Collections()...)
	}
	return out
}

// NewTasks projects task events into the "tasks" collection.
func NewTasks(app core.App) Projection { return tasksProjection{app: app} }

type tasksProjection struct {
	app core.App
}

type taskEventData struct {
	Title string `json:"title"`
}

func (tasksProjection) Name() string          { return "tasks" }
func (tasksProjection) Collections() []string { return []string{"tasks"} }

// EventTypes declares the consumed event types (introspection; the engine
// still delivers everything — Apply self-filters).
func (tasksProjection) EventTypes() []string { return []string{"TaskCreated", "TaskCompleted"} }

func (p tasksProjection) Apply(ctx context.Context, ev events.Event) error {
	app := p.app
	// the internal marker lets projection writes pass the writeguard
	ctx = writeguard.MarkInternal(ctx)

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
