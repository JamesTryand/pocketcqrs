// Package adminapi holds the platform's catalog/ops/admin HTTP surface —
// introspection, hot reload, and function-file management — as an
// importable package, so a Go embedder reconstructing its own main.go (see
// docs/go-guide.md's wrapper pattern) can register the same admin routes
// pocketcqrs's own stock binary exposes, not just the write-side
// (gateway.RegisterRoutes) and read-model wiring it already had access to.
//
// Before this package existed, every admin/introspection route (catalog,
// admin/mode, admin/reload, admin/functions*, admin/dryrun, admin/scaffold,
// events, streams, deadletters*) lived in package main at pocketcqrs's own
// module root, registered by unexported, package-private functions. A
// consumer embedding pocketcqrs as a library could not reach them at all.
package adminapi

import (
	"sync"

	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/authverify"
	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/functions"
	"github.com/jamestryand/pocketcqrs/outbound"
	"github.com/jamestryand/pocketcqrs/projections"
)

// State holds everything the admin/introspection HTTP surface reads or
// mutates. It is the same struct pocketcqrs's own stock binary uses
// internally (as main.components, which embeds *State) — one source of
// truth for the shape, not a parallel copy that can drift.
//
// A hot reload (see ReloadFunctions) swaps FnRuntime, JSDeciders,
// JSReactors, JSProjs and CronJobs wholesale under ReloadMu — the mutex
// that discipline is why State is a struct an embedder constructs and
// holds onto, not an interface: an interface expressive enough to capture
// "these fields change together, atomically, under this lock" would need a
// setter per field, which is worse than the struct it would be describing.
//
// Fields that only the stock CLI needs (idempotency store, batch writer,
// node role, remote-verify cache) deliberately stay out of State — they
// are not part of the admin surface, and exporting them here would leak
// the stock binary's own private configuration as public API. See
// FAULTS-AND-WORK.md's Item 10 entry for the full design rationale.
type State struct {
	App      core.App
	Store    *events.Store
	Registry *decider.Registry
	Engine   *consumers.Engine
	HTTPFns  *functions.HTTPRegistry
	JSProjs  []*functions.JSProjection

	// GoProjections lists the platform's built-in (Go, non-JS) projections
	// — set once at boot by whoever constructs State (the stock CLI's
	// --tutorial flag decides its own; a Go embedder supplies its own
	// list). Unlike JSProjs, this never changes over the process lifetime:
	// Go projections cannot hot-reload. BuildCatalog reads it directly.
	GoProjections []projections.Projection

	// FnRuntime is swapped wholesale on every hot reload (see
	// ReloadFunctions) — never mutate the runtime in place, replace this
	// field instead, and always under ReloadMu.
	FnRuntime *functions.GojaRuntime

	// Verifier backs remote token verification against the master on a
	// --cqrsRole=secondary node (F-13). Nil on a single-node deployment or
	// a master, in which case every gate here behaves as a plain
	// superuser/capability check against this node's own auth tables.
	Verifier *authverify.Verifier

	// Outbound backs the $http binding available to function code, or is
	// nil when outbound HTTP is not enabled. Carried across a hot reload
	// (see carryCapabilities) so it does not silently vanish on the first
	// reload after boot.
	Outbound *outbound.Client

	// JSDeciders tracks JS-managed aggregates (vs. built-in Go deciders)
	// with their active specs, for hot-reload swaps and upcaster rebuilds.
	JSDeciders map[string]*functions.DeciderSpec
	// JSReactors tracks the active JS reactors so a reload can unregister
	// exactly what it registered (their checkpoint keys are stable, so a
	// swapped reactor resumes where the old code left off).
	JSReactors []*functions.ReactorSpec
	// CronJobs lists the registered cron job ids ("fn:"+name).
	CronJobs []string

	// reloadMu serializes hot reloads. Unexported deliberately: only this
	// package's own reload/functions-admin logic ever locks it — an
	// embedder registers routes and lets them run, it does not reach in
	// and lock this itself.
	reloadMu sync.Mutex

	// SchemaDefaultRule mirrors --cqrsSchemaDefaultRule (Item 9): the
	// deployment-wide default read-rule ReconcileSchemas applies to a
	// newly created //@schema collection, both at boot and on every
	// maintenance-mode reload.
	SchemaDefaultRule string

	// Tutorial mirrors --tutorial. allProjections (via buildCatalog) reads
	// it to decide whether the example projections are included in the
	// platform catalog — the only admin-surface behavior the stock CLI's
	// tutorial flag affects, which is why it lives here rather than in the
	// stock binary's own CLI-only config.
	Tutorial bool
}
