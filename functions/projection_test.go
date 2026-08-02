package functions

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pocketbase/pocketbase/tests"

	"pocketcqrs/events"
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
	if err := ReconcileSchemas(app, []*ProjectionSpec{spec}); err != nil {
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
	if err := ReconcileSchemas(app, []*ProjectionSpec{spec}); err != nil {
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
