package emschema

import "testing"

// A scenario with no caller reads as a role holder, and a self-access param
// it sends names the subject: "s1's entries" are s1's (dotnetcqrs's harness
// binds the sent value the same way). The timesheets "staff member's view is
// scoped to their own entries" scenarios found it: they got no rows.
const selfParamDoc = `{"eventModelingSchemaVersion":"3.17.0","id":"p","name":"P",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{"logged":{"name":"Logged","swimlaneId":"s","aggregate":"entry","fields":[{"name":"entryId","type":"string","idAttribute":true},{"name":"staffId","type":"string"}]}},
	"commands":{"log":{"name":"Log","aggregate":"entry","fields":[{"name":"entryId","type":"string","idAttribute":true},{"name":"staffId","type":"string"}]}},
	"readModels":{"entries":{"name":"Entries","builtFromEventIds":["logged"],"requiredRole":["manager"],
		"selfAccess":{"subjectField":"staffId","param":"staffId"},
		"fields":[{"name":"entryId","type":"string","idAttribute":true},{"name":"staffId","type":"string"}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[
		{"id":"l","name":"L","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc","commandId":"log","eventIds":["logged"],"scenarios":[]},
		{"id":"v","name":"V","pattern":"stateView","swimlaneId":"s","status":"planned","screenId":"sc","readModelId":"entries","scenarios":[
			{"id":"own","name":"own","kind":"stateView",
			 "given":[{"eventId":"logged","data":{"entryId":"e1","staffId":"s1"}},{"eventId":"logged","data":{"entryId":"e2","staffId":"s2"}}],
			 "when":{"readModelId":"entries","queryParams":{"staffId":"s1"}},
			 "then":{"result":{"entries":[{"entryId":"e1"}]}}},
			{"id":"caller-wins","name":"caller wins","kind":"stateView",
			 "given":[{"eventId":"logged","data":{"entryId":"e1","staffId":"s1"}},{"eventId":"logged","data":{"entryId":"e2","staffId":"s2"}}],
			 "when":{"readModelId":"entries","queryParams":{"staffId":"s1"},"caller":{"subjectId":"s2","role":"staff"}},
			 "then":{"result":{"entries":[{"entryId":"e2"}]}}}]}]}`

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
