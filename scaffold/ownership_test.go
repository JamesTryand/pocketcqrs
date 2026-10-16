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

// loanDomain is state-ownership.json's shape: a loan changed only by the
// member who requested it, whom its own state names (schema 3.15.0).
func loanDomain() Domain {
	return Domain{
		Aggregate: "loan",
		Commands: []Command{
			{Name: "RequestLoan", Once: true, Fields: []Field{{Name: "memberId", Type: "text"}},
				Events: []Event{{Name: "LoanRequested", Fields: []Field{{Name: "memberId", Type: "text"}}}}},
			{Name: "ChangeLoan", RequiresExisting: true, Fields: []Field{{Name: "days", Type: "number"}},
				OwnedBy: &Ownership{StateField: "memberId", Message: "only the member who requested it may change it"},
				Events:  []Event{{Name: "LoanChanged", Fields: []Field{{Name: "days", Type: "number"}}}}},
		},
	}
}

const ownershipBehaviour = `package loan

import (
	"encoding/json"
	"testing"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

func TestOwnership(t *testing.T) {
	d := Loan()
	s, err := d.Evolve(d.InitialState(), events.Event{Type: LoanRequested, Data: json.RawMessage(` + "`" + `{"memberId":"m1"}` + "`" + `)})
	if err != nil {
		t.Fatal(err)
	}
	update := decider.Command{Name: "ChangeLoan", Payload: json.RawMessage(` + "`" + `{"days":3}` + "`" + `)}
	update.Actor = "m1"
	if _, err := d.Decide(update, s); err != nil {
		t.Fatalf("the owner may: %v", err)
	}
	update.Actor = "m2"
	_, err = d.Decide(update, s)
	if !decider.IsForbidden(err) || err.Error() != "only the member who requested it may change it" {
		t.Fatalf("anyone else is forbidden, with the message: %v", err)
	}
}
`

// The Go target: the owner check runs after the existence check, and
// refuses anyone else with decider.Forbidden (403 at the gateway).
func TestGoOwnershipRuns(t *testing.T) {
	runGeneratedGo(t, loanDomain(), ownershipBehaviour)
}

// The JS target: the generated check throws an Error named Forbidden, which
// the runtime turns into decider.Forbidden.
func TestJSOwnershipRuns(t *testing.T) {
	files, err := loanDomain().Generate()
	if err != nil {
		t.Fatal(err)
	}
	rt := functions.NewGojaRuntime(nil)
	spec, err := functions.LoadDeciderSource(rt, "loan.js", files[0].Source)
	if err != nil {
		t.Fatal(err)
	}
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registry := decider.NewRegistry(store)
	registry.RegisterUntyped("loan", spec.UntypedDecider())
	ctx := context.Background()
	if _, err := registry.HandleWithMeta(ctx, "loan", "l1", decider.Command{Name: "RequestLoan", Payload: json.RawMessage(`{"memberId":"m1"}`)}, map[string]any{"actor": "m1"}); err != nil {
		t.Fatal(err)
	}
	update := decider.Command{Name: "ChangeLoan", Payload: json.RawMessage(`{"days":3}`)}
	if _, err := registry.HandleWithMeta(ctx, "loan", "l1", update, map[string]any{"actor": "m1"}); err != nil {
		t.Fatalf("the owner may: %v", err)
	}
	_, err = registry.HandleWithMeta(ctx, "loan", "l1", update, map[string]any{"actor": "m2"})
	if !decider.IsForbidden(err) || err.Error() != "only the member who requested it may change it" {
		t.Fatalf("anyone else is forbidden, with the message: %v", err)
	}
}

// The owner field must be a text field of the aggregate's state.
func TestOwnerFieldIsChecked(t *testing.T) {
	for _, field := range []string{"ghost", "days"} {
		d := loanDomain()
		d.Commands[1].OwnedBy.StateField = field
		if err := d.Validate(); err == nil {
			t.Errorf("owner field %q accepted", field)
		}
	}
}
