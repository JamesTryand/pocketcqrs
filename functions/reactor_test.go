package functions

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/reactors"
)

const shipReactorJS = `//@trigger reactor OrderPlaced
//@dispatches shipment/StartShipment

function reactTo(event) {
  return [{
    aggregate: 'shipment',
    id: 'ship-' + event.data.orderId,
    command: 'StartShipment',
    payload: { orderId: event.data.orderId }
  }];
}
`

// twoTypeLog seeds a log with TWO event types on purpose: a fixture with one
// type cannot fail a trigger-filter test, so it proves nothing about whether
// the reactor actually filters.
func twoTypeLog(t *testing.T) *events.Store {
	t.Helper()
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if _, err := store.Append(ctx, "order", "o1", 0, []events.NewEvent{
		{Type: "OrderPlaced", Data: json.RawMessage(`{"orderId":"o1"}`)},
		{Type: "OrderNoted", Data: json.RawMessage(`{"note":"ignore me"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestReactorDirectiveParsing(t *testing.T) {
	d, err := Declares("ship.js", shipReactorJS)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != KindReactor {
		t.Fatalf("expected the reactor kind, got %q", d.Kind)
	}
	if d.SchemaBearing {
		t.Error("a reactor declares no schema, so it must reload in running mode")
	}
	if len(d.Reactor) != 1 || d.Reactor[0] != "OrderPlaced" {
		t.Errorf("triggers not reported: %+v", d.Reactor)
	}
	if len(d.Dispatches) != 1 || d.Dispatches[0] != "shipment/StartShipment" {
		t.Errorf("declared dispatches not reported: %+v", d.Dispatches)
	}

	// an empty trigger list is refused rather than loading as a reactor
	// that can never fire
	if _, err := Declares("bad.js", "//@trigger react\n"); err == nil {
		t.Error("//@trigger reactor with no event types must be refused")
	}

	// a malformed //@dispatches is REFUSED, not dropped: it is the only
	// declared record of what the reactor sends
	for _, bad := range []string{"//@trigger reactor X\n//@dispatches nostroke\n",
		"//@trigger reactor X\n//@dispatches /Command\n",
		"//@trigger reactor X\n//@dispatches agg/\n"} {
		if _, err := Declares("bad.js", bad); err == nil {
			t.Errorf("a malformed //@dispatches must be refused: %q", bad)
		}
	}

	// //@dispatches without a react trigger is a mistake worth naming
	if _, err := Declares("bad.js", "//@trigger event X\n//@dispatches a/B\n"); err == nil {
		t.Error("//@dispatches outside a reactor must be refused")
	}
}

// TestReactorIsSinglePurpose: react gets its own bucket rather than joining
// event/http/cron. One file declaring both is two delivery paths over one
// event, with two checkpoints and no sensible reading.
func TestReactorIsSinglePurpose(t *testing.T) {
	for name, src := range map[string]string{
		"react + event":      "//@trigger reactor X\n//@trigger event X\n",
		"react + http":       "//@trigger reactor X\n//@trigger http\n",
		"react + cron":       "//@trigger reactor X\n//@trigger cron * * * * *\n",
		"react + projection": "//@trigger reactor X\n//@trigger projection p on X\n//@schema ps a:number\n//@key a\n",
		"react + decider":    "//@trigger reactor X\n//@trigger decider a\n//@handles X\n",
	} {
		if _, err := Declares("mixed.js", src); err == nil || !strings.Contains(err.Error(), "single-purpose") {
			t.Errorf("%s must be refused as not single-purpose, got %v", name, err)
		}
	}
}

// TestReactorCheckpointKeyDoesNotCollide is the concrete bug this tier could
// have shipped with. Consumer.Name() IS the durable checkpoint key, and Go
// reactors already use "reactor:<name>" — a JS reactor sharing that prefix
// would share the checkpoint, silently skipping events for both.
func TestReactorCheckpointKeyDoesNotCollide(t *testing.T) {
	rt := NewGojaRuntime(nil)
	spec, err := LoadReactorSource(rt, "fulfillment.js", shipReactorJS)
	if err != nil {
		t.Fatal(err)
	}
	goKey := reactors.AsConsumer(reactors.Fulfillment(), nil, nil).Name()

	if spec.Name() == goKey {
		t.Fatalf("JS and Go reactors share the checkpoint key %q", spec.Name())
	}
	if !strings.HasPrefix(spec.Name(), "fn-reactor:") {
		t.Errorf("expected the fn-reactor: prefix, got %q", spec.Name())
	}
	// ...but the metadata ACTOR must still match, because the catalog's flow
	// detection joins on `actor LIKE 'reactor:%'`. Two decisions that look
	// like one; this asserts they went opposite ways on purpose.
	if !strings.HasPrefix(goKey, "reactor:") {
		t.Fatalf("the Go reactor prefix moved; this test's premise is stale: %q", goKey)
	}
}

func TestReactorDispatchesThroughRegistry(t *testing.T) {
	store := twoTypeLog(t)
	ctx := context.Background()

	registry := decider.NewRegistry(store)
	registry.RegisterUntyped("shipment", recordingDecider())

	rt := NewGojaRuntime(nil)
	rt.SetRegistry(registry)
	spec, err := LoadReactorSource(rt, "ship.js", shipReactorJS)
	if err != nil {
		t.Fatal(err)
	}

	stream, err := store.LoadStream(ctx, "order", "o1")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range stream {
		if err := spec.Apply(ctx, ev); err != nil {
			t.Fatalf("apply %s: %v", ev.Type, err)
		}
	}

	// exactly one dispatch: OrderNoted must not have triggered anything
	shipped, err := store.LoadStream(ctx, "shipment", "ship-o1")
	if err != nil {
		t.Fatal(err)
	}
	if len(shipped) != 1 {
		t.Fatalf("expected one dispatched command to land, got %d", len(shipped))
	}

	// the causation metadata is what the catalog's flow detection joins on
	var meta map[string]any
	if err := json.Unmarshal(shipped[0].Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["actor"] != "reactor:ship" {
		t.Errorf("actor must be reactor:<name> so ReactorFlows sees it, got %v", meta["actor"])
	}
	if meta["causationId"] != stream[0].ID {
		t.Errorf("causationId should point at the triggering event, got %v", meta["causationId"])
	}
	if meta["correlationId"] == nil || meta["correlationId"] == "" {
		t.Error("correlationId must be carried")
	}
}

// TestReactorWithoutRegistryFailsLoudly: quietly doing nothing would look
// exactly like a reactor whose triggers never matched.
func TestReactorWithoutRegistryFailsLoudly(t *testing.T) {
	store := twoTypeLog(t)
	ctx := context.Background()
	rt := NewGojaRuntime(nil)
	spec, err := LoadReactorSource(rt, "ship.js", shipReactorJS)
	if err != nil {
		t.Fatal(err)
	}
	stream, _ := store.LoadStream(ctx, "order", "o1")
	if err := spec.Apply(ctx, stream[0]); err == nil {
		t.Fatal("a reactor with no registry installed must fail loudly")
	}
}

// TestReactorNonDispatchesAreCounted: a reactor returning plain objects would
// dispatch NOTHING, forever, while the log shows a consumer keeping up
// perfectly — the projection-returning-non-ops defect in its new home.
func TestReactorNonDispatchesAreCounted(t *testing.T) {
	_, ignored, err := normalizeDispatches([]any{
		map[string]any{"note": "not a dispatch"},
		"a bare string",
		42,
	})
	if err != nil {
		t.Fatalf("stray values should be counted, not fatal: %v", err)
	}
	if ignored != 3 {
		t.Fatalf("expected 3 ignored values, got %d", ignored)
	}

	// ...but a descriptor that is recognisably meant to dispatch and cannot
	// is an error: the author clearly intended to send something
	if _, _, err := normalizeDispatches([]any{
		map[string]any{"aggregate": "shipment", "command": "StartShipment"},
	}); err == nil {
		t.Error("a dispatch missing its id must fail loudly, not be counted away")
	}
}

// TestDryRunReactorDispatchesNothing: the mode reports what WOULD be sent.
func TestDryRunReactorDispatchesNothing(t *testing.T) {
	store := twoTypeLog(t)
	ctx := context.Background()

	rt := NewGojaRuntime(nil)
	spec, err := LoadReactorSource(rt, "ship.js", shipReactorJS)
	if err != nil {
		t.Fatal(err)
	}
	res, err := DryRunReactor(store, spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 1 {
		t.Errorf("only the matching event type should be replayed, got %d", res.Events)
	}
	if len(res.Dispatches) != 1 {
		t.Fatalf("expected one previewed dispatch, got %+v", res.Dispatches)
	}
	d := res.Dispatches[0]
	if d.Aggregate != "shipment" || d.ID != "ship-o1" || d.Command != "StartShipment" {
		t.Errorf("unexpected dispatch preview: %+v", d)
	}

	// nothing ran and nothing was written — assert on the LOG, not on the
	// response, which would be happy either way
	total, _, err := store.LogTotals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("a dry run appended to the log: %d events", total)
	}
}

// TestReactorReplayIsIdempotent: delivery is at-least-once, so the same
// event arriving twice must not produce a second command. The standard
// pattern is a deterministic target id; the replay then hits a domain
// rejection, which is logged and skipped rather than blocking the log.
func TestReactorReplayIsIdempotent(t *testing.T) {
	store := twoTypeLog(t)
	ctx := context.Background()

	registry := decider.NewRegistry(store)
	registry.RegisterUntyped("shipment", onceOnlyDecider())

	rt := NewGojaRuntime(nil)
	rt.SetRegistry(registry)
	spec, err := LoadReactorSource(rt, "ship.js", shipReactorJS)
	if err != nil {
		t.Fatal(err)
	}
	stream, _ := store.LoadStream(ctx, "order", "o1")

	for i := 0; i < 3; i++ {
		if err := spec.Apply(ctx, stream[0]); err != nil {
			t.Fatalf("replay %d must not block the log: %v", i, err)
		}
	}
	shipped, _ := store.LoadStream(ctx, "shipment", "ship-o1")
	if len(shipped) != 1 {
		t.Fatalf("three deliveries produced %d events; reactions must be idempotent", len(shipped))
	}
}

// ---- test deciders ----

// recordingDecider accepts anything and records it.
func recordingDecider() decider.Untyped {
	return decider.Untyped{
		Initial: func() any { return map[string]any{} },
		Evolve: func(state any, ev events.Event) (any, error) {
			return map[string]any{"started": true}, nil
		},
		Decide: func(cmd decider.Command, state any, meta map[string]any) ([]events.NewEvent, error) {
			return []events.NewEvent{{Type: "ShipmentStarted", Data: cmd.Payload}}, nil
		},
	}
}

// onceOnlyDecider refuses a repeat — the idempotency path a replayed
// reaction is supposed to land on.
func onceOnlyDecider() decider.Untyped {
	return decider.Untyped{
		Initial: func() any { return map[string]any{"exists": false} },
		Evolve: func(state any, ev events.Event) (any, error) {
			return map[string]any{"exists": true}, nil
		},
		Decide: func(cmd decider.Command, state any, meta map[string]any) ([]events.NewEvent, error) {
			if s, _ := state.(map[string]any); s != nil && s["exists"] == true {
				return nil, errors.New("shipment already exists")
			}
			return []events.NewEvent{{Type: "ShipmentStarted", Data: cmd.Payload}}, nil
		},
	}
}
