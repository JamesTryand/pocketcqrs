package functions

import (
	"reflect"
	"testing"
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseSchemaDirective(tc.rest, tc.key); err == nil {
				t.Fatal("expected rejection")
			}
		})
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
