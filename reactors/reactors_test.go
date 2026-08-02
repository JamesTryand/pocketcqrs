package reactors

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/JamesTryand/pocketcqrs/aggregates"
	"github.com/JamesTryand/pocketcqrs/decider"
	"github.com/JamesTryand/pocketcqrs/events"
)

func setup(t *testing.T) (*events.Store, *decider.Registry) {
	t.Helper()
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	r := decider.NewRegistry(store)
	aggregates.RegisterAll(r)
	return store, r
}

func placeAndConfirmOrder(t *testing.T, r *decider.Registry, orderID string) {
	t.Helper()
	ctx := context.Background()
	mk := func(name string, payload any) decider.Command {
		data, _ := json.Marshal(payload)
		return decider.Command{Name: name, Payload: data}
	}
	if _, err := r.Handle(ctx, aggregates.OrderAggregate, orderID, mk(aggregates.CmdPlaceOrder, map[string]string{"customerRef": "c1"})); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Handle(ctx, aggregates.OrderAggregate, orderID, mk(aggregates.CmdAddOrderLine, aggregates.OrderLine{SKU: "w", Qty: 1, Price: 100})); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Handle(ctx, aggregates.OrderAggregate, orderID, mk(aggregates.CmdConfirmOrder, nil)); err != nil {
		t.Fatal(err)
	}
}

func TestFulfillmentDispatchesTask(t *testing.T) {
	store, r := setup(t)
	ctx := context.Background()

	placeAndConfirmOrder(t, r, "o1")

	// feed the order stream through the fulfillment reactor
	stream, err := store.LoadStream(ctx, aggregates.OrderAggregate, "o1")
	if err != nil {
		t.Fatal(err)
	}
	c := AsConsumer(Fulfillment(), r, nil)
	for _, ev := range stream {
		if err := c.Apply(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}

	tasks, err := store.LoadStream(ctx, aggregates.TaskAggregate, "fulfill-o1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Type != aggregates.TaskCreated {
		t.Fatalf("expected 1 TaskCreated, got %+v", tasks)
	}

	// provenance metadata is stamped
	var meta map[string]any
	if err := json.Unmarshal(tasks[0].Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["actor"] != "reactor:fulfillment" {
		t.Fatalf("unexpected actor: %v", meta)
	}
	if meta["causationId"] != stream[2].ID { // the OrderConfirmed event
		t.Fatalf("causationId = %v, want %v", meta["causationId"], stream[2].ID)
	}
	if meta["correlationId"] == "" {
		t.Fatal("correlationId missing")
	}
}

func TestFulfillmentReplayIsIdempotent(t *testing.T) {
	store, r := setup(t)
	ctx := context.Background()

	placeAndConfirmOrder(t, r, "o1")
	stream, _ := store.LoadStream(ctx, aggregates.OrderAggregate, "o1")
	confirmed := stream[2]

	c := AsConsumer(Fulfillment(), r, nil)
	if err := c.Apply(ctx, confirmed); err != nil {
		t.Fatal(err)
	}
	// replay: domain rejection ("task already exists") is logged and skipped
	if err := c.Apply(ctx, confirmed); err != nil {
		t.Fatal(err)
	}

	tasks, _ := store.LoadStream(ctx, aggregates.TaskAggregate, "fulfill-o1")
	if len(tasks) != 1 {
		t.Fatalf("replay duplicated the task: %+v", tasks)
	}
}

func TestFulfillmentIgnoresUnrelatedEvents(t *testing.T) {
	store, r := setup(t)
	ctx := context.Background()

	placeAndConfirmOrder(t, r, "o1")
	stream, _ := store.LoadStream(ctx, aggregates.OrderAggregate, "o1")

	c := AsConsumer(Fulfillment(), r, nil)
	for _, ev := range stream[:2] { // placed + line added only
		if err := c.Apply(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}

	tasks, _ := store.LoadStream(ctx, aggregates.TaskAggregate, "fulfill-o1")
	if len(tasks) != 0 {
		t.Fatalf("no task expected before confirmation, got %+v", tasks)
	}
}

func TestCorrelationIDInherited(t *testing.T) {
	ev := events.Event{ID: "child", Metadata: json.RawMessage(`{"correlationId":"root"}`)}
	if got := correlationID(ev); got != "root" {
		t.Fatalf("expected inherited correlation, got %q", got)
	}
	ev = events.Event{ID: "root", Metadata: json.RawMessage(`{}`)}
	if got := correlationID(ev); got != "root" {
		t.Fatalf("expected self as root, got %q", got)
	}
}
