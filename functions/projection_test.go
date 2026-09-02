package functions

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/events"
)

// TestMultiCollectionProjectionApply loads a two-schema projection against a
// real app, reconciles both collections and verifies that ops route by
// their collection attribute — and that unrouted ops are rejected.
func TestMultiCollectionProjectionApply(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	rt := NewGojaRuntime(nil)
	src := `//@trigger projection sales on SalePlaced
//@schema customers cust:text total:number
//@key cust
//@schema products sku:text sold:number
//@key sku
function project(event) {
	return [
		{ collection: "customers", upsert: { key: event.data.cust, fields: { total: event.data.qty } } },
		{ collection: "products", upsert: { key: event.data.sku, fields: { sold: event.data.qty } } },
	];
}
`
	spec, err := LoadProjectionFile(rt, app, writeTempFn(t, "sales.js", src))
	if err != nil {
		t.Fatal(err)
	}
	if err := ReconcileSchemas(app, []*ProjectionSpec{spec}, ""); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"customers", "products"} {
		if _, err := app.FindCollectionByNameOrId(name); err != nil {
			t.Fatalf("collection %s not reconciled: %v", name, err)
		}
	}

	ev := events.Event{
		Position: 1,
		Type:     "SalePlaced",
		Data:     json.RawMessage(`{"cust":"c1","sku":"p1","qty":2}`),
	}
	if err := spec.Consumer().Apply(context.Background(), ev); err != nil {
		t.Fatal(err)
	}

	cust, err := app.FindFirstRecordByData("customers", "cust", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if cust.GetInt("total") != 2 {
		t.Fatalf("unexpected customers row: total=%d", cust.GetInt("total"))
	}
	prod, err := app.FindFirstRecordByData("products", "sku", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if prod.GetInt("sold") != 2 {
		t.Fatalf("unexpected products row: sold=%d", prod.GetInt("sold"))
	}

	// idempotent: re-applying the same event keeps one row per collection
	if err := spec.Consumer().Apply(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	recs, err := app.FindRecordsByFilter("customers", "", "", -1, 0)
	if err != nil || len(recs) != 1 {
		t.Fatalf("expected 1 customers row, got %d (err=%v)", len(recs), err)
	}

	// an op without collection is ambiguous and blocks
	ambSpec, err := LoadProjectionFile(rt, app, writeTempFn(t, "amb.js", `//@trigger projection amb on SalePlaced
//@schema customers cust:text
//@key cust
//@schema products sku:text
//@key sku
function project(event) { return { upsert: { key: "x", fields: {} } }; }
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := ambSpec.Consumer().Apply(context.Background(), ev); err == nil {
		t.Fatal("expected ambiguity error")
	}
}

// TestSingleCollectionProjectionApply verifies the zero-config path: with
// one declared schema, ops need no collection attribute.
func TestSingleCollectionProjectionApply(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	rt := NewGojaRuntime(nil)
	spec, err := LoadProjectionFile(rt, app, writeTempFn(t, "counts.js", `//@trigger projection counts on OrderPlaced
//@schema counts customerRef:text n:number
//@key customerRef
function project(event) { return { upsert: { key: event.data.customerRef, fields: { n: 1 } } }; }
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := ReconcileSchemas(app, []*ProjectionSpec{spec}, ""); err != nil {
		t.Fatal(err)
	}

	ev := events.Event{
		Position: 1,
		Type:     "OrderPlaced",
		Data:     json.RawMessage(`{"customerRef":"c9"}`),
	}
	if err := spec.Consumer().Apply(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	rec, err := app.FindFirstRecordByData("counts", "customerRef", "c9")
	if err != nil {
		t.Fatal(err)
	}
	if rec.GetInt("n") != 1 {
		t.Fatalf("unexpected row: n=%d", rec.GetInt("n"))
	}
}

// TestIncrementOpAccumulatesLive is Finding 3's count derivation against a
// real app: two increments and one decrement, delivered as three separate
// Apply calls (the live shape — each event its own call, not one fixture
// batch), must net to 1 on the real saved record, and a fresh row starts
// from zero with no explicit seeding.
func TestIncrementOpAccumulatesLive(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	rt := NewGojaRuntime(nil)
	spec, err := LoadProjectionFile(rt, app, writeTempFn(t, "projects.js", `//@trigger projection projects on StaffAssignedToProject StaffUnassignedFromProject
//@schema projects projectId:text staffCount:number
//@key projectId
function project(event) {
	switch (event.type) {
	case 'StaffAssignedToProject':
		return { increment: { key: event.data.projectId, field: 'staffCount', delta: 1 } };
	case 'StaffUnassignedFromProject':
		return { increment: { key: event.data.projectId, field: 'staffCount', delta: -1 } };
	}
}
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := ReconcileSchemas(app, []*ProjectionSpec{spec}, ""); err != nil {
		t.Fatal(err)
	}

	for _, ev := range []events.Event{
		{Position: 1, Type: "StaffAssignedToProject", Data: json.RawMessage(`{"projectId":"p1"}`)},
		{Position: 2, Type: "StaffAssignedToProject", Data: json.RawMessage(`{"projectId":"p1"}`)},
		{Position: 3, Type: "StaffUnassignedFromProject", Data: json.RawMessage(`{"projectId":"p1"}`)},
	} {
		if err := spec.Consumer().Apply(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}

	rec, err := app.FindFirstRecordByData("projects", "projectId", "p1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.GetInt("staffCount") != 1 {
		t.Fatalf("unexpected row: staffCount=%d", rec.GetInt("staffCount"))
	}
}

// TestGroupByBumpOpAccumulatesLive is Finding 4's groupBy derivation
// (schema 2.3.0) against a real app: two contributing events for the SAME
// group key must accumulate onto ONE nested entry, and a different group
// key must land in a SEPARATE entry within the same row's own list — each
// delivered as its own Apply call (the live shape), mirroring
// TestIncrementOpAccumulatesLive one JSON level deeper.
func TestGroupByBumpOpAccumulatesLive(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	rt := NewGojaRuntime(nil)
	spec, err := LoadProjectionFile(rt, app, writeTempFn(t, "payrollPeriods.js", `//@trigger projection payrollPeriods on TimeLogged
//@schema payrollPeriods periodId:text staffTotals:json
//@key periodId
function project(event) {
	return { groupByBump: {
		key: event.data.periodId,
		field: 'staffTotals',
		groupField: 'staffId',
		groupValue: event.data.staffId,
		subfield: 'outOfHoursHours',
		delta: event.data.hours,
	} };
}
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := ReconcileSchemas(app, []*ProjectionSpec{spec}, ""); err != nil {
		t.Fatal(err)
	}

	for _, ev := range []events.Event{
		{Position: 1, Type: "TimeLogged", Data: json.RawMessage(`{"periodId":"pp1","staffId":"s1","hours":3}`)},
		{Position: 2, Type: "TimeLogged", Data: json.RawMessage(`{"periodId":"pp1","staffId":"s1","hours":2}`)},
		{Position: 3, Type: "TimeLogged", Data: json.RawMessage(`{"periodId":"pp1","staffId":"s2","hours":4}`)},
	} {
		if err := spec.Consumer().Apply(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}

	rec, err := app.FindFirstRecordByData("payrollPeriods", "periodId", "pp1")
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := rec.UnmarshalJSONField("staffTotals", &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 staffTotals entries, got %d: %v", len(rows), rows)
	}
	byStaff := map[string]float64{}
	for _, r := range rows {
		byStaff[fmt.Sprint(r["staffId"])], _ = toFloat(r["outOfHoursHours"])
	}
	if byStaff["s1"] != 5 {
		t.Errorf("expected s1 outOfHoursHours=5 (3+2 accumulated), got %v", byStaff["s1"])
	}
	if byStaff["s2"] != 4 {
		t.Errorf("expected s2 outOfHoursHours=4 (its own entry), got %v", byStaff["s2"])
	}
}
