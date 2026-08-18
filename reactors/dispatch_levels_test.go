package reactors

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

// The two arms have to stay apart, and the reason is the whole of F-2.
//
// A domain rejection is the AT-LEAST-ONCE IDEMPOTENCY PATH working: a
// redelivered reaction hits the target's own "already exists" rule and is
// correctly refused. Promoting that to a warning would make correct operation
// noisy, and dead-lettering it would be wrong outright.
//
// An unknown aggregate is a PERMANENT wiring fault. No redelivery can ever
// succeed, the reaction is lost, and the checkpoint advances past it. Logging
// that at the same level as the case above is how it stayed invisible for five
// resume points while the decider collision two lines earlier logged at WARN.
func TestDispatchSeparatesPermanentFaultsFromDomainRejections(t *testing.T) {
	ctx := context.Background()
	trigger := events.Event{ID: "e1", Metadata: []byte(`{}`)}
	reactions := []Reaction{
		{Aggregate: "gone", ID: "x1", Command: decider.Command{Name: "DoThing"}},
	}

	t.Run("unknown aggregate warns", func(t *testing.T) {
		var logged, warned []string
		d := &fakeDispatcher{err: fmt.Errorf("%w: %q", decider.ErrUnknownAggregate, "gone")}

		err := Dispatch(ctx, d, "r", trigger, reactions,
			func(m string, _ ...any) { logged = append(logged, m) },
			func(m string, _ ...any) { warned = append(warned, m) })
		if err != nil {
			t.Fatalf("a permanent fault must not block the log: %v", err)
		}
		if len(warned) != 1 {
			t.Fatalf("a dropped reaction must be warned about, got %d warnings", len(warned))
		}
		if len(logged) != 0 {
			t.Errorf("a permanent fault must not also go down the ordinary path: %v", logged)
		}
	})

	t.Run("domain rejection stays quiet", func(t *testing.T) {
		var logged, warned []string
		d := &fakeDispatcher{err: errors.New("task already exists")}

		err := Dispatch(ctx, d, "r", trigger, reactions,
			func(m string, _ ...any) { logged = append(logged, m) },
			func(m string, _ ...any) { warned = append(warned, m) })
		if err != nil {
			t.Fatalf("a domain rejection must not block the log: %v", err)
		}
		if len(warned) != 0 {
			t.Errorf("the idempotency path must NOT warn — correct operation would become noisy: %v", warned)
		}
		if len(logged) != 1 {
			t.Fatalf("a domain rejection should still be logged once, got %d", len(logged))
		}
	})
}

// warn is optional at both call sites, so a caller that passes nil must get
// the old behaviour rather than a panic.
func TestDispatchFallsBackToLoggerWithoutAWarnFunc(t *testing.T) {
	var logged int
	d := &fakeDispatcher{err: fmt.Errorf("%w: %q", decider.ErrUnknownAggregate, "gone")}

	err := Dispatch(context.Background(), d, "r",
		events.Event{ID: "e1", Metadata: []byte(`{}`)},
		[]Reaction{{Aggregate: "gone", ID: "x", Command: decider.Command{Name: "C"}}},
		func(string, ...any) { logged++ }, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if logged != 1 {
		t.Fatalf("a nil warn must fall back to logger, got %d calls", logged)
	}
}
