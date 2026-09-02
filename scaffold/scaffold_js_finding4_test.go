package scaffold

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/functions"
)

// TestFinding4JSProjectionDerivations mirrors TestFinding4ProjectionDerivations
// for the JS projection: the groupBy field's own trigger event must route
// to a groupByBump op per subfield (never the generic merge, never a plain
// increment) — see jsDerivedOp's own doc comment for why groupByBump is its
// own op kind rather than a read-modify-write inside project().
func TestFinding4JSProjectionDerivations(t *testing.T) {
	files, err := finding4Domain().Generate()
	if err != nil {
		t.Fatal(err)
	}
	periods := files[1].Source // payrollPeriods
	for _, want := range []string{
		"case 'TimeLogged':",
		"{ groupByBump: { key: (event.data || {}).periodId, field: \"staffTotals\", groupField: \"staffId\", " +
			"groupValue: (event.data || {}).staffId, subfield: \"outOfHoursHours\", delta: ((event.data || {}).hours || 0) * 1 } }",
		"{ groupByBump: { key: (event.data || {}).periodId, field: \"staffTotals\", groupField: \"staffId\", " +
			"groupValue: (event.data || {}).staffId, subfield: \"entriesLogged\", delta: 1 } }",
	} {
		if !strings.Contains(periods, want) {
			t.Errorf("payrollPeriods projection missing %q:\n%s", want, periods)
		}
	}
	if strings.Contains(periods, "applyMerge") {
		t.Errorf("payrollPeriods has no plain merge events; applyMerge must not be generated:\n%s", periods)
	}
}

// TestFinding4JSProjectionEndToEnd is the round-trip proof this feature
// needs (per platform/eventmodeling-verify-gaps' own execution plan): load
// the generated payrollPeriods projection through the real JS runtime and
// fold a fixture through it exactly as emschema/verify.go's runViewScenario
// does for a stateView scenario — two contributing events for ONE staff
// member must accumulate onto one nested entry, and a second staff member
// must land in a SEPARATE entry, entirely from an ISOLATED fixture run (no
// pb reads), the same shape TestFinding3JSProjectionEndToEnd proves for a
// plain count/sum.
func TestFinding4JSProjectionEndToEnd(t *testing.T) {
	files, err := finding4Domain().Generate()
	if err != nil {
		t.Fatal(err)
	}

	rt := functions.NewGojaRuntime(nil)
	periodsSpec, err := functions.LoadProjectionSource(rt, nil, "payrollPeriods.js", files[1].Source)
	if err != nil {
		t.Fatalf("payrollPeriods projection did not load: %v", err)
	}

	// rowKeyField defaults to the read model's own Key ("periodId") since
	// finding4Domain's subfields declare no override — count/sum/groupBy
	// trigger events are usually cross-stream, so the row is found by a
	// PAYLOAD field, not event.aggregateId (see collectDerivedActions'
	// default and finding3Domain's projectId-keyed events for the same
	// shape). AggregateID here is just the triggering event's own id.
	fixture := []events.Event{
		{Position: 1, AggregateID: "t1", Type: "TimeLogged", Data: json.RawMessage(`{"periodId":"pp1","staffId":"s1","hours":3}`)},
		{Position: 2, AggregateID: "t2", Type: "TimeLogged", Data: json.RawMessage(`{"periodId":"pp1","staffId":"s1","hours":2}`)},
		{Position: 3, AggregateID: "t3", Type: "TimeLogged", Data: json.RawMessage(`{"periodId":"pp1","staffId":"s2","hours":4}`)},
	}
	run, err := functions.DryRunProjectionOver(periodsSpec, fixture)
	if err != nil {
		t.Fatalf("payrollPeriods projection failed over the fixture: %v", err)
	}

	rows, ok := run.Rows["payrollPeriods"]["pp1"]["staffTotals"].([]map[string]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("expected 2 staffTotals entries, got %#v (rows: %v)", run.Rows["payrollPeriods"]["pp1"]["staffTotals"], run.Rows)
	}
	byStaff := map[string]map[string]any{}
	for _, r := range rows {
		byStaff[fmt.Sprint(r["staffId"])] = r
	}
	if byStaff["s1"]["outOfHoursHours"] != float64(5) {
		t.Errorf("expected s1 outOfHoursHours=5 (3+2 accumulated), got %v", byStaff["s1"]["outOfHoursHours"])
	}
	if byStaff["s1"]["entriesLogged"] != float64(2) {
		t.Errorf("expected s1 entriesLogged=2 (two TimeLogged events), got %v", byStaff["s1"]["entriesLogged"])
	}
	if byStaff["s2"]["outOfHoursHours"] != float64(4) {
		t.Errorf("expected s2 outOfHoursHours=4 (its own entry), got %v", byStaff["s2"]["outOfHoursHours"])
	}
	if byStaff["s2"]["entriesLogged"] != float64(1) {
		t.Errorf("expected s2 entriesLogged=1 (its own entry), got %v", byStaff["s2"]["entriesLogged"])
	}
}
