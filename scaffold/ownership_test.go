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

// expenseDomain is state-ownership.json's: an expense changed only by its
// own staff member, whom its own state names (schema 3.15.0).
func expenseDomain() Domain {
	return Domain{
		Aggregate: "expense",
		Commands: []Command{
			{Name: "LogExpense", Once: true, Fields: []Field{{Name: "staffId", Type: "text"}},
				Events: []Event{{Name: "ExpenseLogged", Fields: []Field{{Name: "staffId", Type: "text"}}}}},
			{Name: "UpdateExpense", RequiresExisting: true, Fields: []Field{{Name: "value", Type: "number"}},
				OwnedBy: &Ownership{StateField: "staffId", Message: "only its own staff member may change it"},
				Events:  []Event{{Name: "ExpenseUpdated", Fields: []Field{{Name: "value", Type: "number"}}}}},
		},
	}
}

const ownershipBehaviour = `package expense

import (
	"encoding/json"
	"testing"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

func TestOwnership(t *testing.T) {
	d := Expense()
	s, err := d.Evolve(d.InitialState(), events.Event{Type: ExpenseLogged, Data: json.RawMessage(` + "`" + `{"staffId":"s1"}` + "`" + `)})
	if err != nil {
		t.Fatal(err)
	}
	update := decider.Command{Name: "UpdateExpense", Payload: json.RawMessage(` + "`" + `{"value":3}` + "`" + `)}
	update.Actor = "s1"
	if _, err := d.Decide(update, s); err != nil {
		t.Fatalf("the owner may: %v", err)
	}
	update.Actor = "s2"
	_, err = d.Decide(update, s)
	if !decider.IsForbidden(err) || err.Error() != "only its own staff member may change it" {
		t.Fatalf("anyone else is forbidden, with the message: %v", err)
	}
}
`

// The Go target: the owner check runs after the existence check, and
// refuses anyone else with decider.Forbidden (403 at the gateway).
func TestGoOwnershipRuns(t *testing.T) {
	runGeneratedGo(t, expenseDomain(), ownershipBehaviour)
}

// The JS target: the generated check throws an Error named Forbidden, which
// the runtime turns into decider.Forbidden.
func TestJSOwnershipRuns(t *testing.T) {
	files, err := expenseDomain().Generate()
	if err != nil {
		t.Fatal(err)
	}
	rt := functions.NewGojaRuntime(nil)
	spec, err := functions.LoadDeciderSource(rt, "expense.js", files[0].Source)
	if err != nil {
		t.Fatal(err)
	}
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registry := decider.NewRegistry(store)
	registry.RegisterUntyped("expense", spec.UntypedDecider())
	ctx := context.Background()
	if _, err := registry.HandleWithMeta(ctx, "expense", "x1", decider.Command{Name: "LogExpense", Payload: json.RawMessage(`{"staffId":"s1"}`)}, map[string]any{"actor": "s1"}); err != nil {
		t.Fatal(err)
	}
	update := decider.Command{Name: "UpdateExpense", Payload: json.RawMessage(`{"value":3}`)}
	if _, err := registry.HandleWithMeta(ctx, "expense", "x1", update, map[string]any{"actor": "s1"}); err != nil {
		t.Fatalf("the owner may: %v", err)
	}
	_, err = registry.HandleWithMeta(ctx, "expense", "x1", update, map[string]any{"actor": "s2"})
	if !decider.IsForbidden(err) || err.Error() != "only its own staff member may change it" {
		t.Fatalf("anyone else is forbidden, with the message: %v", err)
	}
}

// The owner field must be a text field of the aggregate's state.
func TestOwnerFieldIsChecked(t *testing.T) {
	for _, field := range []string{"ghost", "value"} {
		d := expenseDomain()
		d.Commands[1].OwnedBy.StateField = field
		if err := d.Validate(); err == nil {
			t.Errorf("owner field %q accepted", field)
		}
	}
}
