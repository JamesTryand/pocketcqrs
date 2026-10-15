package emschema

import (
	"strings"
	"testing"
)

// payRunDoc: one pay-run stream records a payment per person; the
// payments read model has a row per payment (rowKeyField paymentId, schema
// 3.14.0), and a reversal removes that payment's row only.
const payRunDoc = `{"eventModelingSchemaVersion":"3.17.0","id":"p","name":"P",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{
		"person-paid":{"name":"Person Paid","swimlaneId":"s","aggregate":"payRun",
			"fields":[{"name":"payRunId","type":"string","idAttribute":true},{"name":"paymentId","type":"string"},{"name":"personId","type":"string"},{"name":"amount","type":"decimal"}]},
		"payment-reversed":{"name":"Payment Reversed","swimlaneId":"s","aggregate":"payRun",
			"fields":[{"name":"payRunId","type":"string","idAttribute":true},{"name":"paymentId","type":"string"}]}},
	"commands":{
		"pay":{"name":"Pay","aggregate":"payRun","fields":[{"name":"payRunId","type":"string","idAttribute":true},{"name":"paymentId","type":"string"},{"name":"personId","type":"string"},{"name":"amount","type":"decimal"}]},
		"reverse":{"name":"Reverse","aggregate":"payRun","fields":[{"name":"payRunId","type":"string","idAttribute":true},{"name":"paymentId","type":"string"}]}},
	"readModels":{"payments":{"name":"Payments","builtFromEventIds":["person-paid","payment-reversed"],
		"removedByEventIds":["payment-reversed"],"rowKeyField":"paymentId",
		"fields":[{"name":"paymentId","type":"string"},{"name":"personId","type":"string"},{"name":"amount","type":"decimal"}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[
		{"id":"pay","name":"Pay","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc","commandId":"pay","eventIds":["person-paid"],"scenarios":[]},
		{"id":"rev","name":"Reverse","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc","commandId":"reverse","eventIds":["payment-reversed"],"scenarios":[]},
		{"id":"view","name":"View","pattern":"stateView","swimlaneId":"s","status":"planned","screenId":"sc","readModelId":"payments","scenarios":[
			{"id":"two-payments","name":"two payments in one run are two rows","kind":"stateView",
			 "given":[{"eventId":"person-paid","data":{"payRunId":"r1","paymentId":"pay-1","personId":"ann","amount":100}},
			          {"eventId":"person-paid","data":{"payRunId":"r1","paymentId":"pay-2","personId":"bob","amount":200}}],
			 "when":{"readModelId":"payments"},
			 "then":{"result":{"payments":[{"paymentId":"pay-1","personId":"ann","amount":100},{"paymentId":"pay-2","personId":"bob","amount":200}]}}},
			{"id":"one-reversed","name":"a reversal removes that payment's row only","kind":"stateView",
			 "given":[{"eventId":"person-paid","data":{"payRunId":"r1","paymentId":"pay-1","personId":"ann","amount":100}},
			          {"eventId":"person-paid","data":{"payRunId":"r1","paymentId":"pay-2","personId":"bob","amount":200}},
			          {"eventId":"payment-reversed","data":{"payRunId":"r1","paymentId":"pay-1"}}],
			 "when":{"readModelId":"payments"},
			 "then":{"result":{"payments":[{"paymentId":"pay-2","personId":"bob","amount":200}]}}}]}]}`

// A read model's rowKeyField (schema 3.14.0) keys each row by a field of the
// event, not its stream: one stream's events can update many rows.
func TestRowKeyFieldKeysRowsByTheEvent(t *testing.T) {
	results := verifyDoc(t, payRunDoc)
	for id, r := range results {
		if !r.Passed {
			t.Errorf("%s: %s", id, r.Detail)
		}
	}
	if len(results) != 2 {
		t.Fatalf("want 2 scenarios, got %d", len(results))
	}
}

// Every event a read model is built from (or removed by) must carry its
// rowKeyField, or the import is refused: such an event could name no row.
func TestRowKeyFieldMustBeOnEveryEvent(t *testing.T) {
	doc, err := Load(writeDoc(t, strings.Replace(payRunDoc,
		`"payment-reversed":{"name":"Payment Reversed","swimlaneId":"s","aggregate":"payRun",
			"fields":[{"name":"payRunId","type":"string","idAttribute":true},{"name":"paymentId","type":"string"}]}`,
		`"payment-reversed":{"name":"Payment Reversed","swimlaneId":"s","aggregate":"payRun",
			"fields":[{"name":"payRunId","type":"string","idAttribute":true}]}`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Map(doc, Options{}); err == nil || !strings.Contains(err.Error(), "rowKeyField") {
		t.Fatalf("want a refusal naming rowKeyField, got %v", err)
	}
}
