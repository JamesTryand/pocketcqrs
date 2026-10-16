package scaffold

import (
	"strings"
	"testing"
)

// accountsDomain: a booking over 14 days is also flagged (a declared
// condition, set fixing the reason); a refund is flagged by a hand-written
// hook; closing a grant round awards each recipient a hook says has earned
// one (a repeated event).
func accountsDomain() Domain {
	logged := Event{Name: "BookingMade", Fields: []Field{{Name: "days", Type: "number"}}}
	flagged := Event{Name: "BookingFlagged", Fields: []Field{{Name: "reason", Type: "text"}}}
	submitted := Event{Name: "RefundRequested", Fields: []Field{{Name: "amount", Type: "number"}}}
	refundFlagged := Event{Name: "RefundFlagged", Fields: []Field{{Name: "reason", Type: "text"}}}
	closed := Event{Name: "GrantRoundClosed", Fields: []Field{{Name: "period", Type: "text"}}}
	awarded := Event{Name: "GrantAwarded", Fields: []Field{{Name: "recipientId", Type: "text"}, {Name: "amount", Type: "number"}}}
	return Domain{
		Aggregate: "ledger",
		Commands: []Command{
			{Name: "MakeBooking", Fields: []Field{{Name: "days", Type: "number"}}, Events: []Event{logged, flagged},
				Outcomes: []Outcome{
					{Events: []string{"BookingMade", "BookingFlagged"},
						When: [][]RefusalCondition{{{Side: "command", Field: "days", Op: "greaterThan", Operand: &RefusalOperand{Literal: 14.0}}}},
						Set:  map[string]map[string]any{"BookingFlagged": {"reason": "long-hire"}}},
					{Events: []string{"BookingMade"}},
				}},
			{Name: "RequestRefund", Fields: []Field{{Name: "amount", Type: "number"}}, Events: []Event{submitted, refundFlagged},
				Outcomes: []Outcome{
					{Events: []string{"RefundRequested", "RefundFlagged"}, ByHand: true,
						Set: map[string]map[string]any{"RefundFlagged": {"reason": "over-threshold"}}},
					{Events: []string{"RefundRequested"}},
				}},
			{Name: "CloseGrantRound", Fields: []Field{{Name: "period", Type: "text"}}, Events: []Event{closed, awarded},
				Outcomes: []Outcome{{Events: []string{"GrantRoundClosed", "GrantAwarded"}, Repeated: []string{"GrantAwarded"}}}},
		},
	}
}

const accountsBehaviour = `package ledger

import (
	"encoding/json"
	"testing"

	"github.com/jamestryand/pocketcqrs/decider"
)

// the hand-written hooks the decider calls (ledger_hand.go in a real host)
func RequestRefundOutcome0(s State, cmd decider.Command) bool {
	var p struct{ Amount float64 }
	_ = json.Unmarshal(cmd.Payload, &p)
	return p.Amount > 500
}

func CloseGrantRoundOutcome0GrantAwarded(s State, cmd decider.Command) []map[string]any {
	return []map[string]any{{"recipientId": "r1", "amount": 120}, {"recipientId": "r2", "amount": 80}}
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
		{"MakeBooking", ` + "`" + `{"days":7}` + "`" + `, []string{"BookingMade{\"days\":7}"}},
		{"MakeBooking", ` + "`" + `{"days":15}` + "`" + `, []string{"BookingMade{\"days\":15}", "BookingFlagged{\"reason\":\"long-hire\"}"}},
		{"RequestRefund", ` + "`" + `{"amount":100}` + "`" + `, []string{"RefundRequested{\"amount\":100}"}},
		{"RequestRefund", ` + "`" + `{"amount":900}` + "`" + `, []string{"RefundRequested{\"amount\":900}", "RefundFlagged{\"reason\":\"over-threshold\"}"}},
		{"CloseGrantRound", ` + "`" + `{"period":"2026-09"}` + "`" + `, []string{"GrantRoundClosed{\"period\":\"2026-09\"}",
			"GrantAwarded{\"amount\":120,\"recipientId\":\"r1\"}", "GrantAwarded{\"amount\":80,\"recipientId\":\"r2\"}"}},
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
	runGeneratedGo(t, accountsDomain(), accountsBehaviour)
}

// The Go import writes the hand file's skeleton, <aggregate>_hand.go.example,
// with every hook the decider calls; nothing compiles it.
func TestGoHandExampleIsWritten(t *testing.T) {
	d := accountsDomain()
	files, err := d.GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Name == d.Aggregate+"_hand.go.example" {
			for _, h := range d.Hooks() {
				if !strings.Contains(f.Source, "func "+h) {
					t.Errorf("the example lacks %s", h)
				}
			}
			return
		}
	}
	t.Fatal("no _hand.go.example")
}
