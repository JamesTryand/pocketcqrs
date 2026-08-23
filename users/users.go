// Package users provisions the users PocketBase collection: a genuinely
// generic, app-level end-user auth collection, distinct from every other
// collection this project provisions:
//
//   - _superusers (PocketBase's own) and roles (this project's, see the
//     roles package) both gate operator/ops-dashboard access
//     (authverify.RequireSuperuser / RequireCapability) — neither route ever
//     looks at this collection.
//   - service_accounts (pocketcqrs-extensions) is for non-human integration
//     credentials, never logged into directly.
//
// users is none of those: it is a blank, ordinary PocketBase auth
// collection meant for whatever end-user identity a consuming app actually
// needs. Core ships the collection and a sensible self-service rule set;
// the app supplies its own fields via its own migration (core's own
// ensureCollection is create-only, so an app extending an already-existing
// "users" is not fighting a later core boot re-asserting a narrower shape
// — see ensureCollection's doc comment). This deliberately mirrors
// serviceaccounts.RegisterCollection()'s "provision the collection, let
// something else own what happens with the rows" split.
package users

import (
	"sync"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/migrations"
	"github.com/pocketbase/pocketbase/tools/types"
)

// CollectionName is the fixed name of the collection this package
// provisions.
const CollectionName = "users"

var once sync.Once

// RegisterCollection adds the users migration to PocketBase's
// app-migrations list. Call it before the deployment's app starts —
// core.AppMigrations is a process-global list read by RunAppMigrations
// during bootstrap, the same timing pocketcqrs core's own
// migrations.RegisterExamples and roles.RegisterCollection document.
//
// Always registered, not --tutorial-gated: this is core's own shipped
// primitive, the same posture roles.RegisterCollection takes for Item 11.
//
// sync.Once because core.AppMigrations is process-global: calling twice
// would append duplicate entries for the same migration.
func RegisterCollection() {
	once.Do(func() {
		migrations.Register(ensureCollection, removeCollection,
			"pocketcqrs_2_users_collection")
	})
}

// ensureCollection creates the users collection if absent: an ordinary
// auth collection (password auth ON, PocketBase's own default — nothing
// Microsoft/OAuth2-specific baked in here; an app or operator wanting
// Entra-only sign-in, or any other provider, configures that afterward
// through PocketBase's own admin UI, the collection itself doesn't care)
// with NO extra fields and a self-service rule set:
//
//   - CreateRule "" (public): open self-registration out of the box —
//     the single most common baseline for a collection literally named
//     "users", and easy for an app to lock down (invite-only, disabled
//     entirely, ...) via its own later Save, which is exactly the "used
//     at the app level" contract this collection is meant to support.
//   - ListRule/ViewRule/UpdateRule/DeleteRule "@request.auth.id = id":
//     a user sees and manages only their own record. Nothing here grants
//     any access beyond that record — critically, NO capabilities field,
//     so a users record can never satisfy authverify.RequireCapability or
//     RequireSuperuser no matter what an app adds to it later; ops/
//     dashboard access stays exactly as gated as it was before this
//     collection existed.
//
// Deliberately create-only, not reconciled on every boot the way
// //@schema collections are: an app is expected to extend this collection
// with its own fields via its own migration (app.Save after
// FindCollectionByNameOrId, adding fields — the exact pattern
// functions/schema.go's reconcileBase already uses for additive schema
// changes). If core's own migration instead re-asserted "no extra fields"
// on every boot, an app's own additions would never be a safe, permanent
// extension.
func ensureCollection(app core.App) error {
	if _, err := app.FindCollectionByNameOrId(CollectionName); err == nil {
		return nil // already exists (an app's own migration may have already extended it)
	}

	c := core.NewAuthCollection(CollectionName)
	c.CreateRule = types.Pointer("")
	c.ListRule = types.Pointer("@request.auth.id = id")
	c.ViewRule = types.Pointer("@request.auth.id = id")
	c.UpdateRule = types.Pointer("@request.auth.id = id")
	c.DeleteRule = types.Pointer("@request.auth.id = id")
	return app.Save(c)
}

func removeCollection(app core.App) error {
	c, err := app.FindCollectionByNameOrId(CollectionName)
	if err != nil {
		return nil // already deleted
	}
	return app.Delete(c)
}
