// Package migrations holds this repo's EXAMPLE PocketBase migrations.
//
// Collections are treated as infrastructure (DDL), created here rather
// than via domain events.
//
// Nothing here registers itself. RegisterExamples must be called, and main
// only calls it under --tutorial, because a migration that is never
// registered is never applied and never recorded — which is what makes the
// example domains switchable in both directions. Gating inside the up
// function would NOT: PocketBase records a migration as applied whenever up
// returns nil, so an empty first boot would stamp these as done and a later
// --tutorial boot would skip them, leaving the collections permanently
// uncreated.
package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/migrations"
	"github.com/pocketbase/pocketbase/tools/types"
)

func registerTasksCollection() {
	migrations.Register(func(app core.App) error {
		if _, err := app.FindCollectionByNameOrId("tasks"); err == nil {
			return nil // already exists
		}

		c := core.NewBaseCollection("tasks")
		// read side: publicly queryable; writes are rejected by the
		// writeguard regardless of these rules (nil = API-level deny)
		c.ListRule = types.Pointer("")
		c.ViewRule = types.Pointer("")
		c.Fields.Add(
			&core.TextField{Name: "taskId", Required: true},
			&core.TextField{Name: "title", Required: true},
			&core.BoolField{Name: "completed"},
		)
		c.Indexes = types.JSONArray[string]{
			"CREATE UNIQUE INDEX idx_tasks_taskId ON tasks (taskId)",
		}
		return app.Save(c)
	}, func(app core.App) error {
		c, err := app.FindCollectionByNameOrId("tasks")
		if err != nil {
			return nil // already deleted
		}
		return app.Delete(c)
		// The filename below is passed explicitly rather than left to
		// runtime.Caller: it is this migration's identity in _migrations,
		// and pinning the string means moving or renaming this file cannot
		// orphan the rows already recorded against it.
	}, "1754200000_tasks_collection.go")
}
