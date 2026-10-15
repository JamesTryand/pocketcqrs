package adminapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/authverify"
)

// ErrUnknownProjection refuses a rebuild request naming no projection this
// node runs.
var ErrUnknownProjection = errors.New("adminapi: no such projection")

// RequestRebuild records a rebuild of the named projection for the running
// engine to carry out between its passes (consumers.RebuildRequests): wipe
// its collections, reset its checkpoint, replay the log. Nothing else
// happens here, whether or not a server is running: the engine stays the
// only thing that runs a projection. Requesting it again before the engine
// acts is harmless. Only a projection can be rebuilt, never a reactor or an
// effect function, which would act on the world again.
func (s *State) RequestRebuild(ctx context.Context, name string) error {
	if s.Rebuilds == nil {
		return errors.New("adminapi: rebuild requests are not available on this node")
	}
	if !s.hasProjection(name) {
		return fmt.Errorf("%w %q", ErrUnknownProjection, name)
	}
	return s.Rebuilds.Request(ctx, name)
}

// hasProjection reports whether name is one of this node's Go or JS
// projections.
func (s *State) hasProjection(name string) bool {
	for _, p := range s.GoProjections {
		if p.Name() == name {
			return true
		}
	}
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	for _, p := range s.JSProjs {
		if p.Name() == name {
			return true
		}
	}
	return false
}

// RegisterRebuildRoute binds
//
//	POST /api/cqrs/admin/projections/{name}/rebuild
//
// (superuser): records a rebuild request and answers 202 Accepted. The
// running engine carries it out; the projection's read model is empty or
// partial until its replay catches up.
func RegisterRebuildRoute(e *core.ServeEvent, s *State) {
	e.Router.POST("/api/cqrs/admin/projections/{name}/rebuild", func(re *core.RequestEvent) error {
		name := re.Request.PathValue("name")
		if err := s.RequestRebuild(re.Request.Context(), name); err != nil {
			if errors.Is(err, ErrUnknownProjection) {
				return apis.NewNotFoundError(err.Error(), err)
			}
			return apis.NewBadRequestError(err.Error(), err)
		}
		return re.JSON(http.StatusAccepted, map[string]string{
			"requested": name,
			"note":      "the engine wipes and replays it on its next pass; its read model is partial until then",
		})
	}).Bind(authverify.RequireSuperuser(s.Verifier))
}
