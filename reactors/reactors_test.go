package reactors

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jamestryand/pocketcqrs/aggregates"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
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
	c := AsConsumer(Fulfillment(), r, nil, nil)
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
	if _, ok := meta["provenance"]; ok {
		t.Fatalf("unexpected provenance key on a purely local reaction: %v", meta)
	}
}

// TestFulfillmentInheritsProvenance proves reactors.Dispatch propagates a
// causing event's provenance onto its reaction, the mechanism the federation
// trust model item in NEEDS.md needs: a local reactor reacting to an event
// that itself carries provenance (e.g. once a federation-ingest write sets
// it) must not silently drop that signal just because the reactor itself has
// no idea federation exists.
func TestFulfillmentInheritsProvenance(t *testing.T) {
	store, r := setup(t)
	ctx := context.Background()
	mk := func(name string, payload any) decider.Command {
		data, _ := json.Marshal(payload)
		return decider.Command{Name: name, Payload: data}
	}

	if _, err := r.Handle(ctx, aggregates.OrderAggregate, "o1", mk(aggregates.CmdPlaceOrder, map[string]string{"customerRef": "c1"})); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Handle(ctx, aggregates.OrderAggregate, "o1", mk(aggregates.CmdAddOrderLine, aggregates.OrderLine{SKU: "w", Qty: 1, Price: 100})); err != nil {
		t.Fatal(err)
	}
	// stand in for a future federation-ingest write: the causing event
	// itself carries provenance, as ordinary caller-supplied meta.
	if _, err := r.HandleWithMeta(ctx, aggregates.OrderAggregate, "o1", mk(aggregates.CmdConfirmOrder, nil),
		map[string]any{"provenance": "federated:peer1"}); err != nil {
		t.Fatal(err)
	}

	stream, err := store.LoadStream(ctx, aggregates.OrderAggregate, "o1")
	if err != nil {
		t.Fatal(err)
	}
	c := AsConsumer(Fulfillment(), r, nil, nil)
	for _, ev := range stream {
		if err := c.Apply(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}

	tasks, err := store.LoadStream(ctx, aggregates.TaskAggregate, "fulfill-o1")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %+v", tasks)
	}
	var meta map[string]any
	if err := json.Unmarshal(tasks[0].Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["actor"] != "reactor:fulfillment" {
		t.Fatalf("actor changed unexpectedly: %v", meta)
	}
	if meta["provenance"] != "federated:peer1" {
		t.Fatalf("provenance not inherited: %v", meta)
	}
}

func TestFulfillmentReplayIsIdempotent(t *testing.T) {
	store, r := setup(t)
	ctx := context.Background()

	placeAndConfirmOrder(t, r, "o1")
	stream, _ := store.LoadStream(ctx, aggregates.OrderAggregate, "o1")
	confirmed := stream[2]

	c := AsConsumer(Fulfillment(), r, nil, nil)
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

	c := AsConsumer(Fulfillment(), r, nil, nil)
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

func TestCauseProvenanceInherited(t *testing.T) {
	ev := events.Event{Metadata: json.RawMessage(`{"provenance":"federated:peer1"}`)}
	if got := causeProvenance(ev); got != "federated:peer1" {
		t.Fatalf("expected inherited provenance, got %q", got)
	}
	// no meaningful self-value: an event with nothing to claim has none,
	// unlike correlationID which falls back to being its own root.
	ev = events.Event{ID: "root", Metadata: json.RawMessage(`{}`)}
	if got := causeProvenance(ev); got != "" {
		t.Fatalf("expected empty provenance, got %q", got)
	}
}

// fakeDispatcher lets the retry-vs-continue split be exercised directly.
type fakeDispatcher struct {
	err   error
	calls int
}

func (f *fakeDispatcher) HandleWithMeta(ctx context.Context, aggregate, id string, cmd decider.Command, meta map[string]any) ([]events.Event, error) {
	f.calls++
	return nil, f.err
}

// TestDispatchRetriesOnConcurrencyAndContinuesOnRejection pins the two halves
// of the reaction rule, which are shared by the Go and JS reactor tiers.
//
// They must go OPPOSITE ways: a concurrency conflict means the target stream
// moved and the whole event should be retried, so the error propagates and
// the consumer does not advance. A domain rejection is the ordinary
// idempotency path ("already exists") and must NOT block the log, so it is
// logged and the consumer moves on. Getting these the wrong way round either
// wedges the log forever or silently drops reactions.
func TestDispatchRetriesOnConcurrencyAndContinuesOnRejection(t *testing.T) {
	ctx := context.Background()
	trigger := events.Event{ID: "e1", Type: "OrderConfirmed"}
	reactions := []Reaction{
		{Aggregate: "task", ID: "t1", Command: decider.Command{Name: "CreateTask"}},
		{Aggregate: "task", ID: "t2", Command: decider.Command{Name: "CreateTask"}},
	}

	// a concurrency conflict stops the batch so the event is retried whole
	conflict := &fakeDispatcher{err: events.ErrConcurrency}
	if err := Dispatch(ctx, conflict, "fulfillment", trigger, reactions, nil, nil); err == nil {
		t.Fatal("a concurrency conflict must propagate so the event is retried")
	}
	if conflict.calls != 1 {
		t.Errorf("the batch should stop at the conflict, got %d calls", conflict.calls)
	}

	// a domain rejection is logged and the rest still go out
	rejected := &fakeDispatcher{err: errors.New("task already exists")}
	if err := Dispatch(ctx, rejected, "fulfillment", trigger, reactions, nil, nil); err != nil {
		t.Fatalf("a domain rejection must not block the log: %v", err)
	}
	if rejected.calls != 2 {
		t.Errorf("every reaction should be attempted, got %d calls", rejected.calls)
	}
}
