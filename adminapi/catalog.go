package adminapi

import (
	"context"
	"net/http"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/authverify"
	"github.com/jamestryand/pocketcqrs/catalog"
)

// BuildCatalog collects the catalog from the live state (shared by the
// stock binary's own `catalog` CLI command and this package's JSON
// endpoint).
//
// GoProjections is read directly from State rather than derived here: which
// Go projections are live is the constructing binary's own decision (the
// stock CLI's --tutorial flag included), made once at boot, not something
// this package should reach back into caller-specific logic to compute.
func (s *State) BuildCatalog(ctx context.Context) (*catalog.Catalog, error) {
	return catalog.Build(ctx, catalog.Inputs{
		App:           s.App,
		Store:         s.Store,
		Registry:      s.Registry,
		Engine:        s.Engine,
		Runtime:       s.FnRuntime,
		HTTP:          s.HTTPFns,
		GoProjections: s.GoProjections,
		JSProjs:       s.JSProjs,
		JSDeciders:    s.JSDeciders,
		JSReactors:    s.JSReactors,
	})
}

// RegisterCatalogRoute binds the catalog endpoint:
//
//	GET /api/cqrs/catalog
//
// Item 11: gated by authverify.RequireCapability (capOpsCatalogRead), not a
// bare superuser check — a "roles" record with that capability can reach
// it, and it is remote-verify-aware on a --cqrsVerifyAuth secondary. For an
// ordinary single-node superuser (s.Verifier == nil, the common case),
// RequireCapability(nil, ...) behaves identically to a plain superuser
// check — proven by authverify's own TestRequireCapabilityLocal.
func RegisterCatalogRoute(e *core.ServeEvent, s *State) {
	e.Router.GET("/api/cqrs/catalog", func(re *core.RequestEvent) error {
		cat, err := s.BuildCatalog(re.Request.Context())
		if err != nil {
			return apis.NewBadRequestError(err.Error(), err)
		}
		return re.JSON(http.StatusOK, cat)
	}).Bind(authverify.RequireCapability(s.Verifier, capOpsCatalogRead))
}
