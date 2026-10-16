package emschema

import "testing"

// A row's bool column no event has set reads false, as PocketBase serves an
// unset bool field (and as dotnetcqrs's harness compares one). A team
// deactivated by an event that carries no "active" is expected inactive...
// only because nothing set it. A
// text column stays strict: unset is not "".
const unsetBoolDoc = `{"eventModelingSchemaVersion":"3.17.0","id":"b","name":"B",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{"created":{"name":"Created","swimlaneId":"s","aggregate":"team","fields":[{"name":"teamId","type":"string","idAttribute":true}]}},
	"commands":{"create":{"name":"Create","aggregate":"team","fields":[{"name":"teamId","type":"string","idAttribute":true}]}},
	"readModels":{"teams":{"name":"Teams","builtFromEventIds":["created"],
		"fields":[{"name":"teamId","type":"string","idAttribute":true},{"name":"active","type":"boolean"},{"name":"note","type":"string"}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[
		{"id":"c","name":"C","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc","commandId":"create","eventIds":["created"],"scenarios":[]},
		{"id":"v","name":"V","pattern":"stateView","swimlaneId":"s","status":"planned","screenId":"sc","readModelId":"teams","scenarios":[
			{"id":"unset-false","name":"unset is false","kind":"stateView","given":[{"eventId":"created","data":{"teamId":"g1"}}],
			 "when":{"readModelId":"teams"},"then":{"result":{"entries":[{"teamId":"g1","active":false}]}}},
			{"id":"unset-not-true","name":"unset is not true","kind":"stateView","given":[{"eventId":"created","data":{"teamId":"g1"}}],
			 "when":{"readModelId":"teams"},"then":{"result":{"entries":[{"teamId":"g1","active":true}]}}},
			{"id":"unset-not-empty","name":"unset text is not empty","kind":"stateView","given":[{"eventId":"created","data":{"teamId":"g1"}}],
			 "when":{"readModelId":"teams"},"then":{"result":{"entries":[{"teamId":"g1","note":""}]}}}]}]}`

func TestUnsetBoolReadsFalse(t *testing.T) {
	results := verifyDoc(t, unsetBoolDoc)
	want := map[string]bool{"unset-false": true, "unset-not-true": false, "unset-not-empty": false}
	for id, pass := range want {
		if results[id].Passed != pass {
			t.Errorf("%s: passed=%v, want %v (%s)", id, results[id].Passed, pass, results[id].Detail)
		}
	}
}

// A row's date reads as PocketBase serves a date field ("2026-09-15
// 00:00:00.000Z"): it matches the model's "2026-09-15" (midnight, as
// dotnetcqrs's harness matches one) and the same instant written RFC 3339,
// and nothing else. Event payloads stay strict.
func TestRowDatesMatchByInstant(t *testing.T) {
	for _, c := range []struct {
		want, got string
		match     bool
	}{
		{`{"d":"2026-09-15"}`, `{"d":"2026-09-15 00:00:00.000Z"}`, true},
		{`{"d":"2026-09-30T17:00:00Z"}`, `{"d":"2026-09-30 17:00:00.000Z"}`, true},
		{`{"d":"2026-09-15"}`, `{"d":"2026-09-16 00:00:00.000Z"}`, false},
		{`{"d":"2026-09-15"}`, `{"d":"2026-09-15 10:00:00.000Z"}`, false},
		{`{"d":"not a date"}`, `{"d":"2026-09-15 00:00:00.000Z"}`, false},
	} {
		if got := rowDiff([]byte(c.want), []byte(c.got)) == ""; got != c.match {
			t.Errorf("rowDiff(%s, %s) match=%v, want %v", c.want, c.got, got, c.match)
		}
	}
	if subsetDiff([]byte(`{"d":"2026-09-15"}`), []byte(`{"d":"2026-09-15 00:00:00.000Z"}`)) == "" {
		t.Error("an event payload's date matched by instant")
	}
}
