package scaffold

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/functions"
)

// TestGroupByJSProjectionDerivations mirrors TestGroupByProjectionDerivations
// for the JS projection: the groupBy field's own trigger event must route
// to a groupByBump op per subfield (never the generic merge, never a plain
// increment) — see jsDerivedOp's own doc comment for why groupByBump is its
// own op kind rather than a read-modify-write inside project().
func TestGroupByJSProjectionDerivations(t *testing.T) {
	files, err := groupByDomain().Generate()
	if err != nil {
		t.Fatal(err)
	}
	periods := files[1].Source // salesPeriods
	for _, want := range []string{
		"case 'SaleRecorded':",
		"{ groupByBump: { key: (event.data || {}).periodId, field: \"memberTotals\", groupField: \"memberId\", " +
			"groupValue: (event.data || {}).memberId, subfield: \"unitsSold\", delta: ((event.data || {}).units || 0) * 1 } }",
		"{ groupByBump: { key: (event.data || {}).periodId, field: \"memberTotals\", groupField: \"memberId\", " +
			"groupValue: (event.data || {}).memberId, subfield: \"salesRecorded\", delta: 1 } }",
	} {
		if !strings.Contains(periods, want) {
			t.Errorf("salesPeriods projection missing %q:\n%s", want, periods)
		}
	}
	if strings.Contains(periods, "applyMerge") {
		t.Errorf("salesPeriods has no plain merge events; applyMerge must not be generated:\n%s", periods)
	}
}

// TestGroupByJSProjectionEndToEnd is the round-trip proof this feature
// needs: load
// the generated salesPeriods projection through the real JS runtime and
// fold a fixture through it exactly as emschema/verify.go's runViewScenario
// does for a stateView scenario — two contributing events for ONE
// member must accumulate onto one nested entry, and a second member
// must land in a SEPARATE entry, entirely from an ISOLATED fixture run (no
// pb reads), the same shape TestDerivationJSProjectionEndToEnd proves for a
// plain count/sum.
func TestGroupByJSProjectionEndToEnd(t *testing.T) {
	files, err := groupByDomain().Generate()
	if err != nil {
		t.Fatal(err)
	}

	rt := functions.NewGojaRuntime(nil)
	periodsSpec, err := functions.LoadProjectionSource(rt, nil, "salesPeriods.js", files[1].Source)
	if err != nil {
		t.Fatalf("salesPeriods projection did not load: %v", err)
	}

	// rowKeyField defaults to the read model's own Key ("periodId") since
	// groupByDomain's subfields declare no override — count/sum/groupBy
	// trigger events are usually cross-stream, so the row is found by a
	// PAYLOAD field, not event.aggregateId (see collectDerivedActions'
	// default and derivationDomain's teamId-keyed events for the same
	// shape). AggregateID here is just the triggering event's own id.
	fixture := []events.Event{
		{Position: 1, AggregateID: "t1", Type: "SaleRecorded", Data: json.RawMessage(`{"periodId":"pp1","memberId":"m1","units":3}`)},
		{Position: 2, AggregateID: "t2", Type: "SaleRecorded", Data: json.RawMessage(`{"periodId":"pp1","memberId":"m1","units":2}`)},
		{Position: 3, AggregateID: "t3", Type: "SaleRecorded", Data: json.RawMessage(`{"periodId":"pp1","memberId":"m2","units":4}`)},
	}
	run, err := functions.DryRunProjectionOver(periodsSpec, fixture)
	if err != nil {
		t.Fatalf("salesPeriods projection failed over the fixture: %v", err)
	}

	rows, ok := run.Rows["salesPeriods"]["pp1"]["memberTotals"].([]map[string]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("expected 2 memberTotals entries, got %#v (rows: %v)", run.Rows["salesPeriods"]["pp1"]["memberTotals"], run.Rows)
	}
	byMember := map[string]map[string]any{}
	for _, r := range rows {
		byMember[fmt.Sprint(r["memberId"])] = r
	}
	if byMember["m1"]["unitsSold"] != float64(5) {
		t.Errorf("expected m1 unitsSold=5 (3+2 accumulated), got %v", byMember["m1"]["unitsSold"])
	}
	if byMember["m1"]["salesRecorded"] != float64(2) {
		t.Errorf("expected m1 salesRecorded=2 (two SaleRecorded events), got %v", byMember["m1"]["salesRecorded"])
	}
	if byMember["m2"]["unitsSold"] != float64(4) {
		t.Errorf("expected m2 unitsSold=4 (its own entry), got %v", byMember["m2"]["unitsSold"])
	}
	if byMember["m2"]["salesRecorded"] != float64(1) {
		t.Errorf("expected m2 salesRecorded=1 (its own entry), got %v", byMember["m2"]["salesRecorded"])
	}
}
