package functions

import (
	"reflect"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

func TestParseSchemaDirective(t *testing.T) {
	spec, err := parseSchemaDirective("orders_by_customer customerRef:text orderCount:number total:number", "customerRef")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Collection != "orders_by_customer" || spec.Key != "customerRef" || len(spec.Fields) != 3 {
		t.Fatalf("unexpected spec: %+v", spec)
	}
	if spec.uniqueIndexSQL() != "CREATE UNIQUE INDEX idx_orders_by_customer_customerRef ON orders_by_customer (customerRef)" {
		t.Fatalf("unexpected index SQL: %s", spec.uniqueIndexSQL())
	}
}

func TestParseSchemaDirectiveRelation(t *testing.T) {
	spec, err := parseSchemaDirective("order_summaries customer:text order:relation(orders) total:number", "customer")
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Fields) != 3 {
		t.Fatalf("unexpected fields: %+v", spec.Fields)
	}
	rel := spec.Fields[1]
	if rel.Type != "relation" || rel.RelationTarget != "orders" {
		t.Fatalf("unexpected relation field: %+v", rel)
	}
	if spec.Fields[2].Type != "number" || spec.Fields[2].RelationTarget != "" {
		t.Fatalf("unexpected plain field: %+v", spec.Fields[2])
	}
}

func TestParseSchemaDirectiveRejections(t *testing.T) {
	cases := []struct {
		name, rest, key string
	}{
		{"no fields", "onlycollection", "k"},
		{"missing key directive", "c k:text", ""},
		{"key not in schema", "c a:text", "k"},
		{"bad type", "c a:string", "a"},
		{"missing type", "c a", "a"},
		{"sql-ish collection", "c); DROP TABLE events;-- a:text", "a"},
		{"sql-ish field", "c `x`:text", "`x`"},
		{"empty relation target", "c k:text a:relation()", "k"},
		{"unclosed relation", "c k:text a:relation(orders", "k"},
		{"invalid relation target", "c k:text a:relation(1orders)", "k"},
		{"trailing junk after relation", "c k:text a:relation(orders)x", "k"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseSchemaDirective(tc.rest, tc.key); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestReconcileSchemasRelations(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	// a pre-existing collection created outside any spec (migration-style)
	if err := app.Save(core.NewBaseCollection("existing_orders")); err != nil {
		t.Fatal(err)
	}

	rt := NewGojaRuntime(nil)
	mk := func(collection, key string, fields ...FieldSpec) *ProjectionSpec {
		return &ProjectionSpec{
			Name:    collection,
			Schema:  &SchemaSpec{Collection: collection, Key: key, Fields: fields},
			runtime: rt,
		}
	}
	specs := []*ProjectionSpec{
		// declared BEFORE its relation targets — the two passes must wire it anyway
		mk("child_rows", "ref",
			FieldSpec{Name: "ref", Type: "text"},
			FieldSpec{Name: "parent", Type: "relation", RelationTarget: "parent_rows"},
			FieldSpec{Name: "order", Type: "relation", RelationTarget: "existing_orders"},
		),
		mk("parent_rows", "ref",
			FieldSpec{Name: "ref", Type: "text"},
		),
	}
	if err := ReconcileSchemas(app, specs); err != nil {
		t.Fatal(err)
	}

	child, err := app.FindCollectionByNameOrId("child_rows")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := app.FindCollectionByNameOrId("parent_rows")
	if err != nil {
		t.Fatal(err)
	}
	existing, err := app.FindCollectionByNameOrId("existing_orders")
	if err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]string{"parent": parent.Id, "order": existing.Id} {
		f, ok := child.Fields.GetByName(name).(*core.RelationField)
		if !ok {
			t.Fatalf("field %s is not a relation: %+v", name, child.Fields.GetByName(name))
		}
		if f.CollectionId != want || f.MaxSelect != 1 {
			t.Fatalf("field %s: got collectionId=%s maxSelect=%d, want %s/1", name, f.CollectionId, f.MaxSelect, want)
		}
	}

	foundIdx := false
	for _, idx := range child.Indexes {
		if idx == "CREATE UNIQUE INDEX idx_child_rows_ref ON child_rows (ref)" {
			foundIdx = true
		}
	}
	if !foundIdx {
		t.Fatalf("key index missing: %v", child.Indexes)
	}

	// idempotent: a second reconcile adds nothing and keeps the wiring
	if err := ReconcileSchemas(app, specs); err != nil {
		t.Fatal(err)
	}
	again, err := app.FindCollectionByNameOrId("child_rows")
	if err != nil {
		t.Fatal(err)
	}
	if rel, ok := again.Fields.GetByName("parent").(*core.RelationField); !ok || rel.CollectionId != parent.Id {
		t.Fatalf("relation lost on second reconcile: %+v", again.Fields.GetByName("parent"))
	}
}

func TestReconcileSchemasRelationTargetMissing(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	rt := NewGojaRuntime(nil)
	specs := []*ProjectionSpec{{
		Name: "broken",
		Schema: &SchemaSpec{Collection: "broken", Key: "ref", Fields: []FieldSpec{
			{Name: "ref", Type: "text"},
			{Name: "ghost", Type: "relation", RelationTarget: "no_such_collection"},
		}},
		runtime: rt,
	}}
	if err := ReconcileSchemas(app, specs); err == nil {
		t.Fatal("expected missing relation target error")
	}
}

func TestNormalizeOps(t *testing.T) {
	// nil -> none
	if ops, err := normalizeOps(nil); err != nil || len(ops) != 0 {
		t.Fatalf("nil: ops=%v err=%v", ops, err)
	}

	// single upsert
	ops, err := normalizeOps(map[string]any{
		"upsert": map[string]any{"key": "c1", "fields": map[string]any{"n": 1}},
	})
	if err != nil || len(ops) != 1 || ops[0].key != "c1" || ops[0].delete {
		t.Fatalf("upsert: ops=%v err=%v", ops, err)
	}

	// single delete
	ops, err = normalizeOps(map[string]any{"delete": "c1"})
	if err != nil || len(ops) != 1 || !ops[0].delete || ops[0].key != "c1" {
		t.Fatalf("delete: ops=%v err=%v", ops, err)
	}

	// array
	ops, err = normalizeOps([]any{
		map[string]any{"upsert": map[string]any{"key": "a", "fields": map[string]any{}}},
		map[string]any{"delete": "b"},
	})
	if err != nil || len(ops) != 2 || ops[0].delete || !ops[1].delete {
		t.Fatalf("array: ops=%v err=%v", ops, err)
	}

	// malformed
	if _, err = normalizeOps(map[string]any{"upsert": "nope"}); err == nil {
		t.Fatal("expected malformed upsert error")
	}
}

func TestSeedRandomDeterministic(t *testing.T) {
	seq := func(seed int64) []float64 {
		rt := NewGojaRuntime(nil)
		vm, timer := rt.newVM("t")
		defer timer.Stop()
		seedRandom(vm, seed)
		out := make([]float64, 3)
		for i := range out {
			v, err := vm.RunString("Math.random()")
			if err != nil {
				t.Fatal(err)
			}
			out[i] = v.ToFloat()
		}
		return out
	}

	a, b := seq(42), seq(42)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("same seed diverged: %v vs %v", a, b)
	}
	if c := seq(43); reflect.DeepEqual(a, c) {
		t.Fatalf("different seeds converged: %v vs %v", a, c)
	}
	for _, v := range a {
		if v < 0 || v >= 1 {
			t.Fatalf("out of range: %v", v)
		}
	}
}
