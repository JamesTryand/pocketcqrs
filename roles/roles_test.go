package roles

import (
	"slices"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// TestEnsureCollectionShape pins the shape Item 11's design relies on:
// password auth stays ON (unlike pocketcqrs-extensions' service_accounts,
// these records log in themselves), the capabilities field exists and is
// NOT hidden (authverify.RequireCapability's remote-verify path reads it
// from the master's serialized verdict -- a hidden field would silently
// vanish there), and every collection rule is nil (superuser-only
// management, matching a superuser's own sensitivity).
func TestEnsureCollectionShape(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	if err := ensureCollection(app); err != nil {
		t.Fatalf("ensureCollection: %v", err)
	}

	c, err := app.FindCollectionByNameOrId(CollectionName)
	if err != nil {
		t.Fatalf("collection not created: %v", err)
	}
	if !c.IsAuth() {
		t.Fatal("expected an auth collection")
	}
	if !c.PasswordAuth.Enabled {
		t.Error("expected PasswordAuth enabled -- roles records log in themselves")
	}
	jf, ok := c.Fields.GetByName(CapabilitiesField).(*core.JSONField)
	if !ok {
		t.Fatalf("expected a %q JSON field", CapabilitiesField)
	}
	if jf.Hidden {
		t.Error("capabilities must not be hidden -- the remote-verify path reads it from the serialized verdict")
	}
	if c.ListRule != nil || c.ViewRule != nil || c.CreateRule != nil || c.UpdateRule != nil || c.DeleteRule != nil {
		t.Error("expected every rule nil (superuser-only)")
	}

	// idempotent: calling again (as a re-run migration would on an
	// already-provisioned deployment) must not error or duplicate
	if err := ensureCollection(app); err != nil {
		t.Fatalf("second ensureCollection call: %v", err)
	}
}

// TestCapabilitiesRoundTrip proves a real record's capabilities field
// survives a save/reload cycle intact -- what authverify.RequireCapability
// actually depends on.
func TestCapabilitiesRoundTrip(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	if err := ensureCollection(app); err != nil {
		t.Fatal(err)
	}
	c, err := app.FindCollectionByNameOrId(CollectionName)
	if err != nil {
		t.Fatal(err)
	}

	rec := core.NewRecord(c)
	rec.SetEmail("poweruser@example.com")
	rec.SetPassword("1234567890")
	rec.Set(CapabilitiesField, []string{"ops.events.read", "ops.catalog.read"})
	if err := app.Save(rec); err != nil {
		t.Fatal(err)
	}

	reloaded, err := app.FindRecordById(CollectionName, rec.Id)
	if err != nil {
		t.Fatal(err)
	}
	got := reloaded.GetStringSlice(CapabilitiesField)
	for _, want := range []string{"ops.events.read", "ops.catalog.read"} {
		if !slices.Contains(got, want) {
			t.Fatalf("capabilities round-trip missing %q: got %v", want, got)
		}
	}
}
