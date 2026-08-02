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
