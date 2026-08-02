// Package gateway exposes the write side over HTTP: commands in, events out.
package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"pocketcqrs/decider"
	"pocketcqrs/events"
)

// RegisterRoutes binds the command endpoint:
//
//	POST /api/cqrs/{aggregate}/{id}/{command}
//	body: the command payload as JSON (may be empty)
//
// Returns 200 with the appended events, 400 for domain/validation errors,
// 404 for unknown aggregates, 409 for concurrency conflicts.
//
// Note: v1 is unauthenticated; authn/authz on commands is a later gate.
func RegisterRoutes(e *core.ServeEvent, registry *decider.Registry) {
	e.Router.POST("/api/cqrs/{aggregate}/{id}/{command}", func(re *core.RequestEvent) error {
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

		appended, err := registry.Handle(re.Request.Context(), aggregate, id,
			decider.Command{Name: cmdName, Payload: json.RawMessage(payload)})
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
}
