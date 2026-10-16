package retention_test

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/kms"
	"github.com/jamestryand/pocketcqrs/pii"
	"github.com/jamestryand/pocketcqrs/retention"
)

// Governance is opt-in: a host that wires it gets the governed dataSubject
// decider and the governor, whichever of pii.Wire and retention.Wire it
// calls first.
func TestWireGovernsErasureWhicheverOrderTheHostWiresIn(t *testing.T) {
	t.Setenv(kms.EnvFacadeURL, "https://kms.example.test/")
	for _, retentionFirst := range []bool{true, false} {
		store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		reg := decider.NewRegistry(store)
		engine := consumers.NewEngine(store, nil)
		wirePII := func() {
			if _, err := pii.Wire(context.Background(), pii.WireOptions{Registry: reg, Engine: engine, Checkpoints: store, Loader: store}); err != nil {
				t.Fatal(err)
			}
		}
		wireRetention := func() {
			if _, err := retention.Wire(context.Background(), retention.WireOptions{Registry: reg, Engine: engine, Loader: store,
				Policy: retention.AllowAll{}}); err != nil {
				t.Fatal(err)
			}
		}
		if retentionFirst {
			wireRetention()
			wirePII()
		} else {
			wirePII()
			wireRetention()
		}
		got := reg.Commands(pii.SubjectAggregate)
		want := slices.Clone(retention.Commands)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("retention first=%v: commands %v", retentionFirst, got)
		}
		if !strings.Contains(strings.Join(engine.Names(), ","), retention.GovernorName) {
			t.Fatalf("retention first=%v: no governor in %v", retentionFirst, engine.Names())
		}
	}
}

func TestWireNeedsAPolicy(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = retention.Wire(context.Background(), retention.WireOptions{Registry: decider.NewRegistry(store),
		Engine: consumers.NewEngine(store, nil), Loader: store})
	if err == nil || !strings.Contains(err.Error(), "Policy") {
		t.Fatalf("a governor with no policy must not start: %v", err)
	}
}
