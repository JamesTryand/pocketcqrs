package adminapi

import (
	"github.com/pocketbase/pocketbase/core"
)

// Config bundles the deployment-specific knobs the admin surface needs
// beyond State itself, mirroring gateway.Config's shape.
type Config struct {
	// FunctionsDir is the directory holding the user's JS function files —
	// read by the reload and function-file-admin routes.
	FunctionsDir string
}

// RegisterRoutes binds the whole catalog/ops/admin HTTP surface in one
// call: catalog, events/streams/deadletters/admin-mode (ops), hot reload,
// and function-file management (list/read/write/delete/dryrun/scaffold).
// This is the one-call surface Item 10 exists for — before this package, a
// Go embedder reconstructing its own main.go (docs/go-guide.md's wrapper
// pattern) had no way to reach any of these routes at all, since they lived
// as unexported, package-private functions in pocketcqrs's own module-root
// package main.
//
// Matches gateway.RegisterRoutes's shape: one call, given the live state
// and this deployment's config, registers everything. An embedder that
// wants only a subset can call RegisterCatalogRoute / RegisterOpsRoutes /
// RegisterReloadRoute / RegisterFunctionAdminRoutes individually instead.
func RegisterRoutes(e *core.ServeEvent, s *State, cfg Config) {
	RegisterCatalogRoute(e, s)
	RegisterOpsRoutes(e, s)
	RegisterReloadRoute(e, s, cfg.FunctionsDir)
	RegisterFunctionAdminRoutes(e, s, cfg.FunctionsDir)
}
