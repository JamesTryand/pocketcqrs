package main

// This file is the whole of the example wiring in main. Everything it names —
// the task and order aggregates, their projections, the fulfillment reactor
// and the collection migrations — is example content: shipped to be read and
// run under --tutorial, never a dependency of the platform. Keeping it in one
// file means "what counts as an example?" is answerable by reading one file
// rather than by grepping the boot sequence.

import (
	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/projections"
)

// exampleProjections are the Go projections this repo ships as examples.
func exampleProjections(app core.App) []projections.Projection {
	return []projections.Projection{
		projections.NewTasks(app),
		projections.NewOrders(app),
	}
}

// exampleCollections names every collection the example projections own.
//
// Derived from the projections rather than written out as a literal list, so
// it cannot drift from what they actually maintain — order_lines belongs to
// the orders projection and is easy to forget, and a future example
// projection is covered without touching this.
func exampleCollections(app core.App) []string {
	return projections.GuardedCollections(exampleProjections(app)...)
}
