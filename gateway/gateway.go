// Package gateway exposes the write side over HTTP: commands in, events out.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"pocketcqrs/decider"
	"pocketcqrs/events"
)

// Config controls the gateway behavior.
type Config struct {
	// AllowAnonymous permits command execution without an auth token
	// (dev only; commands then carry no actor metadata).
	AllowAnonymous bool
	// Mode reports the current system mode; domain commands are rejected
	// with 503 while it returns events.ModeMaintenance (schema-bearing
	// function files reload behind that barrier). Nil disables the check.
	Mode func(ctx context.Context) (string, error)
}

// RegisterRoutes binds the command endpoint:
//
//	POST /api/cqrs/{aggregate}/{id}/{command}
//	body: the command payload as JSON (may be empty)
//
// Unless AllowAnonymous is set, a valid PocketBase record or superuser auth
// token is required; the authenticated record is stamped into every
// resulting event's metadata as {"actor": ..., "actorCollection": ...}.
//
// While the system is in maintenance mode (see Config.Mode), commands are
// rejected with 503: the write side is paused so schema-bearing functions
// can be reloaded safely.
//
// Returns 200 with the appended events, 400 for domain/validation errors,
// 401 without a token, 404 for unknown aggregates, 409 for concurrency
// conflicts, 503 in maintenance mode.
func RegisterRoutes(e *core.ServeEvent, registry *decider.Registry, cfg Config) {
	route := e.Router.POST("/api/cqrs/{aggregate}/{id}/{command}", func(re *core.RequestEvent) error {
		if cfg.Mode != nil {
			mode, err := cfg.Mode(re.Request.Context())
			if err != nil {
				// fail closed: the barrier's state must be known
				return re.JSON(http.StatusInternalServerError,
					map[string]string{"error": "failed reading system mode: " + err.Error()})
			}
			if mode == events.ModeMaintenance {
				return re.JSON(http.StatusServiceUnavailable, map[string]string{
					"error": "system is in maintenance mode: domain commands are temporarily rejected",
					"hint":  "retry after maintenance ends (pocketcqrs system maintenance off)",
				})
			}
		}

		aggregate := re.Request.PathValue("aggregate")
		id := re.Request.PathValue("id")
		cmdName := re.Request.PathValue("command")

		if !registry.Has(aggregate) {
			return apis.NewNotFoundError("unknown aggregate: "+aggregate, nil)
		}

		payload, err := io.ReadAll(re.Request.Body)
		if err != nil {
			return apis.NewBadRequestError("failed reading request body", err)
		}
		if len(payload) == 0 {
			payload = []byte(`{}`)
		}

		appended, err := registry.HandleWithMeta(re.Request.Context(), aggregate, id,
			decider.Command{Name: cmdName, Payload: json.RawMessage(payload)},
			actorMeta(re))
		if err != nil {
			switch {
			case errors.Is(err, decider.ErrUnknownAggregate):
				return apis.NewNotFoundError(err.Error(), err)
			case errors.Is(err, events.ErrConcurrency):
				return re.JSON(http.StatusConflict, map[string]string{"error": err.Error()})
			default:
				return apis.NewBadRequestError(err.Error(), err)
			}
		}

		return re.JSON(http.StatusOK, map[string]any{"events": appended})
	})

	if !cfg.AllowAnonymous {
		route.Bind(apis.RequireAuth())
	}
}

// actorMeta extracts the command-issuer identity from the request, if any.
func actorMeta(re *core.RequestEvent) map[string]any {
	if re.Auth == nil {
		return nil
	}
	return map[string]any{
		"actor":           re.Auth.Id,
		"actorCollection": re.Auth.Collection().Name,
	}
}
