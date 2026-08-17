package main

import (
	"path/filepath"
	"testing"

	"github.com/jamestryand/pocketcqrs/aggregates"
	"github.com/jamestryand/pocketcqrs/decider"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/functions"
)

// liveTarget stands in for the running decider registry.
type liveTarget map[string][]string

func (l liveTarget) Has(aggregate string) bool  { _, ok := l[aggregate]; return ok }
func (l liveTarget) Commands(a string) []string { return l[a] }

// prospectiveFor builds the overlay the way reloadFunctions does, without
// standing up an app. c.registry is an interface field here only in the test's
// imagination — so the maintenance branch is exercised through prospectiveSet
// directly, which is the part with the logic in it.
func prospectiveFor(live functions.CommandTarget, loaded []*functions.DeciderSpec, liveJS map[string]bool) functions.CommandTarget {
	p := &prospectiveSet{live: live, adds: map[string][]string{}, removes: map[string]bool{}}
	for aggregate := range liveJS {
		p.removes[aggregate] = true
	}
	for _, spec := range loaded {
		delete(p.removes, spec.Aggregate)
		p.adds[spec.Aggregate] = spec.Commands
	}
	return p
}

// THE ordering test. Reactors install before deciders (reload.go), so a gate
// checking the LIVE registry would refuse a reactor whose target decider is
// arriving in the same maintenance reload — making the gate worse than the
// fault it fixes.
func TestGateAcceptsADeciderAndItsReactorInTheSameReload(t *testing.T) {
	live := liveTarget{} // nothing registered yet — brand new slice
	loaded := []*functions.DeciderSpec{
		{Aggregate: "shipment", Commands: []string{"NotifyShippingPartner"}},
	}

	target := prospectiveFor(live, loaded, nil)
	spec := &functions.ReactorSpec{
		Reactor:    "notifyPartner",
		Dispatches: []string{"shipment/NotifyShippingPartner"},
	}

	if err := functions.ValidateReactorSpec(target, spec); err != nil {
		t.Fatalf("a decider and its reactor shipped together must both load: %v", err)
	}
}

// The overlay must also see removals: a reactor pointing at a decider whose
// file was deleted in this reload is refused rather than left dispatching into
// a hole.
func TestGateRefusesAReactorWhoseTargetIsBeingRemoved(t *testing.T) {
	live := liveTarget{"shipment": {"NotifyShippingPartner"}}
	target := prospectiveFor(live, nil, map[string]bool{"shipment": true})

	spec := &functions.ReactorSpec{
		Reactor:    "notifyPartner",
		Dispatches: []string{"shipment/NotifyShippingPartner"},
	}
	if err := functions.ValidateReactorSpec(target, spec); err == nil {
		t.Fatal("a reactor whose target decider is being removed must be refused")
	}
}

// A built-in Go aggregate is in neither adds nor removes — it cannot change
// without a rebuild — so it must fall through to the live registry rather than
// vanishing from the prospective set.
func TestProspectiveSetFallsThroughToBuiltIns(t *testing.T) {
	live := liveTarget{"order": {"PlaceOrder"}}
	target := prospectiveFor(live, []*functions.DeciderSpec{
		{Aggregate: "shipment", Commands: []string{"NotifyShippingPartner"}},
	}, nil)

	if !target.Has("order") {
		t.Fatal("a built-in aggregate disappeared from the prospective set")
	}
	spec := &functions.ReactorSpec{Reactor: "r", Dispatches: []string{"order/PlaceOrder"}}
	if err := functions.ValidateReactorSpec(target, spec); err != nil {
		t.Fatalf("dispatching at a built-in must still pass: %v", err)
	}
}

// In running mode deciders do not reload at all, so the live registry already
// IS the post-reload truth and no overlay should be built. Guards against a
// future edit that applies the maintenance overlay unconditionally and starts
// refusing reactors against deciders that were never going to change.
func TestRunningModeValidatesAgainstTheLiveRegistry(t *testing.T) {
	c := &components{}
	got := c.prospectiveCommands(events.ModeRunning, &functions.LoadResult{
		Deciders: []*functions.DeciderSpec{{Aggregate: "ignored", Commands: []string{"Nope"}}},
	})
	if _, overlaid := got.(*prospectiveSet); overlaid {
		t.Fatal("running mode must validate against the live registry: deciders are not reloading")
	}
}

// The tutorial documents an exact refusal message, so assert it against the
// REAL built-in order aggregate rather than a fake — a doc that quotes output
// is a deliverable, and this is the line that would rot silently if the Go
// aggregate's command list ever changed.
func TestTutorialRefusalMessageMatchesTheDocumentedOne(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registry := decider.NewRegistry(store)
	aggregates.RegisterAll(registry)

	spec := &functions.ReactorSpec{
		Reactor:    "autoShipPendingOrders",
		Dispatches: []string{"order/ShipOrder"},
	}
	err = functions.ValidateReactorSpec(registry, spec)
	if err == nil {
		t.Fatal("the tutorial's reactor must be refused against the built-in order aggregate")
	}

	const documented = `validation: reactor autoShipPendingOrders dispatches order/ShipOrder, ` +
		`but aggregate "order" accepts only [AddOrderLine CancelOrder ConfirmOrder PlaceOrder]`
	if err.Error() != documented {
		t.Fatalf("docs/tutorial.md quotes this message verbatim; update both together.\n got: %s\nwant: %s",
			err.Error(), documented)
	}
}
