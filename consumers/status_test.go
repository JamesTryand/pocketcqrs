package consumers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/events"
)

// Status: health/telemetry contract sections 4.4-4.5, STATE-MACHINES.md
// machine 2 (ReadModelConsumer).

// funcConsumer applies with fn.
type funcConsumer struct {
	name string
	fn   func(events.Event) error
}

func (f *funcConsumer) Name() string                                   { return f.name }
func (f *funcConsumer) Apply(_ context.Context, ev events.Event) error { return f.fn(ev) }

// projectionLike owns collections, so it is a read model without saying so.
type projectionLike struct{ funcConsumer }

func (projectionLike) Collections() []string { return []string{"orders"} }

// seeded is an engine over a store holding n events, with its clock an hour
// after they were written, so anything pending is well past any threshold.
func seeded(t *testing.T, n int) (*events.Store, *Engine) {
	t.Helper()
	var dir string
	store := openStore(t, &dir)
	t.Cleanup(func() { store.Close() })
	for i := 0; i < n; i++ {
		appendOne(t, store, "t"+string(rune('a'+i)))
	}
	engine := NewEngine(store, nil)
	engine.now = func() time.Time { return time.Now().Add(time.Hour) }
	return store, engine
}

func only(t *testing.T, e *Engine) Status {
	t.Helper()
	all := e.Status()
	if len(all) != 1 {
		t.Fatalf("want one consumer, got %+v", all)
	}
	return all[0]
}

func TestStatusBeforeTheFirstPassIsBehindWithUnknownLag(t *testing.T) {
	_, engine := seeded(t, 3)
	engine.Register(&recorder{})

	s := only(t, engine)
	if s.State != Behind || s.Checkpoint != nil || s.LagPositions != nil || s.LagSeconds != nil {
		t.Errorf("got %+v, want behind with nothing known", s)
	}
}

func TestStatusAfterCatchingUpIsCurrentWithZeroLag(t *testing.T) {
	_, engine := seeded(t, 3)
	engine.Register(&recorder{})

	if err := engine.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	s := only(t, engine)
	if s.State != Current || *s.Checkpoint != 3 || *s.LagPositions != 0 || *s.LagSeconds != 0 {
		t.Errorf("got %+v (checkpoint %d, lag %d / %v)", s, *s.Checkpoint, *s.LagPositions, *s.LagSeconds)
	}
}

func TestOnlyConsumersThatOwnCollectionsOrSaySoAreReadModels(t *testing.T) {
	_, engine := seeded(t, 1)
	engine.Register(&projectionLike{funcConsumer{name: "orders"}})
	engine.Register(&funcConsumer{name: "reactor"})
	engine.Register(&explicit{funcConsumer{name: "search"}, true})
	engine.Register(&explicit{funcConsumer{name: "rebuild"}, false})

	got := map[string]bool{}
	for _, s := range engine.Status() {
		got[s.Name] = s.ReadModel
	}
	want := map[string]bool{"orders": true, "reactor": false, "search": true, "rebuild": false}
	for name, readModel := range want {
		if got[name] != readModel {
			t.Errorf("%s: read model %v, want %v", name, got[name], readModel)
		}
	}
}

type explicit struct {
	funcConsumer
	readModel bool
}

func (e *explicit) IsReadModel() bool { return e.readModel }

func TestAFailingConsumerIsBlockedAtTheFailingEventWithItsLagGrowing(t *testing.T) {
	_, engine := seeded(t, 3)
	engine.Register(&funcConsumer{name: "p", fn: func(ev events.Event) error {
		if ev.Position == 2 {
			return errors.New("boom")
		}
		return nil
	}})

	// a retry pass keeps it blocked (it fails at the same event again)
	for range 2 {
		if err := engine.RunOnce(context.Background()); err == nil {
			t.Fatal("want the pass to fail")
		}
	}

	s := only(t, engine)
	if s.State != Blocked || *s.Checkpoint != 1 || *s.LagPositions != 2 {
		t.Errorf("got %+v (checkpoint %d, lag %d)", s, *s.Checkpoint, *s.LagPositions)
	}
	if *s.LagSeconds < 3500 || *s.LagSeconds > 3700 {
		t.Errorf("lag %v s, want about an hour", *s.LagSeconds)
	}
}

func TestABlockedConsumerThatGetsPastTheEventIsCurrentAgain(t *testing.T) {
	_, engine := seeded(t, 3)
	engine.now = time.Now
	failing := true
	engine.Register(&funcConsumer{name: "p", fn: func(ev events.Event) error {
		if failing && ev.Position == 2 {
			return errors.New("boom")
		}
		return nil
	}})

	_ = engine.RunOnce(context.Background())
	if s := only(t, engine); s.State != Blocked {
		t.Fatalf("got %v, want blocked", s.State)
	}
	failing = false
	if err := engine.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := only(t, engine); s.State != Current {
		t.Errorf("got %v, want current", s.State)
	}
}

func TestMidPassAConsumerIsBehindByTheAgeOfTheEventItIsApplying(t *testing.T) {
	_, engine := seeded(t, 3)
	var seen *Status
	engine.Register(&funcConsumer{name: "p", fn: func(ev events.Event) error {
		if ev.Position == 1 {
			s := only(t, engine)
			seen = &s
		}
		return nil
	}})

	if err := engine.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	if seen == nil || seen.State != Behind || *seen.Checkpoint != 0 || *seen.LagPositions != 3 {
		t.Fatalf("got %+v", seen)
	}
	if *seen.LagSeconds < 3500 || *seen.LagSeconds > 3700 {
		t.Errorf("lag %v s, want about an hour", *seen.LagSeconds)
	}
}

func TestLagWithinTheThresholdIsCurrent(t *testing.T) {
	_, engine := seeded(t, 1)
	engine.LagThreshold = 2 * time.Hour
	var seen State = -1
	engine.Register(&funcConsumer{name: "p", fn: func(events.Event) error {
		seen = only(t, engine).State
		return nil
	}})

	if err := engine.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen != Current {
		t.Errorf("got %v, want current", seen)
	}
}

func TestUnregisteringDropsTheConsumerFromStatus(t *testing.T) {
	_, engine := seeded(t, 1)
	engine.Register(&recorder{})
	if err := engine.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	engine.Unregister("rec")

	if all := engine.Status(); len(all) != 0 {
		t.Errorf("got %+v, want none", all)
	}
}
