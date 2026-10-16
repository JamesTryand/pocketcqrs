package emschema

import "testing"

// A scenario with no caller reads as a role holder, and a self-access param
// it sends names the subject: "a1's tickets" are a1's (dotnetcqrs's harness
// binds the sent value the same way). Scenarios of the shape "an agent's
// view is scoped to their own tickets" found it: they got no rows.
const selfParamDoc = `{"eventModelingSchemaVersion":"3.17.0","id":"p","name":"P",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{"logged":{"name":"Logged","swimlaneId":"s","aggregate":"ticket","fields":[{"name":"ticketId","type":"string","idAttribute":true},{"name":"agentId","type":"string"}]}},
	"commands":{"log":{"name":"Log","aggregate":"ticket","fields":[{"name":"ticketId","type":"string","idAttribute":true},{"name":"agentId","type":"string"}]}},
	"readModels":{"tickets":{"name":"Tickets","builtFromEventIds":["logged"],"requiredRole":["supervisor"],
		"selfAccess":{"subjectField":"agentId","param":"agentId"},
		"fields":[{"name":"ticketId","type":"string","idAttribute":true},{"name":"agentId","type":"string"}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[
		{"id":"l","name":"L","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc","commandId":"log","eventIds":["logged"],"scenarios":[]},
		{"id":"v","name":"V","pattern":"stateView","swimlaneId":"s","status":"planned","screenId":"sc","readModelId":"tickets","scenarios":[
			{"id":"own","name":"own","kind":"stateView",
			 "given":[{"eventId":"logged","data":{"ticketId":"t1","agentId":"a1"}},{"eventId":"logged","data":{"ticketId":"t2","agentId":"a2"}}],
			 "when":{"readModelId":"tickets","queryParams":{"agentId":"a1"}},
			 "then":{"result":{"entries":[{"ticketId":"t1"}]}}},
			{"id":"caller-wins","name":"caller wins","kind":"stateView",
			 "given":[{"eventId":"logged","data":{"ticketId":"t1","agentId":"a1"}},{"eventId":"logged","data":{"ticketId":"t2","agentId":"a2"}}],
			 "when":{"readModelId":"tickets","queryParams":{"agentId":"a1"},"caller":{"subjectId":"a2","role":"agent"}},
			 "then":{"result":{"entries":[{"ticketId":"t2"}]}}}]}]}`

func TestSelfParamWithoutACallerNamesTheSubject(t *testing.T) {
	results := verifyDoc(t, selfParamDoc)
	for id, r := range results {
		if !r.Passed {
			t.Errorf("%s: %s", id, r.Detail)
		}
	}
	if len(results) != 2 {
		t.Fatalf("want 2 scenarios, got %d", len(results))
	}
}
