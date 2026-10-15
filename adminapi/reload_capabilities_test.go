package adminapi

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/functions"
	"github.com/jamestryand/pocketcqrs/outbound"
	"github.com/jamestryand/pocketcqrs/outbox"
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
	s := &State{Outbound: testOutboundClient(t)}

	fresh := functions.NewGojaRuntime(nil)
	s.CarryCapabilities(fresh)

	if fresh.Outbound() == nil {
		t.Fatal("$http was not carried onto the fresh runtime: it would work until the first hot reload and then silently vanish")
	}
}

// The other half of the same trap. State.Outbound is a *outbound.Client, and
// a nil pointer stored in an interface is NOT a nil interface — so copying it
// unconditionally would install the binding on an instance that never enabled
// outbound, and $http would exist and panic on first use instead of being
// absent.
func TestReloadDoesNotInstallOutboundWhenItWasNeverEnabled(t *testing.T) {
	s := &State{} // --cqrsAllowOutboundHTTP absent

	fresh := functions.NewGojaRuntime(nil)
	s.CarryCapabilities(fresh)

	if fresh.Outbound() != nil {
		t.Fatal("a nil *outbound.Client was installed as a non-nil interface; $http would exist and panic rather than be absent")
	}
}

// The effect outbox is carried onto a reloaded runtime too, or effect
// functions would quietly go back to running on the engine's pass after
// the first reload; and the worker's handler runs the CURRENT runtime's
// code, whatever a reload has swapped in.
func TestReloadCarriesTheEffectOutbox(t *testing.T) {
	box, err := outbox.Open(filepath.Join(t.TempDir(), "outbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	s := &State{Outbox: box}
	fresh := functions.NewGojaRuntime(nil)
	s.CarryCapabilities(fresh)
	if fresh.Outbox() != box {
		t.Fatal("the outbox wasn't carried onto the fresh runtime")
	}

	var ran []string
	old := functions.NewGojaRuntime(func(msg string, args ...any) { ran = append(ran, "old:"+msg) })
	current := functions.NewGojaRuntime(func(msg string, args ...any) { ran = append(ran, "current:"+msg) })
	s.FnRuntime = old
	s.FnRuntime = current // a reload
	if out := s.RunQueuedEffect(context.Background(), outbox.Entry{ID: "x", Consumer: functions.EffectConsumer,
		Payload: json.RawMessage(`{"function":"gone.js","event":{}}`)}); !out.Done {
		t.Fatalf("outcome %+v", out)
	}
	if len(ran) != 1 || !strings.HasPrefix(ran[0], "current:") {
		t.Fatalf("not the current runtime that ran the work: %v", ran)
	}
}
