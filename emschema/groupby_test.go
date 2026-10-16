package emschema

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/scaffold"
)

// groupByDocument builds a minimal EventModeling document exercising the
// groupBy derivation (schema 2.3.0): a salesPeriods read model whose
// memberTotals field is a nested list of {memberId, unitsSold} rows, one
// per distinct member — mirroring dotnetcqrs's ScenarioVerifierTests
// fixture (two contributing events for one group key proving accumulation,
// a second group key proving a separate entry). subfieldDerivation
// lets the toggle-rejection test below reuse the same document shape with
// just the one field swapped.
func groupByDocument(subfieldDerivation *FieldDerivation) *Document {
	return &Document{
		EventModelingSchemaVersion: SchemaVersion,
		ID:                         "d1",
		Name:                       "GroupBy Rollup",
		Swimlanes:                  []Swimlane{{ID: "sys", Name: "System", Kind: "system"}},
		Events: map[string]Event{
			"sale-recorded": {
				Name: "Sale Recorded", SwimlaneID: "sys", Aggregate: "SalesPeriod",
				Fields: []Field{
					{Name: "periodId", Type: "string", IDAttribute: true},
					{Name: "memberId", Type: "string"},
					{Name: "units", Type: "integer"},
				},
			},
		},
		Commands: map[string]Command{
			"record-sale": {
				Name: "Record Sale", Aggregate: "SalesPeriod",
				Fields: []Field{
					{Name: "periodId", Type: "string"},
					{Name: "memberId", Type: "string"},
					{Name: "units", Type: "integer"},
				},
			},
		},
		ReadModels: map[string]ReadModel{
			"sales-periods": {
				Name:              "Sales Periods",
				BuiltFromEventIDs: []string{"sale-recorded"},
				Fields: []Field{
					{Name: "periodId", Type: "string", IDAttribute: true},
					{Name: "memberTotals", Type: "string", Cardinality: "list",
						Derivation: &FieldDerivation{Kind: DerivationGroupBy, GroupByField: "memberId"},
						Subfields: []Field{
							{Name: "memberId", Type: "string"},
							{Name: "unitsSold", Type: "integer", Derivation: subfieldDerivation},
						},
					},
				},
			},
		},
		Screens: map[string]Screen{
			"scr-log":  {Name: "Record Sale Screen"},
			"scr-view": {Name: "View Sales Periods"},
		},
		Slices: []Slice{
			{
				ID: "record-sale-slice", Name: "Record Sale", Pattern: PatternStateChange,
				SwimlaneID: "sys", Status: "wireframe", ScreenID: "scr-log",
				CommandID: "record-sale", EventIDs: []string{"sale-recorded"},
			},
			{
				ID: "view-sales-periods-slice", Name: "View Sales Periods", Pattern: PatternStateView,
				SwimlaneID: "sys", Status: "wireframe", ScreenID: "scr-view",
				ReadModelID: "sales-periods",
				Scenarios: []Scenario{
					{
						ID: "supervisor-sees-open-period-totals", Name: "member totals accumulate per member",
						Kind: KindStateView,
						Given: []EventRef{
							{EventID: "sale-recorded", Data: json.RawMessage(`{"periodId":"pp1","memberId":"m1","units":3}`)},
							{EventID: "sale-recorded", Data: json.RawMessage(`{"periodId":"pp1","memberId":"m1","units":2}`)},
							{EventID: "sale-recorded", Data: json.RawMessage(`{"periodId":"pp1","memberId":"m2","units":4}`)},
						},
						When: json.RawMessage(`{"readModelId":"sales-periods"}`),
						Then: json.RawMessage(`{"result":{"periodId":"pp1","memberTotals":` +
							`[{"memberId":"m1","unitsSold":5},{"memberId":"m2","unitsSold":4}]}}`),
					},
				},
			},
		},
	}
}

// TestImportAndVerifyGroupByRollup is the round-trip proof this feature
// needs: Map() must carry
// the groupBy field's subfields through into the generated scaffold.Domain,
// and Verify() must prove — through the real generated JS projection, the
// same path a live import takes — that two contributing events for ONE
// member accumulate onto one nested entry while a second member lands in a
// separate entry within the same sales period's row.
func TestImportAndVerifyGroupByRollup(t *testing.T) {
	doc := groupByDocument(&FieldDerivation{
		Kind: DerivationSum, AddOnEventIDs: []string{"sale-recorded"}, AmountField: "units",
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
	var memberTotals *scaffold.Field
	for i, f := range rm.Fields {
		if f.Name == "memberTotals" {
			memberTotals = &rm.Fields[i]
		}
	}
	if memberTotals == nil {
		t.Fatalf("memberTotals field missing from mapped read model: %+v", rm.Fields)
	}
	if memberTotals.Derivation == nil || memberTotals.Derivation.Kind != scaffold.DerivationGroupBy ||
		memberTotals.Derivation.GroupByField != "memberId" {
		t.Fatalf("memberTotals derivation not carried through correctly: %+v", memberTotals.Derivation)
	}
	if len(memberTotals.Subfields) != 2 {
		t.Fatalf("expected 2 subfields (memberId, unitsSold), got %d: %+v", len(memberTotals.Subfields), memberTotals.Subfields)
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
		Kind: DerivationToggle, OnEventIDs: []string{"sale-recorded"}, OffEventIDs: []string{"sale-recorded"},
	})

	_, err := Map(doc, Options{})
	if err == nil {
		t.Fatal("expected a toggle groupBy subfield to be refused")
	}
	if !strings.Contains(err.Error(), "unitsSold") || !strings.Contains(err.Error(), "toggle derivation") ||
		!strings.Contains(err.Error(), "not supported inside groupBy") {
		t.Fatalf("the refusal must name the subfield and why: %v", err)
	}
}
