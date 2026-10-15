package extcaller

import (
	"testing"

	"github.com/jamestryand/pocketcqrs/consumers"
)

// The external caller calls out, so it makes no call for an imported event
// (consumers.Effect).
func TestExternalCallerIsEffectTier(t *testing.T) {
	if !consumers.IsEffect(&Consumer{}) {
		t.Error("the external caller is effect-tier")
	}
}
