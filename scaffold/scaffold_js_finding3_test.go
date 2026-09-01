package scaffold

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/functions"
)

// TestFinding3JSEndsStreamResetsExists mirrors
// TestFinding3EndsStreamResetsExists (scaffold_go_test.go) for the JS
// decider: an endsStream event must emit `exists: false` in evolve(), not
// the unconditional `true` every other event gets.
func TestFinding3JSEndsStreamResetsExists(t *testing.T) {
	files, err := finding3Domain().Generate()
	if err != nil {
		t.Fatal(err)
	}
	dec := files[0].Source
	if !strings.Contains(dec, "case 'StaffRemoved':\n      return Object.assign({}, state, event.data, { exists: false });") {
		t.Errorf("expected StaffRemoved to reset exists to false:\n%s", dec)
	}
	if !strings.Contains(dec, "case 'StaffCreated':\n      return Object.assign({}, state, event.data, { exists: true });") {
		t.Errorf("a non-endsStream event must still set exists true:\n%s", dec)
	}
}

// TestFinding3JSProjectionDerivations mirrors TestFinding3ProjectionDerivations
// for the JS projection: each derivation trigger event must route to its own
// op (never the generic merge), and a plain event must still go through
// applyMerge.
func TestFinding3JSProjectionDerivations(t *testing.T) {
	files, err := finding3Domain().Generate()
	if err != nil {
		t.Fatal(err)
	}
	roster := files[1].Source // staffRoster
	for _, want := range []string{
		"case 'StaffSsoEnabled':",
		"{ upsert: { key: event.aggregateId, fields: { ssoEnabled: true } } }",
		"case 'StaffSsoDisabled':",
		"{ upsert: { key: event.aggregateId, fields: { ssoEnabled: false } } }",
		"case 'StaffCreated':",
		"return applyMerge(event);",
	} {
		if !strings.Contains(roster, want) {
			t.Errorf("staffRoster projection missing %q:\n%s", want, roster)
		}
	}

	projects := files[2].Source // projects
	for _, want := range []string{
		"case 'StaffAssignedToProject':",
		"{ increment: { key: (event.data || {}).projectId, field: \"staffCount\", delta: 1 } }",
		"case 'StaffUnassignedFromProject':",
		"{ increment: { key: (event.data || {}).projectId, field: \"staffCount\", delta: -1 } }",
		"case 'TimeLogged':",
		"delta: ((event.data || {}).hours || 0) * 1",
		"case 'TimeReversed':",
		"delta: ((event.data || {}).hours || 0) * -1",
	} {
		if !strings.Contains(projects, want) {
			t.Errorf("projects projection missing %q:\n%s", want, projects)
		}
	}
	if strings.Contains(projects, "applyMerge") {
		t.Errorf("projects has no plain merge events; applyMerge must not be generated:\n%s", projects)
	}
}

// TestFinding3JSScopeResolver mirrors TestFinding3ScopeResolver: a read
// model with a declared scope, even with no derivation at all, gets a
// resolve<Param> helper querying the via collection through pb.query.
func TestFinding3JSScopeResolver(t *testing.T) {
	files, err := finding3Domain().Generate()
	if err != nil {
		t.Fatal(err)
	}
	flagged := files[3].Source
	for _, want := range []string{
		"function resolvePmStaffId(value) {",
		`pb.query("projectManagers", "staffId = '" + value + "'", 500)`,
		"out.push(rows[i].projectId);",
	} {
		if !strings.Contains(flagged, want) {
			t.Errorf("flaggedEntries projection missing %q:\n%s", want, flagged)
		}
	}
}

// TestFinding3JSProjectionEndToEnd is the actual verify-shaped proof: load
// the generated staffRoster (toggle) and projects (count/sum) projections
// through the real JS runtime and fold a fixture through each, exactly as
// emschema/verify.go's runViewScenario does for a stateView scenario. This
// is what closes the gap progress.md flagged — toggle/count/sum verify
// scenarios can now move to PASS against JS-generated output, not just Go's.
func TestFinding3JSProjectionEndToEnd(t *testing.T) {
	files, err := finding3Domain().Generate()
	if err != nil {
		t.Fatal(err)
	}

	rt := functions.NewGojaRuntime(nil)

	rosterSpec, err := functions.LoadProjectionSource(rt, nil, "staffRoster.js", files[1].Source)
	if err != nil {
		t.Fatalf("staffRoster projection did not load: %v", err)
	}
	rosterFixture := []events.Event{
		{Position: 1, AggregateID: "s1", Type: "StaffCreated", Data: json.RawMessage(`{"name":"Ada"}`)},
		{Position: 2, AggregateID: "s1", Type: "StaffSsoEnabled", Data: json.RawMessage(`{}`)},
	}
	rosterRun, err := functions.DryRunProjectionOver(rosterSpec, rosterFixture)
	if err != nil {
		t.Fatalf("staffRoster projection failed over the fixture: %v", err)
	}
	row := rosterRun.Rows["staffRoster"]["s1"]
	if row["name"] != "Ada" || row["ssoEnabled"] != true {
		t.Fatalf("unexpected staffRoster row after create+enable: %v", row)
	}

	projectsSpec, err := functions.LoadProjectionSource(rt, nil, "projects.js", files[2].Source)
	if err != nil {
		t.Fatalf("projects projection did not load: %v", err)
	}
	// two staff assigned, one unassigned, two time entries logged and one
	// reversed -- must net to staffCount=1, totalHours=3, entirely from an
	// ISOLATED fixture run (no pb reads), the same shape verify.go uses.
	projectsFixture := []events.Event{
		{Position: 1, AggregateID: "a1", Type: "StaffAssignedToProject", Data: json.RawMessage(`{"projectId":"p1"}`)},
		{Position: 2, AggregateID: "a2", Type: "StaffAssignedToProject", Data: json.RawMessage(`{"projectId":"p1"}`)},
		{Position: 3, AggregateID: "a1", Type: "StaffUnassignedFromProject", Data: json.RawMessage(`{"projectId":"p1"}`)},
		{Position: 4, AggregateID: "t1", Type: "TimeLogged", Data: json.RawMessage(`{"projectId":"p1","hours":5}`)},
		{Position: 5, AggregateID: "t2", Type: "TimeLogged", Data: json.RawMessage(`{"projectId":"p1","hours":2}`)},
		{Position: 6, AggregateID: "t1", Type: "TimeReversed", Data: json.RawMessage(`{"projectId":"p1","hours":4}`)},
	}
	projectsRun, err := functions.DryRunProjectionOver(projectsSpec, projectsFixture)
	if err != nil {
		t.Fatalf("projects projection failed over the fixture: %v", err)
	}
	projectRow := projectsRun.Rows["projects"]["p1"]
	if projectRow["staffCount"] != float64(1) {
		t.Errorf("expected staffCount=1, got %v (row: %v)", projectRow["staffCount"], projectRow)
	}
	if projectRow["totalHours"] != float64(3) {
		t.Errorf("expected totalHours=3, got %v (row: %v)", projectRow["totalHours"], projectRow)
	}

	if _, err := functions.LoadDeciderSource(rt, "staff.js", files[0].Source); err != nil {
		t.Fatalf("staff decider did not load: %v", err)
	}
}
