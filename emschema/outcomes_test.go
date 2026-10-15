package emschema

import (
	"strings"
	"testing"
)

func outcomeDoc(outcomes string) string {
	return `{"eventModelingSchemaVersion":"3.17.0","id":"o","name":"O",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{"a":{"name":"A","swimlaneId":"s","aggregate":"x","fields":[{"name":"n","type":"decimal"}]},
		"b":{"name":"B","swimlaneId":"s","aggregate":"x","fields":[{"name":"n","type":"decimal"}]}},
	"commands":{"c":{"name":"C","aggregate":"x","fields":[{"name":"n","type":"decimal"}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[{"id":"sl","name":"Sl","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc",
		"commandId":"c","eventIds":["a","b"],"outcomes":` + outcomes + `,"scenarios":[]}]}`
}

// Outcomes are tried in order, so every alternative but the last says when
// it applies and the last (the default) doesn't. Several with no when at
// all (3.3.0's form) say what may happen but not which: refused, as by
// dotnetcqrs. One alone needs no when.
func TestOutcomeOrderIsChecked(t *testing.T) {
	gt := `[{"all":[{"command":"n","greaterThan":1}]}]`
	for name, c := range map[string]struct {
		outcomes, wantErr string
	}{
		"one alone":             {`[["a"]]`, ""},
		"3.3.0 bare, several":   {`[["a"],["b"]]`, "not which applies"},
		"declared then default": {`[{"eventIds":["a","b"],"when":` + gt + `},["a"]]`, ""},
		"default not last":      {`[["a"],{"eventIds":["b"],"when":` + gt + `}]`, "must have no when"},
		"unknown event":         {`[["zz"]]`, "not one of the slice's events"},
		"repeated not listed":   {`[{"eventIds":["a"],"repeated":["b"]}]`, "not one of this outcome's events"},
		"set wrong type":        {`[{"eventIds":["a"],"set":{"a":{"n":"text"}}}]`, "a text value"},
	} {
		doc, err := Load(writeDoc(t, outcomeDoc(c.outcomes)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_, err = Map(doc, Options{})
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected %v", name, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: want an error containing %q, got %v", name, c.wantErr, err)
		}
	}
}
