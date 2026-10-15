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
	spec, err := functions.LoadDeciderSource(functions.NewGojaRuntime(nil), "entry.js", files[0].Source)
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
		if _, err := store.Append(ctx, "entry", stream, 0, evs); err != nil {
			t.Fatal(err)
		}
	}
	logged := events.NewEvent{Type: "EntryLogged", Data: json.RawMessage(`{"hours":8,"tags":["night"]}`)}
	claimed := events.NewEvent{Type: "EntryClaimed", Data: json.RawMessage(`{"claimedFor":"inv-1"}`)}
	appendTo("open", logged)
	appendTo("onInvoice", logged, claimed)

	for _, c := range []struct{ stream, name, payload, want string }{
		{"open", "UpdateEntry", `{"hours":7}`, ""},
		{"onInvoice", "UpdateEntry", `{"hours":7}`, "entry is on an invoice"},
		{"open", "UpdateEntry", `{"hours":0}`, "hours must be positive"},
		{"open", "UpdateEntry", `{"hours":-2}`, "hours must be positive"},
		{"open", "UpdateEntry", `{"hours":25}`, "a day has at most 24 hours"},
		{"open", "UpdateEntry", `{"hours":9}`, "not more than logged"},
		{"open", "UpdateEntry", `{}`, ""},
		{"open", "UpdateEntry", `{"hours":1,"invoiceId":"frozen"}`, "invoice is frozen"},
		{"open", "UpdateEntry", `{"hours":1,"tag":"night"}`, "already tagged"},
		{"open", "UpdateEntry", `{"hours":1,"tag":"day"}`, ""},
		{"open", "ClaimEntry", `{"invoiceId":"inv-2"}`, ""},
		{"onInvoice", "ClaimEntry", `{"invoiceId":"inv-1"}`, ""},
		{"onInvoice", "ClaimEntry", `{"invoiceId":"inv-2"}`, "entry is on another invoice"},
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
