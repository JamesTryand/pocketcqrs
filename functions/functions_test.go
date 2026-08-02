package functions

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestCronFunctionFire(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	rt := NewGojaRuntime(func(msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprint(args...))
	})
	rt.SetReader(fakeReader{rows: []map[string]any{{"id": "r1"}, {"id": "r2"}}})

	if err := rt.RegisterCronFunction("* * * * *", "hb.js",
		`console.log("[heartbeat] " + job.name + " tasks=" + pb.query("tasks", "", 10).length);`); err != nil {
		t.Fatal(err)
	}

	jobs := rt.CronJobs()
	if len(jobs) != 1 || jobs[0].Schedule != "* * * * *" {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}

	jobs[0].Fire()

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, l := range logs {
		if strings.Contains(l, "[heartbeat] hb.js tasks=2") {
			found = true
		}
	}
	if !found {
		t.Fatalf("cron function did not fire correctly; logs: %v", logs)
	}
}

func TestDeadLetterOnFailureAndRetry(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	rt := NewGojaRuntime(nil)
	rt.SetStore(store)

	// a function that fails on TaskCreated
	if err := rt.RegisterEventFunction([]string{"TaskCreated"}, "poison.js",
		`throw new Error("poisoned");`); err != nil {
		t.Fatal(err)
	}

	engine := consumers.NewEngine(store, nil)
	for _, c := range rt.Consumers() {
		engine.Register(c)
	}

	if _, err := store.Append(ctx, "task", "t1", 0, []events.NewEvent{
		{Type: "TaskCreated", Data: json.RawMessage(`{"title":"x"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// failure was dead-lettered with the full envelope
	letters, err := store.DeadLetters(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(letters) != 1 || letters[0].Consumer != "fn:poison.js" {
		t.Fatalf("expected 1 dead letter, got %+v", letters)
	}
	dl := letters[0]
	if dl.Event.AggregateID != "t1" || dl.Error == "" {
		t.Fatalf("bad dead letter: %+v", dl)
	}

	// retry with the current (still broken) code fails and is recorded
	if err := rt.RetryEventFunction("poison.js", dl.Event); err == nil {
		t.Fatal("expected retry to fail")
	} else {
		_ = store.FailDeadLetterRetry(ctx, dl.ID, err)
	}
	letters, _ = store.DeadLetters(ctx, false)
	if letters[0].Attempts != 2 {
		t.Fatalf("expected attempts=2, got %d", letters[0].Attempts)
	}

	// "fix" the function (re-register same name: latest code wins)
	if err := rt.RegisterEventFunction([]string{"TaskCreated"}, "poison.js",
		`console.log("recovered: " + event.data.title);`); err != nil {
		t.Fatal(err)
	}
	if err := rt.RetryEventFunction("poison.js", dl.Event); err != nil {
		t.Fatalf("retry with fixed code failed: %v", err)
	}
	if err := store.ResolveDeadLetter(ctx, dl.ID); err != nil {
		t.Fatal(err)
	}

	letters, _ = store.DeadLetters(ctx, false)
	if len(letters) != 0 {
		t.Fatalf("expected no pending dead letters, got %+v", letters)
	}
}
