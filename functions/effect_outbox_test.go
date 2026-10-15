package functions

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/outbox"
)

// With an outbox, an effect function runs off the engine's pass (R4): the
// engine's delivery only records the work, and the outbox worker runs the
// function's current code, dead-lettering a throw as before (one attempt).
func TestEffectFunctionsRunFromTheOutbox(t *testing.T) {
	ctx := context.Background()
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	box, err := outbox.Open(filepath.Join(t.TempDir(), "outbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()

	rt := NewGojaRuntime(nil)
	rt.SetStore(store)
	rt.SetOutbox(box)
	if err := rt.RegisterEventFunction([]string{"TaskCreated"}, "poison.js", `throw new Error("poisoned");`); err != nil {
		t.Fatal(err)
	}
	if err := rt.RegisterEventFunction([]string{"TaskCreated"}, "fine.js", `var x = event.data.title;`); err != nil {
		t.Fatal(err)
	}
	engine := consumers.NewEngine(store, nil)
	for _, c := range rt.Consumers() {
		engine.Register(c)
	}
	if _, err := store.Append(ctx, "task", "t1", 0, []events.NewEvent{{Type: "TaskCreated", Data: json.RawMessage(`{"title":"x"}`)}}); err != nil {
		t.Fatal(err)
	}
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if letters, _ := store.DeadLetters(ctx, false); len(letters) != 0 {
		t.Fatal("the function ran on the engine's pass")
	}
	if pending, _ := box.Pending(ctx); len(pending) != 2 {
		t.Fatalf("pending %d, want one per function", len(pending))
	}

	w := &outbox.Worker{Store: box, Handlers: map[string]outbox.Handler{EffectConsumer: rt.RunQueued}}
	if _, err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	letters, _ := store.DeadLetters(ctx, false)
	if len(letters) != 1 || letters[0].Consumer != "fn:poison.js" || letters[0].Event.AggregateID != "t1" {
		t.Fatalf("dead letters %+v", letters)
	}
	if pending, _ := box.Pending(ctx); len(pending) != 0 {
		t.Fatalf("still pending: %+v", pending)
	}

	// a function since removed: its queued work is dropped, said out loud
	if err := box.Enqueue(ctx, "gone", EffectConsumer, json.RawMessage(`{"function":"gone.js","event":{}}`), time.Time{}); err != nil {
		t.Fatal(err)
	}
	w.RunOnce(ctx)
	if pending, _ := box.Pending(ctx); len(pending) != 0 {
		t.Fatal("work for a removed function waits forever")
	}
}
