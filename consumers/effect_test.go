package consumers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jamestryand/pocketcqrs/events"
)

// tiered records what the engine handed it, by method.
type tiered struct {
	name    string
	effect  bool
	applied []string
	folded  []string
}

func (c *tiered) Name() string   { return c.name }
func (c *tiered) IsEffect() bool { return c.effect }
func (c *tiered) Apply(_ context.Context, ev events.Event) error {
	c.applied = append(c.applied, ev.ID)
	return nil
}

// folding is a tiered consumer that also folds (a process manager).
type folding struct{ tiered }

func (c *folding) Fold(_ context.Context, ev events.Event) error {
	c.folded = append(c.folded, ev.ID)
	return nil
}

// importedLog is one native event, then two imported ones, then another
// native one: n1, i1, i2, n2.
func importedLog(t *testing.T) *events.Store {
	t.Helper()
	var dir string
	store := openStore(t, &dir)
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if _, err := store.Append(ctx, "task", "t1", 0, []events.NewEvent{{Type: "TaskCreated", Data: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportEvents(ctx, []events.Event{
		{ID: "i1", Aggregate: "order", AggregateID: "o1", Sequence: 1, Type: "OrderPlaced", Data: json.RawMessage(`{}`), Created: "2026-10-01 10:00:00.000Z"},
		{ID: "i2", Aggregate: "order", AggregateID: "o1", Sequence: 2, Type: "OrderShipped", Data: json.RawMessage(`{}`), Created: "2026-10-01 10:05:00.000Z"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, "task", "t2", 0, []events.NewEvent{{Type: "TaskCreated", Data: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	return store
}

// Imported events already happened at another deployment, side effects
// included. An effect-tier consumer must not act on them again, but must
// still SEE them: "skip" means don't act, not don't see. One that folds its
// own state (a process manager, cqrs-patterns rule 6) folds them, so its
// running state is right for the next native event. Every other consumer
// (a projection) applies them as usual. Every checkpoint passes them,
// whatever the consumer's lag, with no checkpoint moved by the import.
func TestImportedEventsAreFoldedButNotActedOn(t *testing.T) {
	store := importedLog(t)
	ctx := context.Background()
	evs, err := store.Poll(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	n1, n2 := evs[0].ID, evs[3].ID

	effect := &tiered{name: "effect", effect: true}
	manager := &folding{tiered{name: "manager", effect: true}}
	projection := &tiered{name: "projection"}
	engine := NewEngine(store, func(string, ...any) {})
	engine.Register(effect)
	engine.Register(manager)
	engine.Register(projection)
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	same := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	if !same(effect.applied, []string{n1, n2}) {
		t.Errorf("an effect consumer acts on native events only: %v", effect.applied)
	}
	if !same(manager.applied, []string{n1, n2}) || !same(manager.folded, []string{"i1", "i2"}) {
		t.Errorf("a process manager acts on native events and folds imported ones: applied %v, folded %v",
			manager.applied, manager.folded)
	}
	if !same(projection.applied, []string{n1, "i1", "i2", n2}) {
		t.Errorf("a projection applies everything: %v", projection.applied)
	}
	for _, name := range []string{"effect", "manager", "projection"} {
		if pos, _ := store.Checkpoint(ctx, name); pos != 4 {
			t.Errorf("%s: checkpoint %d, want 4", name, pos)
		}
	}
}
