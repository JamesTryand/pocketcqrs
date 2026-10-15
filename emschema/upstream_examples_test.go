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
		// HAND: chosen by a hand-written hook, which only a host's own
		// hooks can check; still never a failure
		if !r.Passed && !r.Hand {
			t.Errorf("%s/%s (%s): %s", r.SliceID, r.ScenarioID, r.Name, r.Detail)
		}
	}
}

// handScenarios lists the scenarios verify reported HAND.
func handScenarios(t *testing.T, rel string, opts Options) map[string]bool {
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
	out := map[string]bool{}
	for _, r := range results {
		if r.Hand {
			out[r.ScenarioID] = true
		}
	}
	return out
}

// conditional-outcomes.json (3.12.0): a declared condition (hours over 12
// logs and flags, with set fixing the reason), a refusal checked before
// the outcomes, and a hand-written condition (an expense over the limit),
// which verify reports HAND: only a host's own hook can decide it.
func TestUpstreamConditionalOutcomesExample(t *testing.T) {
	verifyExample(t, "examples/conditional-outcomes.json", Options{})
	hand := handScenarios(t, "examples/conditional-outcomes.json", Options{})
	if !hand["expense-over-limit"] || len(hand) != 1 {
		t.Fatalf("exactly the hand-written condition's scenario is HAND: %v", hand)
	}
}

// repeated-outcomes.json (3.13.0): a release then one payment per person,
// hand-written (HAND), and a read model keyed by payment (rowKeyField).
func TestUpstreamRepeatedOutcomesExample(t *testing.T) {
	verifyExample(t, "examples/repeated-outcomes.json", Options{})
	hand := handScenarios(t, "examples/repeated-outcomes.json", Options{})
	if !hand["two-people-paid"] || !hand["nobody-owed"] || len(hand) != 2 {
		t.Fatalf("the repeated outcome's scenarios are HAND: %v", hand)
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

// refusals.json: refuseWhen rules (3.10.0) with numeric operators (3.11.0),
// each with an error scenario naming its message, and scenarios naming
// their actor and time.
func TestUpstreamRefusalsExample(t *testing.T) {
	verifyExample(t, "examples/refusals.json", Options{})
}

// state-ownership.json: requiredOwnership.stateField (3.15.0), the
// aggregate deciding who owns it from its own state, refusing anyone else
// with the declared message; scenarios name their actor.
func TestUpstreamStateOwnershipExample(t *testing.T) {
	verifyExample(t, "examples/state-ownership.json", Options{})
}

// rows-per-list-item.json (3.16.0): an invoice's lines as a part per
// project, its subtotal once every line is converted (sumOfItems), its
// status by event (byEvent); a discard removes every part.
func TestUpstreamRowsPerListItemExample(t *testing.T) {
	verifyExample(t, "examples/rows-per-list-item.json", Options{})
}

// hand-projection.json (3.17.0): a read model joining two streams, built
// by a hand-written projection. Its view scenario is HAND, and the import
// writes only a .example of the projection.
func TestUpstreamHandProjectionExample(t *testing.T) {
	verifyExample(t, "examples/hand-projection.json", Options{})
	hand := handScenarios(t, "examples/hand-projection.json", Options{})
	if !hand["an-order-after-dispatch-closed-is-late"] || len(hand) != 1 {
		t.Fatalf("the hand projection's view scenario is HAND: %v", hand)
	}
	doc, err := Load(fixture(t, "examples/hand-projection.json"))
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range mapped.Domains {
		files, err := d.Generate()
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if f.Name == "late_orders.js" || f.Name == "lateOrders.js" {
				t.Errorf("%s was generated for a hand projection", f.Name)
			}
		}
	}
}
