// Package roles provisions the roles PocketBase collection: the auth
// collection Item 11's capability-based ops/dashboard access model checks,
// alongside a genuine PocketBase superuser. See pocketbase-cqrs-faas's
// FAULTS-AND-WORK.md Item 11 entry for the full design; this package is the
// "provision the collection" half, authverify.RequireCapability the
// "consult it on a request" half.
//
// Deliberately not "poweruser": Item 11's accepted decision is a GENERAL
// per-capability model, not a single fixed tier, and this collection is
// meant to hold whatever role records a deployment needs -- poweruser is
// just the first one, expressed as a set of capability grants, not as the
// collection's name.
//
// Distinct from --cqrsRole (roleMaster/roleSecondary in main.go): that is
// this NODE's role in a multi-node deployment; this package is about what
// an authenticated PERSON is allowed to do. Two different concepts that
// happen to share the English word "role" -- unrelated code, unrelated
// collections.
package roles

import (
	"sync"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/migrations"
)

// CollectionName is the fixed name of the collection this package
// provisions.
const CollectionName = "roles"

// CapabilitiesField is the JSON array field authverify.RequireCapability
// reads via (*core.Record).GetStringSlice.
const CapabilitiesField = "capabilities"

var once sync.Once

// RegisterCollection adds the roles migration to PocketBase's
// app-migrations list. Call it before the deployment's app starts --
// core.AppMigrations is a process-global list read by RunAppMigrations
// during bootstrap, the same timing pocketcqrs core's own
// migrations.RegisterExamples documents.
//
// Unlike RegisterExamples, this is Item 11's own always-on feature, not
// something --tutorial gates: the ops/dashboard capability model applies to
// every deployment, so main.go calls this unconditionally.
//
// sync.Once because core.AppMigrations is process-global: calling twice
// would append duplicate entries for the same migration.
func RegisterCollection() {
	once.Do(func() {
		migrations.Register(ensureCollection, removeCollection,
			"pocketcqrs_1_roles_collection")
	})
}

// ensureCollection creates the roles collection if absent: an ordinary
// password-auth collection (unlike service_accounts, these records log in
// themselves -- a poweruser session is a real person at a dashboard) plus a
// capabilities JSON field holding the capability strings that record's
// holder is granted (e.g. "ops.events.read"; see ops.go's capOps*
// constants for the vocabulary Item 11 actually wires up).
//
// List/View/Create/Update/Delete rules are left nil (superuser-only, the
// core.Collection zero value): editing who holds which capability is
// exactly as sensitive as editing superusers, and PocketBase's own built-in
// admin UI (a superuser is required to reach it at all) already gives an
// operator a place to create a roles record and edit its capabilities
// array -- Item 11 deliberately does not build bespoke dashboard UI for
// this, per the decision doc's "at least a migration path" alternative.
func ensureCollection(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(CollectionName); err == nil {
		return nil // already exists
	}

	c := core.NewAuthCollection(CollectionName)
	c.Fields.Add(&core.JSONField{Name: CapabilitiesField})
	return app.Save(c)
}

func removeCollection(app core.App) error {
	c, err := app.FindCollectionByNameOrId(CollectionName)
	if err != nil {
		return nil // already deleted
	}
	return app.Delete(c)
}
