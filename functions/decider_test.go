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

const noteDeciderJS = `
function initialState() { return { exists: false, text: "", archived: false }; }
function decide(command, state) {
	switch (command.name) {
		case "CreateNote":
			if (state.exists) throw new Error("note already exists");
			if (!command.payload || !command.payload.text) throw new Error("text is required");
			return [{ type: "NoteCreated", data: { text: command.payload.text, seenAt: command.now, by: command.actor } }];
		case "ArchiveNote":
			if (!state.exists) throw new Error("note does not exist");
			return [{ type: "NoteArchived", data: {} }];
		default:
			throw new Error("unknown command: " + command.name);
	}
}
function evolve(state, event) {
	if (event.type === "NoteCreated") { state.exists = true; state.text = event.data.text; }
	else if (event.type === "NoteArchived") { state.archived = true; }
	return state;
}
`

func mkDeciderSpec(t *testing.T, rt *GojaRuntime, aggregate, src string, handles []string, transforms []TransformSpec) *DeciderSpec {
	t.Helper()
	return &DeciderSpec{Aggregate: aggregate, Handles: handles, Transforms: transforms, Prog: compile(t, src), runtime: rt}
}

func deciderSetup(t *testing.T) (*events.Store, *decider.Registry) {
	t.Helper()
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, decider.NewRegistry(store)
}

func TestJSDeciderLifecycle(t *testing.T) {
	_, registry := deciderSetup(t)
	rt := NewGojaRuntime(nil)
	spec := mkDeciderSpec(t, rt, "note", noteDeciderJS, []string{"NoteCreated", "NoteArchived"}, nil)
	registry.RegisterUntyped(spec.Aggregate, spec.UntypedDecider())
	ctx := context.Background()

	appended, err := registry.HandleWithMeta(ctx, "note", "n1",
		decider.Command{Name: "CreateNote", Payload: json.RawMessage(`{"text":"hello"}`)},
		map[string]any{"actor": "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(appended) != 1 || appended[0].Type != "NoteCreated" || appended[0].Version != 1 {
		t.Fatalf("unexpected events: %+v", appended)
	}

	// command context reached the decider: seenAt/by from command.now/actor
	var data map[string]any
	if err := json.Unmarshal(appended[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["by"] != "u1" || data["seenAt"] == nil {
		t.Fatalf("command context missing from event data: %v", data)
	}

	// metadata: now stamped by the registry, actor from the caller
	var meta map[string]any
	if err := json.Unmarshal(appended[0].Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["now"] == nil || meta["actor"] != "u1" {
		t.Fatalf("metadata not stamped: %v", meta)
	}

	// domain rejection
	if _, err := registry.HandleWithMeta(ctx, "note", "n1",
		decider.Command{Name: "CreateNote", Payload: json.RawMessage(`{"text":"dup"}`)}, nil); err == nil {
		t.Fatal("expected duplicate rejection")
	}

	// second command exercises the evolve fold (JS state rebuild)
	appended, err = registry.HandleWithMeta(ctx, "note", "n1",
		decider.Command{Name: "ArchiveNote", Payload: json.RawMessage(`{}`)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(appended) != 1 || appended[0].Sequence != 2 {
		t.Fatalf("unexpected events after fold: %+v", appended)
	}
}

func TestTier3VMNeutering(t *testing.T) {
	_, registry := deciderSetup(t)
	rt := NewGojaRuntime(nil)
	rt.SetReader(fakeReader{record: map[string]any{"id": "r1"}}) // must NOT be visible

	cases := []struct {
		name, decideBody, wantErr string
	}{
		{"Math.random throws", `Math.random();`, "not available"},
		{"Date absent", `new Date();`, "TypeError"},
		{"pb absent", `pb.findRecord("tasks", "r1");`, "findRecord"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := `function initialState() { return {}; }
			function decide(command, state) { ` + tc.decideBody + ` return []; }
			function evolve(state, event) { return state; }`
			spec := mkDeciderSpec(t, rt, "probe"+tc.name, src, []string{}, nil)
			registry.RegisterUntyped(spec.Aggregate, spec.UntypedDecider())
			_, err := registry.Handle(context.Background(), spec.Aggregate, "x",
				decider.Command{Name: "Go", Payload: json.RawMessage(`{}`)})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected %q error, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestTransformUpcast(t *testing.T) {
	store, registry := deciderSetup(t)
	rt := NewGojaRuntime(nil)

	src := `
	function initialState() { return { text: "", priority: 0 }; }
	function decide(command, state) { return []; }
	function evolve(state, event) {
		if (event.type === "NoteCreated") { state.text = event.data.text; state.priority = event.data.priority; }
		return state;
	}
	function transform_NoteCreated_1_to_2(data) { data.priority = data.priority || 0; return data; }
	`
	spec := mkDeciderSpec(t, rt, "note", src, []string{"NoteCreated"},
		[]TransformSpec{{Type: "NoteCreated", From: 1, To: 2}})
	d := spec.UntypedDecider()
	registry.RegisterUntyped(spec.Aggregate, d)

	// a historical v1 event (without priority) lives in the store
	if _, err := store.Append(context.Background(), "note", "n1", 0,
		[]events.NewEvent{{Type: "NoteCreated", Data: json.RawMessage(`{"text":"old"}`), Version: 1}}); err != nil {
		t.Fatal(err)
	}

	// evolve sees the upcast (v2) shape
	state := d.Initial()
	stream, _ := store.LoadStream(context.Background(), "note", "n1")
	state, err := d.Evolve(state, stream[0])
	if err != nil {
		t.Fatal(err)
	}
	m := state.(map[string]any)
	if m["text"] != "old" || fmt.Sprint(m["priority"]) != "0" {
		t.Fatalf("unexpected upcast state: %v", m)
	}
}

func TestValidateDeciderSpec(t *testing.T) {
	store, _ := deciderSetup(t)
	ctx := context.Background()
	rt := NewGojaRuntime(nil)

	// valid spec on empty store passes
	good := mkDeciderSpec(t, rt, "note", noteDeciderJS, []string{"NoteCreated", "NoteArchived"}, nil)
	if err := ValidateDeciderSpec(store, good); err != nil {
		t.Fatalf("expected pass, got %v", err)
	}

	// seed history
	if _, err := store.Append(ctx, "note", "n1", 0, []events.NewEvent{
		{Type: "NoteCreated", Data: json.RawMessage(`{"text":"x"}`)},
		{Type: "NoteArchived", Data: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}

	// handles coverage: NoteArchived undeclared
	narrow := mkDeciderSpec(t, rt, "note", noteDeciderJS, []string{"NoteCreated"}, nil)
	if err := ValidateDeciderSpec(store, narrow); err == nil || !strings.Contains(err.Error(), "handles") {
		t.Fatalf("expected handles coverage error, got %v", err)
	}

	// crashing evolve is refused
	crashy := mkDeciderSpec(t, rt, "note", `
		function initialState() { return {}; }
		function decide(command, state) { return []; }
		function evolve(state, event) { if (event.type === "NoteArchived") throw new Error("boom"); return state; }
	`, []string{"NoteCreated", "NoteArchived"}, nil)
	if err := ValidateDeciderSpec(store, crashy); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected fold failure, got %v", err)
	}

	// declared transform missing its function
	missingFn := mkDeciderSpec(t, rt, "note", noteDeciderJS, []string{"NoteCreated", "NoteArchived"},
		[]TransformSpec{{Type: "NoteCreated", From: 1, To: 2}})
	if err := ValidateDeciderSpec(store, missingFn); err == nil || !strings.Contains(err.Error(), "transform_NoteCreated_1_to_2") {
		t.Fatalf("expected missing transform fn error, got %v", err)
	}
}
