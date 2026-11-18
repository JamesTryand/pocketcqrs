package emschema

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Schema 6.0.0: the document names messages, never their sources, so the
// boundary parts that remain are read and their commands generated: a
// stateChange started by an ingress, an automation that waits (delay), one
// that calls out (effect, with its answer and give-up), and an output. Nothing
// that would fire or carry them is generated: no reactor for the delay or the
// effect, nothing at all for the output. upstream's order-fulfillment example,
// without its partitioning (still refused), maps and every scenario passes.
func TestBoundaryPartsImport(t *testing.T) {
	raw, err := os.ReadFile(fixture(t, "examples/order-fulfillment.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	delete(d, "partitioning")
	// the example's 3.3.0 outcomes don't say which applies, which pocketcqrs
	// refuses on its own account: the ingress slice gets a real condition,
	// auto-ship's plain alternatives go
	for _, sl := range d["slices"].([]any) {
		s := sl.(map[string]any)
		switch s["id"] {
		case "record-delivery-slice":
			s["outcomes"] = []any{
				map[string]any{"eventIds": []any{"order-delivered"},
					"when": []any{map[string]any{"all": []any{map[string]any{"command": "delivered", "equals": true}}}}},
				map[string]any{"eventIds": []any{"delivery-failed"}},
			}
		case "auto-ship-slice":
			delete(s, "outcomes")
		}
	}
	b, _ := json.Marshal(d)
	doc, err := Load(writeDoc(t, string(b)))
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := Map(doc, Options{AggregateOverrides: map[string]string{
		"record-partner-notified":               "shipmentNotice",
		"record-partner-notification-abandoned": "shipmentNotice",
	}})
	if err != nil {
		t.Fatal(err)
	}

	commands, reactors := map[string]bool{}, map[string]bool{}
	for _, dom := range mapped.Domains {
		for _, c := range dom.Commands {
			commands[c.Name] = true
		}
		for _, r := range dom.Reactors {
			reactors[r.Name] = true
		}
	}
	for _, c := range []string{"RecordDelivery", "FlagDeliveryOverdue", "RecordPartnerNotified", "RecordPartnerNotificationAbandoned"} {
		if !commands[c] {
			t.Errorf("command %s was not generated: %v", c, commands)
		}
	}
	if !reactors["autoShipPendingOrders"] || len(reactors) != 1 {
		t.Errorf("only auto-ship sends at once; the delay and the effect are fired by their sources: %v", reactors)
	}
	notes := strings.Join(mapped.Report.Warnings, "\n")
	for _, want := range []string{
		`automation "delivery-overdue-slice" waits before sending`,
		`automation "notify-shipping-partner-slice" calls out`,
		`output "order-confirmation" leaves after order-placed: nothing is generated`,
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("the report does not say %q:\n%s", want, notes)
		}
	}

	results, err := Verify(doc, mapped, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// the boundary slices: the ingress, the delay and the effect (answer and
	// give-up). Their scenarios all pass but one, which expects a refusal the
	// example never declares (no refuseWhen), so no generator can pass it.
	boundary := map[string]bool{"record-delivery-slice": true, "delivery-overdue-slice": true, "notify-shipping-partner-slice": true}
	ran := 0
	for _, r := range results {
		if !boundary[r.SliceID] || r.ScenarioID == "delivery-before-shipment-rejected" {
			continue
		}
		ran++
		if !r.Passed {
			t.Errorf("%s/%s: %s", r.SliceID, r.ScenarioID, r.Detail)
		}
	}
	if ran != 4 {
		t.Errorf("want the 4 boundary scenarios run, got %d", ran)
	}
}

// An output's fields must be on each of its trigger events: what leaves is
// what the model says (personal data too).
func TestAnOutputsFieldsMustBeOnItsTriggers(t *testing.T) {
	raw, err := os.ReadFile(fixture(t, "examples/order-fulfillment.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(raw), `"fields": ["orderId", "customerEmail", "items"]`, `"fields": ["orderId", "phone"]`, 1)
	var d map[string]any
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		t.Fatal(err)
	}
	delete(d, "partitioning")
	b, _ := json.Marshal(d)
	doc, err := Load(writeDoc(t, string(b)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Map(doc, Options{AggregateOverrides: map[string]string{
		"record-partner-notified": "shipmentNotice", "record-partner-notification-abandoned": "shipmentNotice"}})
	if err == nil || !strings.Contains(err.Error(), `output "order-confirmation": field "phone", which it says leaves, is not a field of trigger event "order-placed"`) {
		t.Fatalf("want the output's unknown field refused, got %v", err)
	}
}
