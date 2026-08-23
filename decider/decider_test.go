package decider

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/events"
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

func TestUnregister(t *testing.T) {
	r := setup(t)
	ctx := context.Background()

	if _, err := r.Handle(ctx, "counter", "c1", Command{Name: "Increment"}); err != nil {
		t.Fatal(err)
	}

	// unregistered aggregates are unknown again
	r.Unregister("counter")
	r.Unregister("no-such-aggregate") // absent: no-op
	if _, err := r.Handle(ctx, "counter", "c1", Command{Name: "Increment"}); !errors.Is(err, ErrUnknownAggregate) {
		t.Fatalf("expected ErrUnknownAggregate, got %v", err)
	}
	if r.Has("counter") {
		t.Fatal("Has reports true after Unregister")
	}

	// re-registering works; history is intact (folds to count 1, appends #2)
	Register(r, "counter", counter())
	appended, err := r.Handle(ctx, "counter", "c1", Command{Name: "Increment"})
	if err != nil {
		t.Fatal(err)
	}
	if len(appended) != 1 || appended[0].Sequence != 2 {
		t.Fatalf("unexpected events after re-register: %+v", appended)
	}
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

// gatedState mirrors the shape of an authorization check a real decider
// needs -- e.g. rotaboard's requirePermission -- to prove cmd.Actor is
// actually usable for one, not just present on the struct. See F-14: before
// this fix, a typed Decider[S] had no way to express this at all.
type gatedState struct {
	Granted map[string]bool
}

func gated() *Decider[gatedState] {
	return &Decider[gatedState]{
		InitialState: func() gatedState { return gatedState{Granted: map[string]bool{}} },
		Decide: func(cmd Command, state gatedState) ([]events.NewEvent, error) {
			switch cmd.Name {
			case "Grant":
				return []events.NewEvent{{Type: "Granted", Data: json.RawMessage(`{"who":"` + cmd.Actor + `"}`)}}, nil
			case "DoGatedThing":
				if cmd.Actor == "" || !state.Granted[cmd.Actor] {
					return nil, errors.New("actor lacks permission")
				}
				return []events.NewEvent{{Type: "DidGatedThing", Data: json.RawMessage(`{"now":"` + cmd.Now + `"}`)}}, nil
			}
			return nil, errors.New("unknown command: " + cmd.Name)
		},
		Evolve: func(state gatedState, ev events.Event) (gatedState, error) {
			if ev.Type == "Granted" {
				var d struct {
					Who string `json:"who"`
				}
				if err := json.Unmarshal(ev.Data, &d); err != nil {
					return state, err
				}
				state.Granted[d.Who] = true
			}
			return state, nil
		},
	}
}

func TestTypedDeciderSeesActorAndNow(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	r := NewRegistry(store)
	Register(r, "gated", gated())
	ctx := context.Background()

	// no actor: an unauthorized attempt is refused, exactly the case that
	// was impossible to express before this fix.
	if _, err := r.HandleWithMeta(ctx, "gated", "g1",
		Command{Name: "DoGatedThing"}, map[string]any{"actor": "alice"}); err == nil {
		t.Fatal("expected DoGatedThing to be refused before any grant")
	}

	// alice grants herself, stamping who via cmd.Actor -- bob never does
	if _, err := r.HandleWithMeta(ctx, "gated", "g1",
		Command{Name: "Grant"}, map[string]any{"actor": "alice"}); err != nil {
		t.Fatal(err)
	}

	// alice, and only alice, can now act
	appended, err := r.HandleWithMeta(ctx, "gated", "g1",
		Command{Name: "DoGatedThing"}, map[string]any{"actor": "alice"})
	if err != nil {
		t.Fatalf("expected alice to be authorized: %v", err)
	}
	if len(appended) != 1 || appended[0].Type != "DidGatedThing" {
		t.Fatalf("unexpected events: %+v", appended)
	}
	var payload struct {
		Now string `json:"now"`
	}
	if err := json.Unmarshal(appended[0].Data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Now == "" {
		t.Fatal("expected cmd.Now to be populated, got empty string")
	}

	if _, err := r.HandleWithMeta(ctx, "gated", "g1",
		Command{Name: "DoGatedThing"}, map[string]any{"actor": "bob"}); err == nil {
		t.Fatal("expected bob (never granted) to still be refused")
	}
}

// TestTypedDeciderSeesProvenance mirrors TestTypedDeciderSeesActorAndNow for
// the Provenance field: populated from meta["provenance"] when present,
// empty when absent -- the same convention Actor already follows.
func TestTypedDeciderSeesProvenance(t *testing.T) {
	var seen []string
	echo := &Decider[struct{}]{
		InitialState: func() struct{} { return struct{}{} },
		Decide: func(cmd Command, state struct{}) ([]events.NewEvent, error) {
			seen = append(seen, cmd.Provenance)
			return []events.NewEvent{{Type: "Echoed", Data: json.RawMessage(`{}`)}}, nil
		},
		Evolve: func(state struct{}, ev events.Event) (struct{}, error) { return state, nil },
	}
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	r := NewRegistry(store)
	Register(r, "echo", echo)
	ctx := context.Background()

	if _, err := r.HandleWithMeta(ctx, "echo", "e1",
		Command{Name: "Do"}, map[string]any{"provenance": "federated:peer1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.HandleWithMeta(ctx, "echo", "e2",
		Command{Name: "Do"}, map[string]any{"actor": "user123"}); err != nil {
		t.Fatal(err)
	}

	if len(seen) != 2 || seen[0] != "federated:peer1" || seen[1] != "" {
		t.Fatalf("unexpected provenance values seen: %+v", seen)
	}
}

// fakeLoader lets a test control exactly what LoadStream returns,
// independent of what's actually durable in the store -- standing in for
// the batching writer's per-window overlay of not-yet-committed events.
type fakeLoader struct {
	events []events.Event
}

func (f fakeLoader) LoadStream(context.Context, string, string) ([]events.Event, error) {
	return f.events, nil
}

func TestDecideWithMetaUsesTheGivenLoaderNotTheStore(t *testing.T) {
	r := setup(t)
	ctx := context.Background()

	// the real store has nothing for c1 -- decide against a loader that
	// claims 3 events already exist, standing in for a batch overlay
	loader := fakeLoader{events: []events.Event{
		{Type: "Incremented", Sequence: 1},
		{Type: "Incremented", Sequence: 2},
		{Type: "Incremented", Sequence: 3},
	}}

	newEvents, expectedSeq, err := r.DecideWithMeta(ctx, loader, "counter", "c1",
		Command{Name: "Increment"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if expectedSeq != 3 {
		t.Fatalf("expected sequence 3 (the loader's view), got %d", expectedSeq)
	}
	if len(newEvents) != 1 {
		t.Fatalf("expected 1 decided event, got %d", len(newEvents))
	}

	// DecideWithMeta never writes -- the real store is untouched
	stream, err := r.store.LoadStream(ctx, "counter", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stream) != 0 {
		t.Fatalf("expected the real store untouched, found %d events", len(stream))
	}
}

func TestDecideWithMetaStampsNowOnlyIfAbsent(t *testing.T) {
	r := setup(t)
	ctx := context.Background()
	loader := fakeLoader{}

	meta := map[string]any{"now": "2020-01-01 00:00:00.000Z"}
	newEvents, _, err := r.DecideWithMeta(ctx, loader, "counter", "c1", Command{Name: "Increment"}, meta)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(newEvents[0].Metadata, &got); err != nil {
		t.Fatal(err)
	}
	if got["now"] != "2020-01-01 00:00:00.000Z" {
		t.Fatalf("expected the caller-supplied now to be preserved, got %v", got["now"])
	}

	newEvents2, _, err := r.DecideWithMeta(ctx, loader, "counter", "c1", Command{Name: "Increment"}, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var got2 map[string]any
	if err := json.Unmarshal(newEvents2[0].Metadata, &got2); err != nil {
		t.Fatal(err)
	}
	if got2["now"] == nil || got2["now"] == "" {
		t.Fatal("expected now to be filled in when absent")
	}
	// F-16: the stamped format must be real RFC3339 (T-separated), not the
	// space-separated near-ISO format that broke downstream date-time
	// consumers.
	now, ok := got2["now"].(string)
	if !ok {
		t.Fatalf("expected now to be a string, got %T", got2["now"])
	}
	if _, err := time.Parse(time.RFC3339, now); err != nil {
		t.Fatalf("stamped now %q is not RFC3339: %v", now, err)
	}
}

func TestHandleWithMetaComposesDecideWithMetaAndAppend(t *testing.T) {
	r := setup(t)
	ctx := context.Background()

	expectedSeq0, err := r.store.LoadStream(ctx, "counter", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(expectedSeq0) != 0 {
		t.Fatal("expected a fresh stream")
	}

	newEvents, expectedSeq, err := r.DecideWithMeta(ctx, r.store, "counter", "c1", Command{Name: "Increment"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if expectedSeq != 0 {
		t.Fatalf("expected sequence 0 for a fresh stream, got %d", expectedSeq)
	}

	appended, err := r.HandleWithMeta(ctx, "counter", "c1", Command{Name: "Increment"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(appended) != len(newEvents) {
		t.Fatalf("HandleWithMeta and DecideWithMeta produced different event counts: %d vs %d", len(appended), len(newEvents))
	}
	if appended[0].Sequence != 1 {
		t.Fatalf("expected the composed HandleWithMeta to append at sequence 1, got %d", appended[0].Sequence)
	}
}
