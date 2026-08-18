package reactors

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

type ledgerState struct{ Lines int }

// ledger deliberately has NO natural uniqueness rule: adding the same line
// twice is a legitimate outcome. That is precisely the case the old "the
// target rejects it" story could not cover, and the reason a commandId is
// needed here at all.
func ledger() *decider.Decider[ledgerState] {
	return &decider.Decider[ledgerState]{
		InitialState: func() ledgerState { return ledgerState{} },
		Decide: func(cmd decider.Command, _ ledgerState) ([]events.NewEvent, error) {
			return []events.NewEvent{{Type: "LineAdded", Data: json.RawMessage(`{}`)}}, nil
		},
		Evolve: func(s ledgerState, _ events.Event) (ledgerState, error) {
			s.Lines++
			return s, nil
		},
	}
}

type addLine struct{}

func (addLine) Name() string { return "addLine" }
func (addLine) React(ev events.Event) []Reaction {
	return []Reaction{{
		Aggregate: "ledger", ID: "l1",
		Command: decider.Command{Name: "AddLine", Payload: json.RawMessage(`{}`)},
	}}
}

func ledgerSetup(t *testing.T) (*events.Store, *decider.Registry, events.Event) {
	t.Helper()
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	reg := decider.NewRegistry(store)
	decider.Register(reg, "ledger", ledger())

	caused, err := store.Append(context.Background(), "order", "o1", 0,
		[]events.NewEvent{{Type: "OrderPlaced", Data: json.RawMessage(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	return store, reg, caused[0]
}

// Delivery is at-least-once, so the same source event can arrive more than
// once. The reaction's commandId is derived from the cause, so the redelivery
// is recognised as already applied instead of adding a second line.
func TestRedeliveredReactionDoesNotApplyTwice(t *testing.T) {
	store, reg, trigger := ledgerSetup(t)
	ctx := context.Background()
	r := addLine{}

	for i := 0; i < 3; i++ {
		if err := Dispatch(ctx, reg, r.Name(), trigger, r.React(trigger), nil, nil); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}

	stream, err := store.LoadStream(ctx, "ledger", "l1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stream) != 1 {
		t.Fatalf("three deliveries of one event must add one line, got %d — this domain "+
			"has no uniqueness rule, so only the commandId can stop it", len(stream))
	}
}

// Two DIFFERENT causes must each apply, or the second would be swallowed.
func TestDistinctCausesEachApply(t *testing.T) {
	store, reg, _ := ledgerSetup(t)
	ctx := context.Background()

	appended, err := store.Append(ctx, "order", "o2", 0, []events.NewEvent{
		{Type: "OrderPlaced", Data: json.RawMessage(`{}`)},
		{Type: "OrderPlaced", Data: json.RawMessage(`{}`)},
	})
	if err != nil {
		t.Fatal(err)
	}

	r := addLine{}
	for _, trigger := range appended {
		if err := Dispatch(ctx, reg, r.Name(), trigger, r.React(trigger), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	stream, _ := store.LoadStream(ctx, "ledger", "l1")
	if len(stream) != 2 {
		t.Fatalf("two distinct causes must each add a line, got %d", len(stream))
	}
}

// The event must carry the commandId, or CommandApplied has nothing to find.
func TestReactionStampsItsCommandID(t *testing.T) {
	store, reg, trigger := ledgerSetup(t)
	ctx := context.Background()
	r := addLine{}
	if err := Dispatch(ctx, reg, r.Name(), trigger, r.React(trigger), nil, nil); err != nil {
		t.Fatal(err)
	}

	stream, _ := store.LoadStream(ctx, "ledger", "l1")
	var meta map[string]any
	if err := json.Unmarshal(stream[0].Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	got, _ := meta["commandId"].(string)
	if got == "" {
		t.Fatal("a reaction's event must carry a commandId")
	}
	if want := reactionCommandID(r.Name(), trigger.ID, 0); got != want {
		t.Errorf("commandId must be derived from the cause: got %s, want %s", got, want)
	}
}

// Derivation is stable across deliveries and distinct across reactions —
// the two properties the whole scheme rests on.
func TestReactionCommandIDDerivation(t *testing.T) {
	a := reactionCommandID("autoShip", "evt-123", 0)
	if b := reactionCommandID("autoShip", "evt-123", 0); a != b {
		t.Fatalf("the same inputs must derive the same id:\n %s\n %s", a, b)
	}
	for _, other := range []string{
		reactionCommandID("autoShip", "evt-123", 1),
		reactionCommandID("autoShip", "evt-124", 0),
		reactionCommandID("notify", "evt-123", 0),
	} {
		if other == a {
			t.Errorf("distinct reactions must not share an id, both were %s", a)
		}
	}
}
