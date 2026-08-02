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
	if res.Events != 2 || res.Upserts != 2 || len(res.Rows) != 2 {
		t.Fatalf("unexpected simulation: %+v", res)
	}

	// diff: identical
	diffs := DiffRows(res.Rows, []map[string]any{
		{"customerRef": "c0", "n": res.Rows["c0"]["n"]},
		{"customerRef": "c1", "n": res.Rows["c1"]["n"]},
	}, "customerRef")
	if len(diffs) != 0 {
		t.Fatalf("expected no diffs, got %v", diffs)
	}

	// diff: missing, changed, extra rows
	diffs = DiffRows(res.Rows, []map[string]any{
		{"customerRef": "c0", "n": 99},
		{"customerRef": "cX", "n": 1},
	}, "customerRef")
	joined := strings.Join(diffs, "\n")
	if len(diffs) != 3 || !strings.Contains(joined, "missing") ||
		!strings.Contains(joined, "field n") || !strings.Contains(joined, "not produced") {
		t.Fatalf("unexpected diffs: %v", diffs)
	}
}
