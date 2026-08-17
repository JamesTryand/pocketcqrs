package functions

import (
	"strings"
	"testing"
)

// fakeTarget is a stand-in for the decider registry: which aggregates exist,
// and what each DECLARES. A present-but-nil entry is an aggregate that
// declares no commands, which is a distinct case from being absent.
type fakeTarget map[string][]string

func (f fakeTarget) Has(aggregate string) bool {
	_, ok := f[aggregate]
	return ok
}

func (f fakeTarget) Commands(aggregate string) []string { return f[aggregate] }

// The tutorial's own reproduction of F-2, as a test.
//
// docs/tutorial.md deliberately collides a JS `order` decider with the
// built-in Go one, so the decider is refused and the Go `order` survives —
// whose commands are PlaceOrder/AddOrderLine/ConfirmOrder/CancelOrder. The
// reactor from that same refused slice dispatches ShipOrder, which no longer
// exists anywhere. Before this gate it loaded happily and dropped every
// reaction at INFO with the checkpoint advancing past them.
func TestReactorGateRefusesACommandTheTargetDoesNotAccept(t *testing.T) {
	target := fakeTarget{"order": {"PlaceOrder", "AddOrderLine", "ConfirmOrder", "CancelOrder"}}
	spec := &ReactorSpec{Reactor: "autoShipPendingOrders", Dispatches: []string{"order/ShipOrder"}}

	err := ValidateReactorSpec(target, spec)
	if err == nil {
		t.Fatal("a reactor dispatching a command the target does not accept must be refused at load")
	}
	// the message has to name the reactor, the claim, and what is actually on
	// offer — otherwise the operator has to go and look the last one up
	for _, want := range []string{"autoShipPendingOrders", "order/ShipOrder", "PlaceOrder"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q, got: %v", want, err)
		}
	}
}

func TestReactorGateAcceptsADeclaredCommand(t *testing.T) {
	target := fakeTarget{"order": {"PlaceOrder", "ShipOrder"}}
	spec := &ReactorSpec{Reactor: "autoShip", Dispatches: []string{"order/ShipOrder"}}

	if err := ValidateReactorSpec(target, spec); err != nil {
		t.Fatalf("a declared command must pass: %v", err)
	}
}

// The third outcome, and the one most likely to be "tidied" into a refusal by
// someone reading the gate as a completeness check.
//
// decider.Decider.Commands is documentation, not enforcement. An aggregate
// that declares nothing cannot contradict anything, so its reactors are
// UNVERIFIABLE, not invalid. Refusing here would break every reactor pointing
// at a hand-written Go aggregate that never declared its commands.
func TestReactorGateAcceptsAnUndeclaringTarget(t *testing.T) {
	target := fakeTarget{"legacy": nil} // exists, declares nothing
	spec := &ReactorSpec{Reactor: "notify", Dispatches: []string{"legacy/DoAnything"}}

	if err := ValidateReactorSpec(target, spec); err != nil {
		t.Fatalf("an undeclaring target is unverifiable, not invalid: %v", err)
	}
}

// <aggregate>/<Command> has two halves, and a missing aggregate is a different
// operator error from a command typo. One message for both costs a debugging
// round.
func TestReactorGateDistinguishesAMissingAggregate(t *testing.T) {
	target := fakeTarget{"order": {"PlaceOrder"}}
	spec := &ReactorSpec{Reactor: "notify", Dispatches: []string{"shipment/Notify"}}

	err := ValidateReactorSpec(target, spec)
	if err == nil {
		t.Fatal("dispatching at an aggregate with no decider must be refused")
	}
	if !strings.Contains(err.Error(), "no decider is registered") {
		t.Errorf("a missing aggregate should say so rather than talk about commands, got: %v", err)
	}
}

// A reactor that declares nothing is not making a claim, so there is nothing
// to check. Reactors predate //@dispatches and it is optional.
func TestReactorGateIgnoresAReactorThatDeclaresNothing(t *testing.T) {
	spec := &ReactorSpec{Reactor: "silent"}
	if err := ValidateReactorSpec(fakeTarget{}, spec); err != nil {
		t.Fatalf("no //@dispatches means no claim to check: %v", err)
	}
}

// A nil target disables the gate rather than failing everything: dry-run paths
// and tests must not be forced to invent a registry.
func TestReactorGateIsSkippedWithoutATarget(t *testing.T) {
	spec := &ReactorSpec{Reactor: "any", Dispatches: []string{"nowhere/Nothing"}}
	if err := ValidateReactorSpec(nil, spec); err != nil {
		t.Fatalf("a nil target must skip the gate, not fail it: %v", err)
	}
}
