// Package migrations holds the PocketCQRS PocketBase migrations.
//
// Collections are treated as infrastructure (DDL), created here rather
// than via domain events. Migrations registered via migrations.Register
// are auto-applied on `serve`.
package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/migrations"
	"github.com/pocketbase/pocketbase/tools/types"
)

func init() {
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
	})
}
