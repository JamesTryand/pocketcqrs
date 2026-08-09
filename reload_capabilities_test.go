package main

import (
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/functions"
	"github.com/jamestryand/pocketcqrs/outbound"
)

func testOutboundClient(t *testing.T) *outbound.Client {
	t.Helper()
	c, err := outbound.New(outbound.Config{
		AllowedHosts: []string{"api.example.com"},
		Timeout:      time.Second,
		MaxInFlight:  1,
		MaxBodyBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A hot reload builds a FRESH runtime, so every capability has to be copied
// onto it. This is the one bug in the outbound work that was found by reading
// rather than by a failing test, and its failure mode is the nastiest kind:
// $http works, and then stops working after the first reload, with nothing in
// the reload report to say why.
func TestReloadCarriesOutboundOntoTheFreshRuntime(t *testing.T) {
	c := &components{outbound: testOutboundClient(t)}

	fresh := functions.NewGojaRuntime(nil)
	c.carryCapabilities(fresh)

	if fresh.Outbound() == nil {
		t.Fatal("$http was not carried onto the fresh runtime: it would work until the first hot reload and then silently vanish")
	}
}

// The other half of the same trap. c.outbound is a *outbound.Client, and a
// nil pointer stored in an interface is NOT a nil interface — so copying it
// unconditionally would install the binding on an instance that never enabled
// outbound, and $http would exist and panic on first use instead of being
// absent.
func TestReloadDoesNotInstallOutboundWhenItWasNeverEnabled(t *testing.T) {
	c := &components{} // --cqrsAllowOutboundHTTP absent

	fresh := functions.NewGojaRuntime(nil)
	c.carryCapabilities(fresh)

	if fresh.Outbound() != nil {
		t.Fatal("a nil *outbound.Client was installed as a non-nil interface; $http would exist and panic rather than be absent")
	}
}
