// Command gohost is the smallest program with a compiled-in Go domain: the
// whole pocketcqrs program (host.Run) plus the tutorial's task and order
// aggregates and projections, registered by the program itself rather than by
// --tutorial, with the command permissions an embedded EventModel document
// declares. The smoke suite runs it; docs/go-guide.md ("Your own program")
// walks through it.
package main

import (
	_ "embed"

	"github.com/jamestryand/pocketcqrs/aggregates"
	"github.com/jamestryand/pocketcqrs/emschema"
	"github.com/jamestryand/pocketcqrs/host"
	"github.com/jamestryand/pocketcqrs/migrations"
	"github.com/jamestryand/pocketcqrs/projections"
)

//go:embed task.eventmodel.json
var model []byte

func main() {
	// the read models' collections are ordinary migrations, registered
	// before Run (here the tutorial's own tasks and orders collections)
	migrations.RegisterExamples()

	host.Run(host.Options{
		Register: func(h *host.Host) error {
			doc, err := emschema.Parse("task.eventmodel.json", model)
			if err != nil {
				return err
			}
			if err := h.DeclareCommands(doc); err != nil {
				return err
			}
			aggregates.RegisterAll(h.Registry)
			h.AddProjection(projections.NewTasks(h.App))
			h.AddProjection(projections.NewOrders(h.App))
			return nil
		},
	})
}
