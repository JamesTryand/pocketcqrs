//go:build smoke

// Command gokill is a program with a compiled-in Go domain, built only by the
// Go consumer kill tests (smoke/go_consumer_kill_test.go), which need a Go
// projection and a Go reactor in a real process they can kill mid-work.
//
// It registers the tutorial's task and order aggregates and the tasks
// collection always, and one consumer as GOKILL_CONSUMER says:
//
//   - "projection": the tutorial's Go tasks projection;
//   - "reactor": the echo reactor below;
//   - unset: neither, so a backlog can be written with the consumer absent.
package main

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/jamestryand/pocketcqrs/aggregates"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/host"
	"github.com/jamestryand/pocketcqrs/migrations"
	"github.com/jamestryand/pocketcqrs/projections"
	"github.com/jamestryand/pocketcqrs/reactors"
)

func main() {
	migrations.RegisterExamples()
	host.Run(host.Options{
		Register: func(h *host.Host) error {
			aggregates.RegisterAll(h.Registry)
			switch os.Getenv("GOKILL_CONSUMER") {
			case "projection":
				h.AddProjection(projections.NewTasks(h.App))
			case "reactor":
				h.AddReactor(echo{})
			}
			return nil
		},
	})
}

// echo creates the task "echo-<id>" for every task created that is not
// itself an echo: one command per event, to an id derived from the event.
type echo struct{}

func (echo) Name() string { return "echo" }

func (echo) React(ev events.Event) []reactors.Reaction {
	if ev.Aggregate != aggregates.TaskAggregate || ev.Type != aggregates.TaskCreated || strings.HasPrefix(ev.AggregateID, "echo-") {
		return nil
	}
	payload, _ := json.Marshal(map[string]string{"title": "echo " + ev.AggregateID})
	return []reactors.Reaction{{
		Aggregate: aggregates.TaskAggregate,
		ID:        "echo-" + ev.AggregateID,
		Command:   decider.Command{Name: aggregates.CmdCreateTask, Payload: payload},
	}}
}
