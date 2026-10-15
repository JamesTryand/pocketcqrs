package functions

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

// expenseJS is a decider whose outcome is chosen by a hand-written hook
// (schema 3.12.0 when: "hand"), the way the generator writes it.
const expenseJS = `//@trigger decider expense
//@hand expense.hand.js
//@hooks SubmitExpenseOutcome0
//@handles ExpenseSubmitted ExpenseFlagged
//@commands SubmitExpense

function initialState() { return { exists: false }; }
function decide(command, state) {
  switch (command.name) {
    case 'SubmitExpense':
      if (SubmitExpenseOutcome0(state, command)) {
        return [{ type: 'ExpenseSubmitted', data: { amount: command.payload.amount } },
                { type: 'ExpenseFlagged', data: { reason: 'over-limit' } }];
      }
      return [{ type: 'ExpenseSubmitted', data: { amount: command.payload.amount } }];
  }
  throw new Error('unknown command: ' + command.name);
}
function evolve(state, event) { return Object.assign({}, state, { exists: true }); }
`

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A decider's hand-written hooks load from its companion file into the
// same program: they decide the outcome, in the decider's own VM.
func TestHandHooksLoadAndDecide(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"expense.js":      expenseJS,
		"expense.hand.js": "function SubmitExpenseOutcome0(state, command) { return command.payload.amount > 500; }\n",
	})
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loaded, err := LoadDir(NewGojaRuntime(func(string, ...any) {}), nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Deciders) != 1 {
		t.Fatalf("want the decider (the hand file is not a function file), got %d", len(loaded.Deciders))
	}
	spec := loaded.Deciders[0]
	if err := ValidateDeciderSpec(store, spec); err != nil {
		t.Fatal(err)
	}
	registry := decider.NewRegistry(store)
	registry.RegisterUntyped("expense", spec.UntypedDecider())
	for id, want := range map[string]int{"small": 1, "large": 2} {
		amount := 100
		if id == "large" {
			amount = 900
		}
		got, err := registry.HandleWithMeta(context.Background(), "expense", id,
			decider.Command{Name: "SubmitExpense", Payload: json.RawMessage(`{"amount":` + strconv.Itoa(amount) + `}`)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != want {
			t.Errorf("%s: the hook should give %d event(s), got %+v", id, want, got)
		}
	}
}

// A decider whose hand file is missing does not load, and says which hooks
// it needs.
func TestHandFileMissingRefusesTheDecider(t *testing.T) {
	dir := writeFiles(t, map[string]string{"expense.js": expenseJS})
	_, err := LoadDir(NewGojaRuntime(func(string, ...any) {}), nil, dir)
	if err == nil || !strings.Contains(err.Error(), "SubmitExpenseOutcome0") {
		t.Fatalf("want a refusal naming the hook, got %v", err)
	}
}

// A hand file that doesn't define a hook the decider calls fails the load
// check, not the first command that reaches it.
func TestUndefinedHookFailsValidation(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"expense.js":      expenseJS,
		"expense.hand.js": "function somethingElse() {}\n",
	})
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loaded, err := LoadDir(NewGojaRuntime(func(string, ...any) {}), nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateDeciderSpec(store, loaded.Deciders[0]); err == nil || !strings.Contains(err.Error(), "SubmitExpenseOutcome0") {
		t.Fatalf("want a validation error naming the hook, got %v", err)
	}
}

// A hook runs in the decider's purified VM: no clock, as for decide itself.
func TestHookHasNoClock(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"expense.js":      expenseJS,
		"expense.hand.js": "function SubmitExpenseOutcome0(state, command) { return new Date().getDay() === 1; }\n",
	})
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loaded, err := LoadDir(NewGojaRuntime(func(string, ...any) {}), nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	registry := decider.NewRegistry(store)
	registry.RegisterUntyped("expense", loaded.Deciders[0].UntypedDecider())
	_, err = registry.HandleWithMeta(context.Background(), "expense", "x",
		decider.Command{Name: "SubmitExpense", Payload: json.RawMessage(`{"amount":1}`)}, nil)
	if err == nil || !strings.Contains(err.Error(), "TypeError") {
		t.Fatalf("a hook reading the clock must fail, got %v", err)
	}
}
