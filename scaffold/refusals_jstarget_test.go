package scaffold

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/functions"
)

// The JS target's refusals, run in the real decider runtime against the same
// cases as the Go target (TestGoRefusalsRun): one rule semantics for both.
func TestJSRefusalsRun(t *testing.T) {
	files, err := refusalDomain().Generate()
	if err != nil {
		t.Fatal(err)
	}
	spec, err := functions.LoadDeciderSource(functions.NewGojaRuntime(nil), "booking.js", files[0].Source)
	if err != nil {
		t.Fatalf("%v\n%s", err, files[0].Source)
	}
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	appendTo := func(stream string, evs ...events.NewEvent) {
		if _, err := store.Append(ctx, "booking", stream, 0, evs); err != nil {
			t.Fatal(err)
		}
	}
	logged := events.NewEvent{Type: "BookingMade", Data: json.RawMessage(`{"duration":8,"tags":["night"]}`)}
	claimed := events.NewEvent{Type: "BookingHeld", Data: json.RawMessage(`{"heldFor":"plan-1"}`)}
	appendTo("open", logged)
	appendTo("held", logged, claimed)

	for _, c := range []struct{ stream, name, payload, want string }{
		{"open", "ChangeBooking", `{"duration":7}`, ""},
		{"held", "ChangeBooking", `{"duration":7}`, "booking is held by an event plan"},
		{"open", "ChangeBooking", `{"duration":0}`, "duration must be positive"},
		{"open", "ChangeBooking", `{"duration":-2}`, "duration must be positive"},
		{"open", "ChangeBooking", `{"duration":25}`, "a booking lasts at most one day"},
		{"open", "ChangeBooking", `{"duration":9}`, "not more than booked"},
		{"open", "ChangeBooking", `{}`, ""},
		{"open", "ChangeBooking", `{"duration":1,"planId":"frozen"}`, "event plan is frozen"},
		{"open", "ChangeBooking", `{"duration":1,"tag":"night"}`, "already tagged"},
		{"open", "ChangeBooking", `{"duration":1,"tag":"day"}`, ""},
		{"open", "HoldBooking", `{"planId":"plan-2"}`, ""},
		{"held", "HoldBooking", `{"planId":"plan-1"}`, ""},
		{"held", "HoldBooking", `{"planId":"plan-2"}`, "booking is held by another event plan"},
	} {
		run, err := functions.DryRunDecide(store, spec, c.stream,
			decider.Command{Name: c.name, Payload: json.RawMessage(c.payload)}, map[string]any{"now": "t", "actor": "a"})
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if run.Rejected {
			got = run.Message
		}
		if got != c.want {
			t.Errorf("%s %s %s: got %q, want %q", c.stream, c.name, c.payload, got, c.want)
		}
	}
}
