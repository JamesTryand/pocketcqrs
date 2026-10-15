package functions

import (
	"testing"

	"github.com/jamestryand/pocketcqrs/consumers"
)

// JS reactors and event-triggered functions act on the world, so the engine
// folds an imported event past them without running them (consumers.Effect).
// A JS projection is not effect-tier: it applies imported history.
func TestJSEffectTiers(t *testing.T) {
	if !consumers.IsEffect(&ReactorSpec{}) {
		t.Error("a JS reactor is effect-tier")
	}
	if !consumers.IsEffect(&eventFunction{}) {
		t.Error("an event-triggered function is effect-tier")
	}
	if consumers.IsEffect(&JSProjection{}) {
		t.Error("a JS projection is not effect-tier")
	}
}
