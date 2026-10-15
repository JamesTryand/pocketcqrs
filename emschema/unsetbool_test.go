package emschema

import "testing"

// A row's bool column no event has set reads false, as PocketBase serves an
// unset bool field (and as dotnetcqrs's harness compares one). A project
// deactivated by an event that carries no "active" is expected inactive...
// only because nothing set it: the timesheets model's IMP-11 scenario. A
// text column stays strict: unset is not "".
const unsetBoolDoc = `{"eventModelingSchemaVersion":"3.17.0","id":"b","name":"B",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{"created":{"name":"Created","swimlaneId":"s","aggregate":"project","fields":[{"name":"projectId","type":"string","idAttribute":true}]}},
	"commands":{"create":{"name":"Create","aggregate":"project","fields":[{"name":"projectId","type":"string","idAttribute":true}]}},
	"readModels":{"projects":{"name":"Projects","builtFromEventIds":["created"],
		"fields":[{"name":"projectId","type":"string","idAttribute":true},{"name":"active","type":"boolean"},{"name":"note","type":"string"}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[
		{"id":"c","name":"C","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc","commandId":"create","eventIds":["created"],"scenarios":[]},
		{"id":"v","name":"V","pattern":"stateView","swimlaneId":"s","status":"planned","screenId":"sc","readModelId":"projects","scenarios":[
			{"id":"unset-false","name":"unset is false","kind":"stateView","given":[{"eventId":"created","data":{"projectId":"p1"}}],
			 "when":{"readModelId":"projects"},"then":{"result":{"entries":[{"projectId":"p1","active":false}]}}},
			{"id":"unset-not-true","name":"unset is not true","kind":"stateView","given":[{"eventId":"created","data":{"projectId":"p1"}}],
			 "when":{"readModelId":"projects"},"then":{"result":{"entries":[{"projectId":"p1","active":true}]}}},
			{"id":"unset-not-empty","name":"unset text is not empty","kind":"stateView","given":[{"eventId":"created","data":{"projectId":"p1"}}],
			 "when":{"readModelId":"projects"},"then":{"result":{"entries":[{"projectId":"p1","note":""}]}}}]}]}`

func TestUnsetBoolReadsFalse(t *testing.T) {
	results := verifyDoc(t, unsetBoolDoc)
	want := map[string]bool{"unset-false": true, "unset-not-true": false, "unset-not-empty": false}
	for id, pass := range want {
		if results[id].Passed != pass {
			t.Errorf("%s: passed=%v, want %v (%s)", id, results[id].Passed, pass, results[id].Detail)
		}
	}
}
