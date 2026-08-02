package functions

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"pocketcqrs/decider"
	"pocketcqrs/events"
)

func writeTempFn(t *testing.T, name, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	writeFn(t, filepath.Dir(path), name, src)
	return path
}

func TestLoadDeciderFileAndDryRun(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	// history: two note streams
	if _, err := store.Append(ctx, "note", "n1", 0, []events.NewEvent{
		{Type: "NoteCreated", Data: json.RawMessage(`{"text":"a"}`)},
		{Type: "NoteArchived", Data: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, "note", "n2", 0, []events.NewEvent{
		{Type: "NoteCreated", Data: json.RawMessage(`{"text":"b"}`)},
	}); err != nil {
		t.Fatal(err)
	}

	rt := NewGojaRuntime(nil)
	spec, err := LoadDeciderFile(rt, writeTempFn(t, "note.js", `//@trigger decider note
//@handles NoteCreated NoteArchived
`+noteDeciderJS))
	if err != nil {
		t.Fatal(err)
	}

	// all-streams fold
	res, err := DryRunDecider(store, spec, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Streams != 2 || res.Events != 3 {
		t.Fatalf("unexpected result: %+v", res)
	}

	// single-stream fold exposes the final state
	res, err = DryRunDecider(store, spec, "n1")
	if err != nil {
		t.Fatal(err)
	}
	state := res.State.(map[string]any)
	if state["text"] != "a" || state["archived"] != true {
		t.Fatalf("unexpected final state: %v", state)
	}
}

func TestDryRunDecide(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if _, err := store.Append(ctx, "note", "n1", 0, []events.NewEvent{
		{Type: "NoteCreated", Data: json.RawMessage(`{"text":"a"}`)},
	}); err != nil {
		t.Fatal(err)
	}

	rt := NewGojaRuntime(nil)
	spec, err := LoadDeciderFile(rt, writeTempFn(t, "note.js", `//@trigger decider note
//@handles NoteCreated NoteArchived
`+noteDeciderJS))
	if err != nil {
		t.Fatal(err)
	}

	// when: a valid command on the folded stream -> outcome events, no append
	produced, err := DryRunDecide(store, spec, "n1",
		decider.Command{Name: "ArchiveNote", Payload: json.RawMessage(`{}`)},
		map[string]any{"now": "dry", "actor": "test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(produced) != 1 || produced[0].Type != "NoteArchived" {
		t.Fatalf("unexpected produced events: %+v", produced)
	}

	// when: a domain-invalid command -> the decider's rejection
	if _, err := DryRunDecide(store, spec, "n1",
		decider.Command{Name: "CreateNote", Payload: json.RawMessage(`{"text":"dup"}`)}, nil); err == nil {
		t.Fatal("expected domain rejection")
	}

	// and nothing was appended
	stream, _ := store.LoadStream(ctx, "note", "n1")
	if len(stream) != 1 {
		t.Fatalf("dry run appended to the stream: %+v", stream)
	}
}

func TestDryRunProjectionMultiCollection(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if _, err := store.Append(ctx, "sale", "s1", 0, []events.NewEvent{
		{Type: "SalePlaced", Data: json.RawMessage(`{"cust":"c1","sku":"p1","qty":2}`)},
	}); err != nil {
		t.Fatal(err)
	}

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
	spec, err := LoadProjectionFile(rt, nil, writeTempFn(t, "sales.js", src))
	if err != nil {
		t.Fatal(err)
	}

	res, err := DryRunProjection(store, spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Upserts != 2 || len(res.Rows) != 2 {
		t.Fatalf("unexpected simulation: %+v", res)
	}
	if res.Rows["customers"]["c1"]["total"] != int64(2) && res.Rows["customers"]["c1"]["total"] != float64(2) {
		t.Fatalf("unexpected customers rows: %+v", res.Rows["customers"])
	}
	if res.Rows["products"]["p1"]["sold"] == nil {
		t.Fatalf("unexpected products rows: %+v", res.Rows["products"])
	}

	// an op without collection is ambiguous for a multi-schema projection
	ambSrc := `//@trigger projection amb on SalePlaced
//@schema customers cust:text
//@key cust
//@schema products sku:text
//@key sku
function project(event) { return { upsert: { key: "x", fields: {} } }; }
`
	ambSpec, err := LoadProjectionFile(rt, nil, writeTempFn(t, "amb.js", ambSrc))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DryRunProjection(store, ambSpec); err == nil {
		t.Fatal("expected ambiguity error")
	}

	// an op naming an undeclared collection is rejected
	undSrc := `//@trigger projection und on SalePlaced
//@schema customers cust:text
//@key cust
function project(event) { return { collection: "nope", upsert: { key: "x", fields: {} } }; }
`
	undSpec, err := LoadProjectionFile(rt, nil, writeTempFn(t, "und.js", undSrc))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DryRunProjection(store, undSpec); err == nil {
		t.Fatal("expected undeclared collection error")
	}
}

func TestDryRunProjectionAndDiff(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	for i, id := range []string{"o1", "o2"} {
		if _, err := store.Append(ctx, "order", id, 0, []events.NewEvent{
			{Type: "OrderPlaced", Data: json.RawMessage(fmt.Sprintf(`{"customerRef":"c%d"}`, i%2))},
		}); err != nil {
			t.Fatal(err)
		}
	}

	rt := NewGojaRuntime(nil)
	src := `//@trigger projection counts on OrderPlaced
//@schema counts customerRef:text n:number
//@key customerRef
function project(event) {
	var rows = pb.query("counts", "", 500) || [];
	var n = 0;
	for (var i = 0; i < rows.length; i++) { if (rows[i].customerRef === event.data.customerRef) n = rows[i].n || 0; }
	return { upsert: { key: event.data.customerRef, fields: { n: n + 1 } } };
}
`
	// note: pb binding is inert (no reader) so the query returns nil;
	// the simulation still exercises ops collection deterministically
	spec, err := LoadProjectionFile(rt, nil, writeTempFn(t, "counts.js", src))
	if err != nil {
		t.Fatal(err)
	}

	res, err := DryRunProjection(store, spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 2 || res.Upserts != 2 || len(res.Rows["counts"]) != 2 {
		t.Fatalf("unexpected simulation: %+v", res)
	}

	// diff: identical
	diffs := DiffRows(res.Rows["counts"], []map[string]any{
		{"customerRef": "c0", "n": res.Rows["counts"]["c0"]["n"]},
		{"customerRef": "c1", "n": res.Rows["counts"]["c1"]["n"]},
	}, "customerRef")
	if len(diffs) != 0 {
		t.Fatalf("expected no diffs, got %v", diffs)
	}

	// diff: missing, changed, extra rows
	diffs = DiffRows(res.Rows["counts"], []map[string]any{
		{"customerRef": "c0", "n": 99},
		{"customerRef": "cX", "n": 1},
	}, "customerRef")
	joined := strings.Join(diffs, "\n")
	if len(diffs) != 3 || !strings.Contains(joined, "missing") ||
		!strings.Contains(joined, "field n") || !strings.Contains(joined, "not produced") {
		t.Fatalf("unexpected diffs: %v", diffs)
	}
}
