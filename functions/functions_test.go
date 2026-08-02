package functions

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"pocketcqrs/consumers"
	"pocketcqrs/events"
)

func TestEventFunctionDelivery(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	var mu sync.Mutex
	var logs []string
	rt := NewGojaRuntime(func(msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, msg)
	})

	err = rt.RegisterEventFunction([]string{"TaskCreated"}, "capture.js",
		`console.log("got " + event.type + " " + event.data.title)`)
	if err != nil {
		t.Fatal(err)
	}

	engine := consumers.NewEngine(store, nil)
	for _, c := range rt.Consumers() {
		engine.Register(c)
	}

	// a matching and a non-matching event
	if _, err := store.Append(ctx, "task", "t1", 0, []events.NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{"title":"hello"}`)},
		{Type: "TaskCompleted", Data: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}

	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, l := range logs {
		if strings.Contains(l, "fn capture.js") {
			found = true
		}
	}
	if !found {
		t.Fatalf("function was not invoked; logs: %v", logs)
	}
}

func TestRegisterEventFunctionCompileError(t *testing.T) {
	rt := NewGojaRuntime(nil)
	if err := rt.RegisterEventFunction([]string{"X"}, "bad.js", `this is not js`); err == nil {
		t.Fatal("expected compile error")
	}
}
