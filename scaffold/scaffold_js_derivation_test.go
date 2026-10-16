package scaffold

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/functions"
)

// TestDerivationJSEndsStreamResetsExists mirrors
// TestDerivationEndsStreamResetsExists (scaffold_go_test.go) for the JS
// decider: an endsStream event must emit `exists: false` in evolve(), not
// the unconditional `true` every other event gets.
func TestDerivationJSEndsStreamResetsExists(t *testing.T) {
	files, err := derivationDomain().Generate()
	if err != nil {
		t.Fatal(err)
	}
	dec := files[0].Source
	// CreateMember may not begin a removed member's stream again, so
	// the removal also sets ended
	if !strings.Contains(dec, "case 'MemberRemoved':\n      return Object.assign({}, state, event.data, { exists: false, ended: true });") {
		t.Errorf("expected MemberRemoved to reset exists to false and set ended:\n%s", dec)
	}
	if !strings.Contains(dec, "if (state.ended) { throw new Error('member has ended and cannot begin again'); }") {
		t.Errorf("expected CreateMember to refuse an ended stream:\n%s", dec)
	}
	if !strings.Contains(dec, "case 'MemberCreated':\n      return Object.assign({}, state, event.data, { exists: true });") {
		t.Errorf("a non-endsStream event must still set exists true:\n%s", dec)
	}
}

// TestDerivationJSProjectionDerivations mirrors TestDerivationProjectionDerivations
// for the JS projection: each derivation trigger event must route to its own
// op (never the generic merge), and a plain event must still go through
// applyMerge.
func TestDerivationJSProjectionDerivations(t *testing.T) {
	files, err := derivationDomain().Generate()
	if err != nil {
		t.Fatal(err)
	}
	directory := files[1].Source // memberDirectory
	for _, want := range []string{
		"case 'MemberSsoEnabled':",
		"{ upsert: { key: event.aggregateId, fields: { ssoEnabled: true } } }",
		"case 'MemberSsoDisabled':",
		"{ upsert: { key: event.aggregateId, fields: { ssoEnabled: false } } }",
		"case 'MemberCreated':",
		"return applyMerge(event);",
	} {
		if !strings.Contains(directory, want) {
			t.Errorf("memberDirectory projection missing %q:\n%s", want, directory)
		}
	}

	teams := files[2].Source // teams
	for _, want := range []string{
		"case 'MemberAssignedToTeam':",
		"{ increment: { key: (event.data || {}).teamId, field: \"memberCount\", delta: 1 } }",
		"case 'MemberUnassignedFromTeam':",
		"{ increment: { key: (event.data || {}).teamId, field: \"memberCount\", delta: -1 } }",
		"case 'SaleRecorded':",
		"delta: ((event.data || {}).units || 0) * 1",
		"case 'SaleReversed':",
		"delta: ((event.data || {}).units || 0) * -1",
	} {
		if !strings.Contains(teams, want) {
			t.Errorf("teams projection missing %q:\n%s", want, teams)
		}
	}
	if strings.Contains(teams, "applyMerge") {
		t.Errorf("teams has no plain merge events; applyMerge must not be generated:\n%s", teams)
	}
}

// TestDerivationJSScopeResolver mirrors TestDerivationScopeResolver: a read
// model with a declared scope, even with no derivation at all, gets a
// resolve<Param> helper querying the via collection through pb.query.
func TestDerivationJSScopeResolver(t *testing.T) {
	files, err := derivationDomain().Generate()
	if err != nil {
		t.Fatal(err)
	}
	flagged := files[3].Source
	for _, want := range []string{
		"function resolveLeadMemberId(value) {",
		`pb.query("teamLeads", "memberId = '" + value + "'", 500)`,
		"out.push(rows[i].teamId);",
	} {
		if !strings.Contains(flagged, want) {
			t.Errorf("escalatedTickets projection missing %q:\n%s", want, flagged)
		}
	}
}

// TestDerivationJSProjectionEndToEnd is the actual verify-shaped proof: load
// the generated memberDirectory (toggle) and teams (count/sum) projections
// through the real JS runtime and fold a fixture through each, exactly as
// emschema/verify.go's runViewScenario does for a stateView scenario. This
// is what lets toggle/count/sum verify scenarios PASS against JS-generated
// output, not just Go's.
func TestDerivationJSProjectionEndToEnd(t *testing.T) {
	files, err := derivationDomain().Generate()
	if err != nil {
		t.Fatal(err)
	}

	rt := functions.NewGojaRuntime(nil)

	directorySpec, err := functions.LoadProjectionSource(rt, nil, "memberDirectory.js", files[1].Source)
	if err != nil {
		t.Fatalf("memberDirectory projection did not load: %v", err)
	}
	directoryFixture := []events.Event{
		{Position: 1, AggregateID: "m1", Type: "MemberCreated", Data: json.RawMessage(`{"name":"Ada"}`)},
		{Position: 2, AggregateID: "m1", Type: "MemberSsoEnabled", Data: json.RawMessage(`{}`)},
	}
	directoryRun, err := functions.DryRunProjectionOver(directorySpec, directoryFixture)
	if err != nil {
		t.Fatalf("memberDirectory projection failed over the fixture: %v", err)
	}
	row := directoryRun.Rows["memberDirectory"]["m1"]
	if row["name"] != "Ada" || row["ssoEnabled"] != true {
		t.Fatalf("unexpected memberDirectory row after create+enable: %v", row)
	}

	teamsSpec, err := functions.LoadProjectionSource(rt, nil, "teams.js", files[2].Source)
	if err != nil {
		t.Fatalf("teams projection did not load: %v", err)
	}
	// two members assigned, one unassigned, two sales recorded and one
	// reversed -- must net to memberCount=1, totalUnits=3, entirely from an
	// ISOLATED fixture run (no pb reads), the same shape verify.go uses.
	teamsFixture := []events.Event{
		{Position: 1, AggregateID: "a1", Type: "MemberAssignedToTeam", Data: json.RawMessage(`{"teamId":"g1"}`)},
		{Position: 2, AggregateID: "a2", Type: "MemberAssignedToTeam", Data: json.RawMessage(`{"teamId":"g1"}`)},
		{Position: 3, AggregateID: "a1", Type: "MemberUnassignedFromTeam", Data: json.RawMessage(`{"teamId":"g1"}`)},
		{Position: 4, AggregateID: "t1", Type: "SaleRecorded", Data: json.RawMessage(`{"teamId":"g1","units":5}`)},
		{Position: 5, AggregateID: "t2", Type: "SaleRecorded", Data: json.RawMessage(`{"teamId":"g1","units":2}`)},
		{Position: 6, AggregateID: "t1", Type: "SaleReversed", Data: json.RawMessage(`{"teamId":"g1","units":4}`)},
	}
	teamsRun, err := functions.DryRunProjectionOver(teamsSpec, teamsFixture)
	if err != nil {
		t.Fatalf("teams projection failed over the fixture: %v", err)
	}
	teamRow := teamsRun.Rows["teams"]["g1"]
	if teamRow["memberCount"] != float64(1) {
		t.Errorf("expected memberCount=1, got %v (row: %v)", teamRow["memberCount"], teamRow)
	}
	if teamRow["totalUnits"] != float64(3) {
		t.Errorf("expected totalUnits=3, got %v (row: %v)", teamRow["totalUnits"], teamRow)
	}

	if _, err := functions.LoadDeciderSource(rt, "member.js", files[0].Source); err != nil {
		t.Fatalf("member decider did not load: %v", err)
	}
}
