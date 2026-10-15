package emschema

import "testing"

// byEvent (schema 3.16.0): a field set to a literal per event; an event not
// named leaves it as it was.
const byEventDoc = `{"eventModelingSchemaVersion":"3.17.0","id":"b","name":"B",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{
		"invoice-staged":{"name":"Invoice Staged","swimlaneId":"s","aggregate":"invoice","fields":[{"name":"invoiceId","type":"string","idAttribute":true},{"name":"total","type":"decimal"}]},
		"invoice-noted":{"name":"Invoice Noted","swimlaneId":"s","aggregate":"invoice","fields":[{"name":"invoiceId","type":"string","idAttribute":true},{"name":"note","type":"string"}]},
		"invoice-sent":{"name":"Invoice Sent","swimlaneId":"s","aggregate":"invoice","fields":[{"name":"invoiceId","type":"string","idAttribute":true}]}},
	"commands":{
		"stage":{"name":"Stage","aggregate":"invoice","fields":[{"name":"invoiceId","type":"string","idAttribute":true},{"name":"total","type":"decimal"}]},
		"note":{"name":"Note","aggregate":"invoice","fields":[{"name":"invoiceId","type":"string","idAttribute":true},{"name":"note","type":"string"}]},
		"send":{"name":"Send","aggregate":"invoice","fields":[{"name":"invoiceId","type":"string","idAttribute":true}]}},
	"readModels":{"invoices":{"name":"Invoices","builtFromEventIds":["invoice-staged","invoice-noted","invoice-sent"],
		"fields":[{"name":"invoiceId","type":"string","idAttribute":true},{"name":"total","type":"decimal"},
			{"name":"status","type":"string","derivation":{"kind":"byEvent","values":{"invoice-staged":"staged","invoice-sent":"sent"}}}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[
		{"id":"a","name":"A","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc","commandId":"stage","eventIds":["invoice-staged"],"scenarios":[]},
		{"id":"n","name":"N","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc","commandId":"note","eventIds":["invoice-noted"],"scenarios":[]},
		{"id":"b","name":"B","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc","commandId":"send","eventIds":["invoice-sent"],"scenarios":[]},
		{"id":"v","name":"V","pattern":"stateView","swimlaneId":"s","status":"planned","screenId":"sc","readModelId":"invoices","scenarios":[
			{"id":"staged","name":"staged","kind":"stateView",
			 "given":[{"eventId":"invoice-staged","data":{"invoiceId":"i1","total":10}},{"eventId":"invoice-noted","data":{"invoiceId":"i1","note":"x"}}],
			 "when":{"readModelId":"invoices"},
			 "then":{"result":{"invoices":[{"invoiceId":"i1","total":10,"status":"staged"}]}}},
			{"id":"sent","name":"sent","kind":"stateView",
			 "given":[{"eventId":"invoice-staged","data":{"invoiceId":"i1","total":10}},{"eventId":"invoice-sent","data":{"invoiceId":"i1"}},{"eventId":"invoice-noted","data":{"invoiceId":"i1","note":"x"}}],
			 "when":{"readModelId":"invoices"},
			 "then":{"result":{"invoices":[{"invoiceId":"i1","total":10,"status":"sent"}]}}}]}]}`

func TestByEventDerivation(t *testing.T) {
	results := verifyDoc(t, byEventDoc)
	for id, r := range results {
		if !r.Passed {
			t.Errorf("%s: %s", id, r.Detail)
		}
	}
	if len(results) != 2 {
		t.Fatalf("want 2 scenarios, got %d", len(results))
	}
}
