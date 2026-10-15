package reactors

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jamestryand/pocketcqrs/aggregates"
	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

// counting is a reactor that folds a running count from the orders it sees
// (a process manager's state) and asks for a task on each one.
type counting struct{ seen int }

func (c *counting) Name() string { return "counting" }
func (c *counting) React(ev events.Event) []Reaction {
	if ev.Type != aggregates.OrderConfirmed {
		return nil
	}
	c.seen++
	payload, _ := json.Marshal(map[string]string{"title": "fulfil " + ev.AggregateID})
	return []Reaction{{Aggregate: aggregates.TaskAggregate, ID: "fulfil-" + ev.AggregateID,
		Command: decider.Command{Name: aggregates.CmdCreateTask, Payload: payload}}}
}

// A Go reactor is effect-tier: an imported OrderConfirmed already had its
// task created at the source deployment, so it reaches React (the reactor
// folds it, here a running count) but nothing is dispatched. A native one
// is acted on as usual.
func TestImportedEventIsFoldedByAReactorButNotDispatched(t *testing.T) {
	store, r := setup(t)
	ctx := context.Background()

	if _, err := store.ImportEvents(ctx, []events.Event{
		{ID: "x1", Aggregate: aggregates.OrderAggregate, AggregateID: "elsewhere", Sequence: 1,
			Type: aggregates.OrderConfirmed, Data: json.RawMessage(`{}`), Created: "2026-10-01 10:00:00.000Z"},
	}); err != nil {
		t.Fatal(err)
	}
	placeAndConfirmOrder(t, r, "o1")

	reactor := &counting{}
	c := AsConsumer(reactor, r, nil, nil)
	if !consumers.IsEffect(c) {
		t.Fatal("a reactor is effect-tier")
	}
	engine := consumers.NewEngine(store, func(string, ...any) {})
	engine.Register(c)
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	if reactor.seen != 2 {
		t.Errorf("the reactor folds both confirmations, imported and native: saw %d", reactor.seen)
	}
	if tasks, _ := store.LoadStream(ctx, aggregates.TaskAggregate, "fulfil-elsewhere"); len(tasks) != 0 {
		t.Errorf("no task for the imported confirmation: %+v", tasks)
	}
	if tasks, _ := store.LoadStream(ctx, aggregates.TaskAggregate, "fulfil-o1"); len(tasks) != 1 {
		t.Errorf("one task for the native confirmation, got %d", len(tasks))
	}
}
