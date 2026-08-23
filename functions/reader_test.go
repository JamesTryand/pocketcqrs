package functions

import (
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

func newJSONFieldCollection(t *testing.T, app core.App, name string) *core.Collection {
	t.Helper()
	col := core.NewBaseCollection(name)
	col.Fields.Add(&core.JSONField{Name: "payload"})
	if err := app.Save(col); err != nil {
		t.Fatalf("save collection: %v", err)
	}
	return col
}

// TestFindRecordDecodesJSONField reproduces F-15: PublicExport's raw
// types.JSONRaw crossing into goja as an array of numeric byte codes instead
// of the value a json-typed field actually holds.
func TestFindRecordDecodesJSONField(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	col := newJSONFieldCollection(t, app, "widgets")
	rec := core.NewRecord(col)
	rec.Set("payload", map[string]any{"color": "red", "qty": float64(3)})
	if err := app.Save(rec); err != nil {
		t.Fatal(err)
	}

	reader := NewAppReader(app)
	got, err := reader.FindRecord("widgets", rec.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected a record")
	}

	payload, ok := got["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload not decoded to a map, got %T: %v", got["payload"], got["payload"])
	}
	if payload["color"] != "red" || payload["qty"] != float64(3) {
		t.Fatalf("unexpected decoded payload: %v", payload)
	}
}

// TestQueryDecodesJSONField mirrors TestFindRecordDecodesJSONField for the
// Query path, which shares the same PublicExport-based bug.
func TestQueryDecodesJSONField(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	col := newJSONFieldCollection(t, app, "widgets")
	rec := core.NewRecord(col)
	rec.Set("payload", []any{"a", "b"})
	if err := app.Save(rec); err != nil {
		t.Fatal(err)
	}

	reader := NewAppReader(app)
	rows, err := reader.Query("widgets", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}

	payload, ok := rows[0]["payload"].([]any)
	if !ok {
		t.Fatalf("payload not decoded to a slice, got %T: %v", rows[0]["payload"], rows[0]["payload"])
	}
	if len(payload) != 2 || payload[0] != "a" || payload[1] != "b" {
		t.Fatalf("unexpected decoded payload: %v", payload)
	}
}

// TestReadModifyWriteJSONFieldRoundTrip reproduces the live corruption: a
// read-modify-write cycle over a json column, following the platform's own
// documented pb.query-inside-project() idiom, must survive a second cycle
// without turning the stored value into a byte-code array.
func TestReadModifyWriteJSONFieldRoundTrip(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	col := newJSONFieldCollection(t, app, "widgets")
	rec := core.NewRecord(col)
	rec.Set("payload", map[string]any{"count": float64(0)})
	if err := app.Save(rec); err != nil {
		t.Fatal(err)
	}

	reader := NewAppReader(app)

	for i := 1; i <= 2; i++ {
		got, err := reader.FindRecord("widgets", rec.Id)
		if err != nil {
			t.Fatal(err)
		}
		payload, ok := got["payload"].(map[string]any)
		if !ok {
			t.Fatalf("cycle %d: payload not a map, got %T: %v", i, got["payload"], got["payload"])
		}
		count, ok := payload["count"].(float64)
		if !ok {
			t.Fatalf("cycle %d: count not a number, got %T: %v", i, payload["count"], payload["count"])
		}
		payload["count"] = count + 1

		live, err := app.FindRecordById("widgets", rec.Id)
		if err != nil {
			t.Fatal(err)
		}
		live.Set("payload", payload)
		if err := app.Save(live); err != nil {
			t.Fatal(err)
		}
	}

	final, err := reader.FindRecord("widgets", rec.Id)
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := final["payload"].(map[string]any)
	if !ok {
		t.Fatalf("final payload not a map, got %T: %v", final["payload"], final["payload"])
	}
	if payload["count"] != float64(2) {
		t.Fatalf("expected count=2 after two cycles, got %v", payload["count"])
	}
}
