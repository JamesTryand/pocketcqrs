package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/functions"
)

// TestRetryDeadLettersAdjudication covers the shared decision behind both
// `pocketcqrs deadletter retry` and POST /api/cqrs/deadletters/{id}/retry:
// re-deliver through the CURRENT code, resolve what now succeeds, record an
// attempt on what does not.
func TestRetryDeadLettersAdjudication(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	ev := events.Event{Position: 7, ID: "e1", Aggregate: "task", AggregateID: "t1",
		Sequence: 1, Type: "TaskCreated", Data: json.RawMessage(`{"title":"x"}`)}

	// three captured failures: one whose function still throws, one whose
	// function has since been fixed, one whose function is gone entirely
	for _, consumer := range []string{"fn:poison.js", "fn:fixed.js", "fn:deleted.js"} {
		if err := store.AddDeadLetter(ctx, consumer, ev, errors.New("boom")); err != nil {
			t.Fatal(err)
		}
	}

	rt := functions.NewGojaRuntime(func(msg string, args ...any) {})
	if err := rt.RegisterEventFunction([]string{"TaskCreated"}, "poison.js",
		`throw new Error("still poison")`); err != nil {
		t.Fatal(err)
	}
	if err := rt.RegisterEventFunction([]string{"TaskCreated"}, "fixed.js",
		`console.log("delivered " + event.type)`); err != nil {
		t.Fatal(err)
	}
	// deleted.js is deliberately NOT registered

	c := &components{store: store, fnRuntime: rt}

	pending, err := store.DeadLetters(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("expected 3 pending dead letters, got %d", len(pending))
	}

	results, err := c.retryDeadLetters(ctx, pending)
	if err != nil {
		t.Fatalf("retry batch failed as a whole: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	byConsumer := map[string]deadLetterResult{}
	for _, res := range results {
		byConsumer[res.Consumer] = res
	}

	if got := byConsumer["fn:fixed.js"]; !got.Resolved || got.Error != "" {
		t.Errorf("fixed function should have resolved: %+v", got)
	}
	if got := byConsumer["fn:poison.js"]; got.Resolved || got.Error == "" || got.Attempts != 2 {
		t.Errorf("still-poison function should stay pending with a second attempt: %+v", got)
	}
	// a function that no longer exists is adjudicated exactly like any other
	// failed delivery — not an error for the caller, so the CLI and the HTTP
	// API keep reporting it the same way and the operator can dismiss it
	if got := byConsumer["fn:deleted.js"]; got.Resolved || !strings.Contains(got.Error, "no function named") {
		t.Errorf("deleted function should report a delivery failure: %+v", got)
	}

	// the store agrees: one resolved, two still pending with 2 attempts each
	stillPending, err := store.DeadLetters(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(stillPending) != 2 {
		t.Fatalf("expected 2 pending after the retry, got %d", len(stillPending))
	}
	for _, dl := range stillPending {
		if dl.Consumer == "fn:fixed.js" {
			t.Error("the fixed function's dead letter should have been resolved")
		}
		if dl.Attempts != 2 {
			t.Errorf("%s: expected 2 attempts recorded, got %d", dl.Consumer, dl.Attempts)
		}
	}
}
