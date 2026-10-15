package emschema

import (
	"strings"
	"testing"
)

// verifyExample imports one of upstream's worked examples and runs its
// scenarios. An example this importer applies in full must pass every one
// of its own scenarios: they are the schema project's statement of what the
// features mean.
func verifyExample(t *testing.T, rel string, opts Options) {
	t.Helper()
	doc, err := Load(fixture(t, rel))
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := Map(doc, opts)
	if err != nil {
		t.Fatal(err)
	}
	results, err := Verify(doc, mapped, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("the example has no scenarios")
	}
	for _, r := range results {
		if !r.Passed {
			t.Errorf("%s/%s (%s): %s", r.SliceID, r.ScenarioID, r.Name, r.Detail)
		}
	}
}

// read-access.json: selfAccess, a granting scope, callers, and an unassign
// that removes the project-manager row (and with it the access it granted).
func TestUpstreamReadAccessExample(t *testing.T) {
	verifyExample(t, "examples/read-access.json", Options{SubjectField: "staffId"})
}

// A removal the read model is not built from would never reach its
// projection, leaving the row (and any access it grants) behind: refused.
func TestRemovalMustBeBuiltFrom(t *testing.T) {
	doc, err := Load(writeDoc(t, `{"eventModelingSchemaVersion":"3.17.0","id":"r","name":"R",
		"swimlanes":[{"id":"s","name":"S","kind":"team"}],
		"events":{"pm-assigned":{"name":"Pm Assigned","swimlaneId":"s","aggregate":"assignment"},
			"pm-unassigned":{"name":"Pm Unassigned","swimlaneId":"s","aggregate":"assignment"}},
		"commands":{"assign":{"name":"Assign","aggregate":"assignment"}},
		"readModels":{"pms":{"name":"Pms","builtFromEventIds":["pm-assigned"],"removedByEventIds":["pm-unassigned"],
			"fields":[{"name":"assignmentId","type":"string"}]}},
		"screens":{"sc":{"name":"Sc"}},
		"slices":[{"id":"a","name":"A","pattern":"stateChange","swimlaneId":"s","status":"planned",
			"screenId":"sc","commandId":"assign","eventIds":["pm-assigned"],"scenarios":[]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Map(doc, Options{}); err == nil || !strings.Contains(err.Error(), "pm-unassigned") {
		t.Fatalf("want a refusal naming pm-unassigned, got %v", err)
	}
}
