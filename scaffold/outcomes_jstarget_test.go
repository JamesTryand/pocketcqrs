package scaffold

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/functions"
)

// The JS target runs the same outcomes as TestGoOutcomesRun, with a real
// hand file compiled into the decider's program.
func TestJSOutcomesRun(t *testing.T) {
	files, err := accountsDomain().Generate()
	if err != nil {
		t.Fatal(err)
	}
	if files[1].Name != "ledger.hand.js.example" || !strings.Contains(files[0].Source, "//@hooks CloseGrantRoundOutcome0GrantAwarded RequestRefundOutcome0") {
		t.Fatalf("the decider names its hooks and an example hand file comes with it: %s\n%s", files[1].Name, files[0].Source)
	}
	hand := `function RequestRefundOutcome0(state, command) { return command.payload.amount > 500; }
function CloseGrantRoundOutcome0GrantAwarded(state, command) { return [{ recipientId: 'r1', amount: 120 }, { recipientId: 'r2', amount: 80 }]; }
`
	spec, err := functions.LoadDeciderSourceWithHand(functions.NewGojaRuntime(nil), "ledger.js", files[0].Source, hand)
	if err != nil {
		t.Fatal(err)
	}
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, c := range []struct{ name, payload, want string }{
		{"MakeBooking", `{"days":7}`, `BookingMade{"days":7}`},
		{"MakeBooking", `{"days":15}`, `BookingMade{"days":15} BookingFlagged{"reason":"long-hire"}`},
		{"RequestRefund", `{"amount":100}`, `RefundRequested{"amount":100}`},
		{"RequestRefund", `{"amount":900}`, `RefundRequested{"amount":900} RefundFlagged{"reason":"over-threshold"}`},
		{"CloseGrantRound", `{"period":"2026-09"}`, `GrantRoundClosed{"period":"2026-09"} GrantAwarded{"amount":120,"recipientId":"r1"} GrantAwarded{"amount":80,"recipientId":"r2"}`},
	} {
		run, err := functions.DryRunDecide(store, spec, "s", decider.Command{Name: c.name, Payload: json.RawMessage(c.payload)}, map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		if run.Rejected {
			t.Fatalf("%s: refused: %s", c.name, run.Message)
		}
		var got []string
		for _, e := range run.Produced {
			var data map[string]any
			_ = json.Unmarshal(e.Data, &data)
			canon, _ := json.Marshal(data)
			got = append(got, e.Type+string(canon))
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("%s %s: got %s, want %s", c.name, c.payload, strings.Join(got, " "), c.want)
		}
	}
}
