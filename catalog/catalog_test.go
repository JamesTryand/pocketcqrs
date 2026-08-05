package catalog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/functions"
	"github.com/jamestryand/pocketcqrs/projections"
)

// stubProjection stands in for a built-in Go projection (with declared
// triggers, like the real ones).
type stubProjection struct{}

func (stubProjection) Name() string                              { return "tasks" }
func (stubProjection) Collections() []string                     { return []string{"tasks"} }
func (stubProjection) EventTypes() []string                      { return []string{"TaskCreated"} }
func (stubProjection) Apply(context.Context, events.Event) error { return nil }

func TestBuildAndRender(t *testing.T) {
	ctx := context.Background()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// history: a task, a note (JS aggregate), and a reactor-caused task
	if _, err := store.Append(ctx, "task", "t1", 0, []events.NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{"title":"a"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, "note", "n1", 0, []events.NewEvent{
		{Type: "NoteCreated", Data: json.RawMessage(`{"text":"x"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	caused, err := store.Append(ctx, "order", "o1", 0, []events.NewEvent{
		{Type: "OrderConfirmed", Data: json.RawMessage(`{}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]string{
		"actor": "reactor:fulfillment", "causationId": caused[0].ID, "correlationId": caused[0].ID,
	})
	if _, err := store.Append(ctx, "task", "fulfill-o1", 0, []events.NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{"title":"fulfil order o1"}`), Metadata: meta},
	}); err != nil {
		t.Fatal(err)
	}

	// one Go decider in the registry
	registry := decider.NewRegistry(store)
	decider.Register(registry, "task", &decider.Decider[struct{}]{
		InitialState: func() struct{} { return struct{}{} },
		Decide:       func(decider.Command, struct{}) ([]events.NewEvent, error) { return nil, nil },
		Evolve:       func(s struct{}, _ events.Event) (struct{}, error) { return s, nil },
	})

	// the functions dir: a JS decider, an HTTP fn, a JS projection
	rt := functions.NewGojaRuntime(nil)
	dir := t.TempDir()
	files := map[string]string{
		"note.js": `//@trigger decider note
//@handles NoteCreated
function initialState() { return { exists: false }; }
function decide(command, state) { return [{ type: "NoteCreated", data: {} }]; }
function evolve(state, event) { return state; }
`,
		"hello.js": "//@trigger http\nfunction handle(request) { return {}; }\n",
		"notes.js": `//@trigger projection notes on NoteCreated
//@schema notes noteId:text text:text
//@key noteId
function project(event) { return { upsert: { key: event.aggregateId, fields: {} } }; }
`,
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := functions.LoadDir(rt, app, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Deciders) != 1 || len(loaded.Projections) != 1 {
		t.Fatalf("unexpected load: %d deciders, %d projections", len(loaded.Deciders), len(loaded.Projections))
	}
	noteSpec := loaded.Deciders[0]
	registry.RegisterUntyped("note", noteSpec.UntypedDecider())

	// an effect function (registered directly, as event fns are)
	if err := rt.RegisterEventFunction([]string{"TaskCreated"}, "audit.js", `console.log(event.type)`); err != nil {
		t.Fatal(err)
	}

	// reconcile + register the JS projection's consumer
	if err := functions.ReconcileSchemas(app, loaded.Projections); err != nil {
		t.Fatal(err)
	}
	var jsProjs []*functions.JSProjection
	for _, spec := range loaded.Projections {
		jsProjs = append(jsProjs, spec.Consumer())
	}
	engine := consumers.NewEngine(store, nil)
	engine.Register(stubProjection{})
	for _, p := range jsProjs {
		engine.Register(p)
	}
	for _, consumer := range rt.Consumers() {
		engine.Register(consumer)
	}

	cat, err := Build(ctx, Inputs{
		App:           app,
		Store:         store,
		Registry:      registry,
		Engine:        engine,
		Runtime:       rt,
		HTTP:          loaded.HTTP,
		GoProjections: []projections.Projection{stubProjection{}},
		JSProjs:       jsProjs,
		JSDeciders:    map[string]*functions.DeciderSpec{"note": noteSpec},
	})
	if err != nil {
		t.Fatal(err)
	}

	// totals: 4 appended events, and the head of the log to measure
	// consumer lag against
	if cat.Totals.Events != 4 || cat.Totals.MaxPosition != 4 || cat.Totals.Streams != 4 {
		t.Fatalf("unexpected totals: %+v", cat.Totals)
	}

	// aggregates: note (js), task (go)
	if len(cat.Aggregates) != 2 {
		t.Fatalf("expected 2 aggregates, got %+v", cat.Aggregates)
	}
	var noteAgg, taskAgg *Aggregate
	for i := range cat.Aggregates {
		switch cat.Aggregates[i].Name {
		case "note":
			noteAgg = &cat.Aggregates[i]
		case "task":
			taskAgg = &cat.Aggregates[i]
		}
	}
	if noteAgg == nil || noteAgg.Origin != "js" || len(noteAgg.Handles) != 1 || noteAgg.Handles[0] != "NoteCreated" {
		t.Fatalf("unexpected note aggregate: %+v", noteAgg)
	}
	if taskAgg == nil || taskAgg.Origin != "go" || taskAgg.Streams != 2 {
		t.Fatalf("unexpected task aggregate: %+v", taskAgg)
	}

	// consumers: tasks (go-projection w/ declared triggers) + notes + fn:audit.js (effect)
	if len(cat.Consumers) != 3 {
		t.Fatalf("expected 3 consumers, got %+v", cat.Consumers)
	}
	var notesCons, tasksCons *Consumer
	for i := range cat.Consumers {
		switch cat.Consumers[i].Name {
		case "notes":
			notesCons = &cat.Consumers[i]
		case "tasks":
			tasksCons = &cat.Consumers[i]
		}
	}
	if notesCons == nil || notesCons.Kind != "js-projection" ||
		len(notesCons.Collections) != 1 || notesCons.Collections[0] != "notes" ||
		len(notesCons.EventTypes) != 1 || notesCons.EventTypes[0] != "NoteCreated" {
		t.Fatalf("unexpected consumers: %+v", cat.Consumers)
	}
	if tasksCons == nil || tasksCons.Kind != "go-projection" ||
		len(tasksCons.EventTypes) != 1 || tasksCons.EventTypes[0] != "TaskCreated" ||
		len(tasksCons.Collections) != 1 || tasksCons.Collections[0] != "tasks" {
		t.Fatalf("unexpected go-projection consumer: %+v", tasksCons)
	}

	// collections: notes present, guarded, owned, keyed; system fields filtered
	var notesCol *Collection
	for i := range cat.Collections {
		if cat.Collections[i].Name == "notes" {
			notesCol = &cat.Collections[i]
		}
	}
	if notesCol == nil || !notesCol.Guarded || notesCol.Owner != "notes" || notesCol.Key != "noteId" {
		t.Fatalf("unexpected collections: %+v", cat.Collections)
	}
	if len(notesCol.Fields) != 2 {
		t.Fatalf("unexpected notes fields: %+v", notesCol.Fields)
	}

	// functions: hello (http)
	if len(cat.Functions.HTTP) != 1 || cat.Functions.HTTP[0] != "hello" {
		t.Fatalf("unexpected http functions: %+v", cat.Functions.HTTP)
	}

	// flows: the empirical fulfillment edge
	if len(cat.Flows) != 1 || cat.Flows[0].Cause != "order/OrderConfirmed" ||
		cat.Flows[0].Target != "task/TaskCreated" || cat.Flows[0].Reactor != "reactor:fulfillment" {
		t.Fatalf("unexpected flows: %+v", cat.Flows)
	}

	// markdown: all sections present
	md := cat.Markdown()
	for _, want := range []string{"## Aggregates", "## Consumers", "## Collections", "## Functions", "## Flows", "```mermaid", "reactor:fulfillment"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q:\n%s", want, md)
		}
	}

	// mermaid: flowchart with the empirical reactor edge (':' sanitizes to '_')
	mermaid := cat.Mermaid()
	if !strings.HasPrefix(mermaid, "flowchart LR\n") {
		t.Fatalf("unexpected mermaid start: %s", mermaid)
	}
	for _, want := range []string{"cons_reactor_fulfillment", `agg_task`, `col_notes`} {
		if !strings.Contains(mermaid, want) {
			t.Fatalf("mermaid missing %q:\n%s", want, mermaid)
		}
	}

	// JSON marshals
	if _, err := cat.JSON(); err != nil {
		t.Fatal(err)
	}

	// skeletons: written for both aggregates, skipped on second run,
	// events + implementation prefilled
	skelDir := filepath.Join(t.TempDir(), "domains")
	written, skipped, err := cat.WriteSkeletons(skelDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 2 || len(skipped) != 0 {
		t.Fatalf("unexpected skeleton write: written=%v skipped=%v", written, skipped)
	}
	written2, skipped2, err := cat.WriteSkeletons(skelDir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(written2) != 0 || len(skipped2) != 2 {
		t.Fatalf("expected skips, got written=%v skipped=%v", written2, skipped2)
	}
	content, err := os.ReadFile(filepath.Join(skelDir, "note.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "`NoteCreated`") || !strings.Contains(string(content), "pb_functions/note.js") {
		t.Fatalf("unexpected note skeleton:\n%s", content)
	}

	// the Go aggregate's skeleton picks up the Go projection's collection
	// via its declared triggers
	taskContent, err := os.ReadFile(filepath.Join(skelDir, "task.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(taskContent), "`tasks`") || !strings.Contains(string(taskContent), "projection `tasks`") {
		t.Fatalf("task skeleton missing its read model:\n%s", taskContent)
	}
}
