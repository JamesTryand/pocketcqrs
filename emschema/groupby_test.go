package emschema

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/scaffold"
)

// groupByDocument builds a minimal EventModeling document exercising F-20's
// groupBy derivation (schema 2.3.0): a payrollPeriods read model whose
// staffTotals field is a nested list of {staffId, outOfHoursHours} rows, one
// per distinct staff member — mirroring dotnetcqrs's ScenarioVerifierTests
// fixture (two contributing events for one group key proving accumulation,
// a second group key proving a separate entry) and the real
// project/gotimesheets shape this closes the gap for. subfieldDerivation
// lets the toggle-rejection test below reuse the same document shape with
// just the one field swapped.
func groupByDocument(subfieldDerivation *FieldDerivation) *Document {
	return &Document{
		EventModelingSchemaVersion: SchemaVersion,
		ID:                         "d1",
		Name:                       "GroupBy Rollup",
		Swimlanes:                  []Swimlane{{ID: "sys", Name: "System", Kind: "system"}},
		Events: map[string]Event{
			"time-logged": {
				Name: "Time Logged", SwimlaneID: "sys", Aggregate: "PayrollPeriod",
				Fields: []Field{
					{Name: "periodId", Type: "string", IDAttribute: true},
					{Name: "staffId", Type: "string"},
					{Name: "hours", Type: "integer"},
				},
			},
		},
		Commands: map[string]Command{
			"log-time": {
				Name: "Log Time", Aggregate: "PayrollPeriod",
				Fields: []Field{
					{Name: "periodId", Type: "string"},
					{Name: "staffId", Type: "string"},
					{Name: "hours", Type: "integer"},
				},
			},
		},
		ReadModels: map[string]ReadModel{
			"payroll-periods": {
				Name:              "Payroll Periods",
				BuiltFromEventIDs: []string{"time-logged"},
				Fields: []Field{
					{Name: "periodId", Type: "string", IDAttribute: true},
					{Name: "staffTotals", Type: "string", Cardinality: "list",
						Derivation: &FieldDerivation{Kind: DerivationGroupBy, GroupByField: "staffId"},
						Subfields: []Field{
							{Name: "staffId", Type: "string"},
							{Name: "outOfHoursHours", Type: "integer", Derivation: subfieldDerivation},
						},
					},
				},
			},
		},
		Screens: map[string]Screen{
			"scr-log":  {Name: "Log Time Screen"},
			"scr-view": {Name: "View Payroll Periods"},
		},
		Slices: []Slice{
			{
				ID: "log-time-slice", Name: "Log Time", Pattern: PatternStateChange,
				SwimlaneID: "sys", Status: "wireframe", ScreenID: "scr-log",
				CommandID: "log-time", EventIDs: []string{"time-logged"},
			},
			{
				ID: "view-payroll-periods-slice", Name: "View Payroll Periods", Pattern: PatternStateView,
				SwimlaneID: "sys", Status: "wireframe", ScreenID: "scr-view",
				ReadModelID: "payroll-periods",
				Scenarios: []Scenario{
					{
						ID: "manager-sees-unreleased-period-totals", Name: "staff totals accumulate per staff member",
						Kind: KindStateView,
						Given: []EventRef{
							{EventID: "time-logged", Data: json.RawMessage(`{"periodId":"pp1","staffId":"s1","hours":3}`)},
							{EventID: "time-logged", Data: json.RawMessage(`{"periodId":"pp1","staffId":"s1","hours":2}`)},
							{EventID: "time-logged", Data: json.RawMessage(`{"periodId":"pp1","staffId":"s2","hours":4}`)},
						},
						When: json.RawMessage(`{"readModelId":"payroll-periods"}`),
						Then: json.RawMessage(`{"result":{"periodId":"pp1","staffTotals":` +
							`[{"staffId":"s1","outOfHoursHours":5},{"staffId":"s2","outOfHoursHours":4}]}}`),
					},
				},
			},
		},
	}
}

// TestImportAndVerifyGroupByRollup is the round-trip proof this feature
// needs before touching project/gotimesheets's real model, per
// platform/eventmodeling-verify-gaps' own execution plan: Map() must carry
// the groupBy field's subfields through into the generated scaffold.Domain,
// and Verify() must prove — through the real generated JS projection, the
// same path a live import takes — that two contributing events for ONE
// staff member accumulate onto one nested entry while a second staff member
// lands in a separate entry within the same payroll period's row.
func TestImportAndVerifyGroupByRollup(t *testing.T) {
	doc := groupByDocument(&FieldDerivation{
		Kind: DerivationSum, AddOnEventIDs: []string{"time-logged"}, AmountField: "hours",
	})

	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatalf("expected the document to map cleanly: %v", err)
	}
	if len(mapped.Domains) != 1 {
		t.Fatalf("expected one aggregate, got %d", len(mapped.Domains))
	}
	d := mapped.Domains[0]
	if len(d.ReadModels) != 1 {
		t.Fatalf("expected one read model, got %d", len(d.ReadModels))
	}
	rm := d.ReadModels[0]
	var staffTotals *scaffold.Field
	for i, f := range rm.Fields {
		if f.Name == "staffTotals" {
			staffTotals = &rm.Fields[i]
		}
	}
	if staffTotals == nil {
		t.Fatalf("staffTotals field missing from mapped read model: %+v", rm.Fields)
	}
	if staffTotals.Derivation == nil || staffTotals.Derivation.Kind != scaffold.DerivationGroupBy ||
		staffTotals.Derivation.GroupByField != "staffId" {
		t.Fatalf("staffTotals derivation not carried through correctly: %+v", staffTotals.Derivation)
	}
	if len(staffTotals.Subfields) != 2 {
		t.Fatalf("expected 2 subfields (staffId, outOfHoursHours), got %d: %+v", len(staffTotals.Subfields), staffTotals.Subfields)
	}

	results, err := Verify(doc, mapped, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected one scenario result, got %d", len(results))
	}
	res := results[0]
	if !res.Passed {
		t.Fatalf("the groupBy accumulation scenario must pass against the generated projection: %s", res.Detail)
	}
}

// TestGroupByToggleSubfieldRejected mirrors dotnetcqrs's DocumentMapper
// rejection: a toggle subfield inside groupBy is refused with a clear
// mapping error rather than silently generating code that cannot know which
// row to update — ToggleDerivation carries no rowKeyField at all.
func TestGroupByToggleSubfieldRejected(t *testing.T) {
	doc := groupByDocument(&FieldDerivation{
		Kind: DerivationToggle, OnEventIDs: []string{"time-logged"}, OffEventIDs: []string{"time-logged"},
	})

	_, err := Map(doc, Options{})
	if err == nil {
		t.Fatal("expected a toggle groupBy subfield to be refused")
	}
	if !strings.Contains(err.Error(), "outOfHoursHours") || !strings.Contains(err.Error(), "toggle derivation") ||
		!strings.Contains(err.Error(), "not supported inside groupBy") {
		t.Fatalf("the refusal must name the subfield and why: %v", err)
	}
}
