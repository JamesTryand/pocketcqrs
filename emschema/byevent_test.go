package emschema

import "testing"

// byEvent (schema 3.16.0): a field set to a literal per event; an event not
// named leaves it as it was.
const byEventDoc = `{"eventModelingSchemaVersion":"3.17.0","id":"b","name":"B",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{
		"shipment-staged":{"name":"Shipment Staged","swimlaneId":"s","aggregate":"shipment","fields":[{"name":"shipmentId","type":"string","idAttribute":true},{"name":"total","type":"decimal"}]},
		"shipment-noted":{"name":"Shipment Noted","swimlaneId":"s","aggregate":"shipment","fields":[{"name":"shipmentId","type":"string","idAttribute":true},{"name":"note","type":"string"}]},
		"shipment-sent":{"name":"Shipment Sent","swimlaneId":"s","aggregate":"shipment","fields":[{"name":"shipmentId","type":"string","idAttribute":true}]}},
	"commands":{
		"stage":{"name":"Stage","aggregate":"shipment","fields":[{"name":"shipmentId","type":"string","idAttribute":true},{"name":"total","type":"decimal"}]},
		"note":{"name":"Note","aggregate":"shipment","fields":[{"name":"shipmentId","type":"string","idAttribute":true},{"name":"note","type":"string"}]},
		"send":{"name":"Send","aggregate":"shipment","fields":[{"name":"shipmentId","type":"string","idAttribute":true}]}},
	"readModels":{"shipments":{"name":"Shipments","builtFromEventIds":["shipment-staged","shipment-noted","shipment-sent"],
		"fields":[{"name":"shipmentId","type":"string","idAttribute":true},{"name":"total","type":"decimal"},
			{"name":"status","type":"string","derivation":{"kind":"byEvent","values":{"shipment-staged":"staged","shipment-sent":"sent"}}}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[
		{"id":"a","name":"A","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc","commandId":"stage","eventIds":["shipment-staged"],"scenarios":[]},
		{"id":"n","name":"N","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc","commandId":"note","eventIds":["shipment-noted"],"scenarios":[]},
		{"id":"b","name":"B","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc","commandId":"send","eventIds":["shipment-sent"],"scenarios":[]},
		{"id":"v","name":"V","pattern":"stateView","swimlaneId":"s","status":"planned","screenId":"sc","readModelId":"shipments","scenarios":[
			{"id":"staged","name":"staged","kind":"stateView",
			 "given":[{"eventId":"shipment-staged","data":{"shipmentId":"sh1","total":10}},{"eventId":"shipment-noted","data":{"shipmentId":"sh1","note":"x"}}],
			 "when":{"readModelId":"shipments"},
			 "then":{"result":{"shipments":[{"shipmentId":"sh1","total":10,"status":"staged"}]}}},
			{"id":"sent","name":"sent","kind":"stateView",
			 "given":[{"eventId":"shipment-staged","data":{"shipmentId":"sh1","total":10}},{"eventId":"shipment-sent","data":{"shipmentId":"sh1"}},{"eventId":"shipment-noted","data":{"shipmentId":"sh1","note":"x"}}],
			 "when":{"readModelId":"shipments"},
			 "then":{"result":{"shipments":[{"shipmentId":"sh1","total":10,"status":"sent"}]}}}]}]}`

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
