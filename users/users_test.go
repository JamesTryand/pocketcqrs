package users

import (
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// freshApp opens a test app with any PRE-EXISTING "users" collection
// renamed out of the way first.
//
// tests.NewTestApp() clones pocketbase/tests' own bundled fixture data.db,
// which -- confirmed by direct investigation, not assumed -- already ships
// a demo "users" collection of its own (PocketBase's long-standing test
// fixture, unrelated to this project) that other fixture collections
// (demo1, view1, view2) hold relations into, so it can't simply be
// deleted. A real pocketcqrs boot's pb_data never has any of this: no
// "users" string appears anywhere in PocketBase's own non-test source, and
// RunAppMigrations starts from a genuinely empty data.db. Without this
// rename, every test below would silently exercise the FIXTURE's
// collection instead of ensureCollection's own create path -- which is
// exactly what happened the first time these tests were written
// (ListRule/ViewRule/etc. mismatches that looked like a save bug, but were
// actually the fixture's own unrelated rule strings). Renaming (not
// deleting) sidesteps the relation references entirely: PocketBase
// resolves relations by collection id, not name.
func freshApp(t *testing.T) core.App {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	if existing, err := app.FindCollectionByNameOrId(CollectionName); err == nil {
		existing.Name = "zz_fixture_" + CollectionName
		if err := app.Save(existing); err != nil {
			t.Fatalf("renaming fixture's pre-existing %q collection out of the way: %v", CollectionName, err)
		}
	}
	return app
}

// TestEnsureCollectionShape pins the self-service rule set and password
// auth staying on -- the "usable at the app level out of the box" contract
// this collection exists for.
func TestEnsureCollectionShape(t *testing.T) {
	app := freshApp(t)

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
		t.Error("expected PasswordAuth enabled by default")
	}
	if c.CreateRule == nil || *c.CreateRule != "" {
		t.Errorf("expected an open (public) CreateRule, got %v", c.CreateRule)
	}
	for name, rule := range map[string]*string{
		"ListRule": c.ListRule, "ViewRule": c.ViewRule,
		"UpdateRule": c.UpdateRule, "DeleteRule": c.DeleteRule,
	} {
		if rule == nil {
			t.Errorf("%s: expected self-only rule, got nil", name)
			continue
		}
		if *rule != "@request.auth.id = id" {
			t.Errorf("%s: expected self-only rule, got %q", name, *rule)
		}
	}

	// idempotent: calling again (as a re-run migration would on an
	// already-provisioned deployment) must not error or duplicate
	if err := ensureCollection(app); err != nil {
		t.Fatalf("second ensureCollection call: %v", err)
	}
}

// TestEnsureCollectionIsCreateOnly proves the design's core promise: once
// an app has extended the collection with its own field, a later
// "ensureCollection" call (as a fresh boot's migration replay would do)
// must NOT touch it -- the collection already exists, so the function
// returns immediately. If this ever changed to a full reconcile that
// reasserted "no extra fields", an app's own extension would be silently
// unsafe to rely on.
func TestEnsureCollectionIsCreateOnly(t *testing.T) {
	app := freshApp(t)

	if err := ensureCollection(app); err != nil {
		t.Fatal(err)
	}
	c, err := app.FindCollectionByNameOrId(CollectionName)
	if err != nil {
		t.Fatal(err)
	}

	// simulate an app's own migration extending the collection
	c.Fields.Add(&core.TextField{Name: "displayName"})
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}

	// a later boot re-running the core migration must leave it alone
	if err := ensureCollection(app); err != nil {
		t.Fatal(err)
	}
	reloaded, err := app.FindCollectionByNameOrId(CollectionName)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Fields.GetByName("displayName") == nil {
		t.Fatal("app-added field was lost by a later ensureCollection call")
	}
}

// TestUsersRecordCannotSatisfyCapabilityGate is the "does not break the
// out-of-the-box internal stuff" property, proven rather than asserted: a
// users record -- even one an app has freely extended with its own fields
// -- carries no "capabilities" field, so authverify.RequireCapability (the
// gate the 5 Item 11 ops routes use) sees it as an ordinary, ungranted
// record. Self-signup being open on this collection must never translate
// into any ops/dashboard access.
func TestUsersRecordCannotSatisfyCapabilityGate(t *testing.T) {
	app := freshApp(t)
	if err := ensureCollection(app); err != nil {
		t.Fatal(err)
	}
	c, err := app.FindCollectionByNameOrId(CollectionName)
	if err != nil {
		t.Fatal(err)
	}

	rec := core.NewRecord(c)
	rec.SetEmail("enduser@example.com")
	rec.SetPassword("1234567890")
	if err := app.Save(rec); err != nil {
		t.Fatal(err)
	}

	if rec.IsSuperuser() {
		t.Fatal("a users record must never be a superuser")
	}
	if got := rec.GetStringSlice("capabilities"); len(got) != 0 {
		t.Fatalf("expected no capabilities on a users record, got %v", got)
	}
}
