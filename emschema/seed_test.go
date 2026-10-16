package emschema

import "testing"

// An event a toggle derives from still merges its own fields onto its row,
// as dotnetcqrs seeds it (SeedOn: builtFromEventIds and toggle/byEvent
// events). Flagging stamps the reason; clearing stamps who cleared it. A
// flagged-tickets view lost both.
const toggleSeedDoc = `{"eventModelingSchemaVersion":"3.17.0","id":"t","name":"T",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{
		"flagged":{"name":"Flagged","swimlaneId":"s","aggregate":"ticket","fields":[{"name":"ticketId","type":"string","idAttribute":true},{"name":"reason","type":"string"}]},
		"cleared":{"name":"Cleared","swimlaneId":"s","aggregate":"ticket","fields":[{"name":"ticketId","type":"string","idAttribute":true},{"name":"clearedBy","type":"string"}]}},
	"commands":{
		"flag":{"name":"Flag","aggregate":"ticket","fields":[{"name":"ticketId","type":"string","idAttribute":true},{"name":"reason","type":"string"}]},
		"clear":{"name":"Clear","aggregate":"ticket","fields":[{"name":"ticketId","type":"string","idAttribute":true},{"name":"clearedBy","type":"string"}]}},
	"readModels":{"flags":{"name":"Flags","builtFromEventIds":["flagged","cleared"],
		"fields":[{"name":"ticketId","type":"string","idAttribute":true},{"name":"reason","type":"string"},{"name":"clearedBy","type":"string"},
			{"name":"isCleared","type":"boolean","derivation":{"kind":"toggle","onEventIds":["cleared"],"offEventIds":["flagged"]}}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[
		{"id":"f","name":"F","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc","commandId":"flag","eventIds":["flagged"],"scenarios":[]},
		{"id":"c","name":"C","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc","commandId":"clear","eventIds":["cleared"],"scenarios":[]},
		{"id":"v","name":"V","pattern":"stateView","swimlaneId":"s","status":"planned","screenId":"sc","readModelId":"flags","scenarios":[
			{"id":"open","name":"open","kind":"stateView",
			 "given":[{"eventId":"flagged","data":{"ticketId":"e1","reason":"late"}}],
			 "when":{"readModelId":"flags"},
			 "then":{"result":{"flags":[{"ticketId":"e1","reason":"late","isCleared":false}]}}},
			{"id":"closed","name":"closed","kind":"stateView",
			 "given":[{"eventId":"flagged","data":{"ticketId":"e1","reason":"late"}},{"eventId":"cleared","data":{"ticketId":"e1","clearedBy":"sup1"}}],
			 "when":{"readModelId":"flags"},
			 "then":{"result":{"flags":[{"ticketId":"e1","reason":"late","clearedBy":"sup1","isCleared":true}]}}}]}]}`

func TestToggleEventsStillMerge(t *testing.T) {
	results := verifyDoc(t, toggleSeedDoc)
	for id, r := range results {
		if !r.Passed {
			t.Errorf("%s: %s", id, r.Detail)
		}
	}
	if len(results) != 2 {
		t.Fatalf("want 2 scenarios, got %d", len(results))
	}
}
