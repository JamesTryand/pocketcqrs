package decider

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"pocketcqrs/events"
)

type counterState struct {
	Count int
}

func counter() *Decider[counterState] {
	return &Decider[counterState]{
		InitialState: func() counterState { return counterState{} },
		Decide: func(cmd Command, state counterState) ([]events.NewEvent, error) {
			if cmd.Name != "Increment" {
				return nil, nil
			}
			return []events.NewEvent{{
				Type:     "Incremented",
				Data:     json.RawMessage(`{}`),
				Metadata: json.RawMessage(`{"source":"decider"}`),
			}}, nil
		},
		Evolve: func(state counterState, ev events.Event) (counterState, error) {
			if ev.Type == "Incremented" {
				state.Count++
			}
			return state, nil
		},
	}
}

func setup(t *testing.T) *Registry {
	t.Helper()
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	r := NewRegistry(store)
	Register(r, "counter", counter())
	return r
}

func TestHandleWithMetaMergesMetadata(t *testing.T) {
	r := setup(t)
	ctx := context.Background()

	appended, err := r.HandleWithMeta(ctx, "counter", "c1",
		Command{Name: "Increment", Payload: json.RawMessage(`{}`)},
		map[string]any{"actor": "user123", "actorCollection": "users"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(appended) != 1 {
		t.Fatalf("expected 1 event, got %d", len(appended))
	}

	var meta map[string]any
	if err := json.Unmarshal(appended[0].Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	// caller-supplied keys present
	if meta["actor"] != "user123" || meta["actorCollection"] != "users" {
		t.Fatalf("missing actor metadata: %v", meta)
	}
	// decider-supplied keys preserved
	if meta["source"] != "decider" {
		t.Fatalf("decider metadata lost: %v", meta)
	}

	// persisted too
	stream, err := r.store.LoadStream(ctx, "counter", "c1")
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(stream[0].Metadata, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted["actor"] != "user123" {
		t.Fatalf("actor metadata not persisted: %v", persisted)
	}
}

func TestHandleWithoutMetaKeepsDeciderMetadata(t *testing.T) {
	r := setup(t)

	appended, err := r.Handle(context.Background(), "counter", "c2",
		Command{Name: "Increment", Payload: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(appended[0].Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["source"] != "decider" {
		t.Fatalf("decider metadata lost: %v", meta)
	}
	if _, ok := meta["actor"]; ok {
		t.Fatalf("unexpected actor key: %v", meta)
	}
}
