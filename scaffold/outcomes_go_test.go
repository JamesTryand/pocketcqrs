package scaffold

import "testing"

// payDomain: logging an entry over 12 hours also flags it (a declared
// condition, set fixing the reason); an expense is flagged by a
// hand-written hook; releasing a pay run pays each person a hook says is
// owed (a repeated event).
func payDomain() Domain {
	logged := Event{Name: "EntryLogged", Fields: []Field{{Name: "hours", Type: "number"}}}
	flagged := Event{Name: "EntryFlagged", Fields: []Field{{Name: "reason", Type: "text"}}}
	submitted := Event{Name: "ExpenseSubmitted", Fields: []Field{{Name: "amount", Type: "number"}}}
	expenseFlagged := Event{Name: "ExpenseFlagged", Fields: []Field{{Name: "reason", Type: "text"}}}
	released := Event{Name: "PayRunReleased", Fields: []Field{{Name: "period", Type: "text"}}}
	paid := Event{Name: "PersonPaid", Fields: []Field{{Name: "personId", Type: "text"}, {Name: "amount", Type: "number"}}}
	return Domain{
		Aggregate: "ledger",
		Commands: []Command{
			{Name: "LogEntry", Fields: []Field{{Name: "hours", Type: "number"}}, Events: []Event{logged, flagged},
				Outcomes: []Outcome{
					{Events: []string{"EntryLogged", "EntryFlagged"},
						When: [][]RefusalCondition{{{Side: "command", Field: "hours", Op: "greaterThan", Operand: &RefusalOperand{Literal: 12.0}}}},
						Set:  map[string]map[string]any{"EntryFlagged": {"reason": "long-day"}}},
					{Events: []string{"EntryLogged"}},
				}},
			{Name: "SubmitExpense", Fields: []Field{{Name: "amount", Type: "number"}}, Events: []Event{submitted, expenseFlagged},
				Outcomes: []Outcome{
					{Events: []string{"ExpenseSubmitted", "ExpenseFlagged"}, ByHand: true,
						Set: map[string]map[string]any{"ExpenseFlagged": {"reason": "over-limit"}}},
					{Events: []string{"ExpenseSubmitted"}},
				}},
			{Name: "ReleasePayRun", Fields: []Field{{Name: "period", Type: "text"}}, Events: []Event{released, paid},
				Outcomes: []Outcome{{Events: []string{"PayRunReleased", "PersonPaid"}, Repeated: []string{"PersonPaid"}}}},
		},
	}
}

const payBehaviour = `package ledger

import (
	"encoding/json"
	"testing"

	"github.com/jamestryand/pocketcqrs/decider"
)

// the hand-written hooks the decider calls (ledger_hand.go in a real host)
func SubmitExpenseOutcome0(s State, cmd decider.Command) bool {
	var p struct{ Amount float64 }
	_ = json.Unmarshal(cmd.Payload, &p)
	return p.Amount > 500
}

func ReleasePayRunOutcome0PersonPaid(s State, cmd decider.Command) []map[string]any {
	return []map[string]any{{"personId": "p1", "amount": 120}, {"personId": "p2", "amount": 80}}
}

func types(t *testing.T, name, payload string) []string {
	evs, err := Ledger().Decide(decider.Command{Name: name, Payload: json.RawMessage(payload)}, State{})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		out = append(out, e.Type+string(e.Data))
	}
	return out
}

func TestOutcomes(t *testing.T) {
	for _, c := range []struct{ name, payload string; want []string }{
		{"LogEntry", ` + "`" + `{"hours":8}` + "`" + `, []string{"EntryLogged{\"hours\":8}"}},
		{"LogEntry", ` + "`" + `{"hours":13}` + "`" + `, []string{"EntryLogged{\"hours\":13}", "EntryFlagged{\"reason\":\"long-day\"}"}},
		{"SubmitExpense", ` + "`" + `{"amount":100}` + "`" + `, []string{"ExpenseSubmitted{\"amount\":100}"}},
		{"SubmitExpense", ` + "`" + `{"amount":900}` + "`" + `, []string{"ExpenseSubmitted{\"amount\":900}", "ExpenseFlagged{\"reason\":\"over-limit\"}"}},
		{"ReleasePayRun", ` + "`" + `{"period":"2026-09"}` + "`" + `, []string{"PayRunReleased{\"period\":\"2026-09\"}",
			"PersonPaid{\"amount\":120,\"personId\":\"p1\"}", "PersonPaid{\"amount\":80,\"personId\":\"p2\"}"}},
	} {
		got := types(t, c.name, c.payload)
		if len(got) != len(c.want) {
			t.Fatalf("%s %s: got %v, want %v", c.name, c.payload, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s %s: event %d is %s, want %s", c.name, c.payload, i, got[i], c.want[i])
			}
		}
	}
}
`

// Outcomes on the Go target: a declared condition with set, a hand hook and
// a repeated event, compiled in their own module with the hooks beside them
// and run. A hook missing would be a build error.
func TestGoOutcomesRun(t *testing.T) {
	runGeneratedGo(t, payDomain(), payBehaviour)
}
