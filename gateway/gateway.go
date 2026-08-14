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

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/idempotency"
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
	// Idempotency, when set, lets a caller retry a command safely: send an
	// Idempotency-Key header, and a retry with the same key and the same
	// aggregate/id/command/body replays the original response instead of
	// re-deciding the command. The same key with a different request is
	// rejected with 422 rather than silently replayed or silently applied
	// twice. Requests without the header are unaffected either way. Nil
	// disables the feature entirely (today's behavior).
	Idempotency *idempotency.Store
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
//
// If Config.Idempotency is set and a request carries an Idempotency-Key
// header, a retry with the same key and request replays the original 200
// or 409 response verbatim rather than re-deciding the command; the same
// key with a different aggregate/id/command/body returns 422.
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

		// Idempotency-Key handling. Scoped to the two outcomes a retried
		// command can actually double-apply or needs to see identically
		// (success, and the concurrency conflict a redelivered success
		// produces on retry) — a domain rejection has no side effect, so
		// simply re-deciding it is already idempotent and is left alone.
		var idemKey, idemHash string
		if cfg.Idempotency != nil {
			idemKey = re.Request.Header.Get("Idempotency-Key")
		}
		if idemKey != "" {
			idemHash = idempotency.Hash(aggregate, id, cmdName, string(payload))
			result, err := cfg.Idempotency.Lookup(re.Request.Context(), idemKey, idemHash)
			switch {
			case errors.Is(err, idempotency.ErrKeyReused):
				return re.JSON(http.StatusUnprocessableEntity, map[string]string{
					"error": "Idempotency-Key already used for a different request",
				})
			case err != nil:
				return re.JSON(http.StatusInternalServerError,
					map[string]string{"error": "idempotency lookup failed: " + err.Error()})
			case result != nil:
				return re.Blob(result.Status, "application/json", result.Body)
			}
		}
		respondJSON := func(status int, data any) error {
			if idemKey == "" {
				return re.JSON(status, data)
			}
			raw, err := json.Marshal(data)
			if err != nil {
				return err
			}
			// best-effort: a failed save must not stop the caller getting
			// its answer, it only means a future retry misses the replay
			_ = cfg.Idempotency.Save(re.Request.Context(), idemKey, idemHash, status, raw)
			return re.Blob(status, "application/json", raw)
		}

		appended, err := registry.HandleWithMeta(re.Request.Context(), aggregate, id,
			decider.Command{Name: cmdName, Payload: json.RawMessage(payload)},
			actorMeta(re))
		if err != nil {
			switch {
			case errors.Is(err, decider.ErrUnknownAggregate):
				return apis.NewNotFoundError(err.Error(), err)
			case errors.Is(err, events.ErrConcurrency):
				return respondJSON(http.StatusConflict, map[string]string{"error": err.Error()})
			case errors.Is(err, events.ErrReadOnly):
				// this node is a --cqrsRole=secondary; nothing forwards
				// commands to the master yet (item 3, unbuilt), so refuse
				// outright rather than accept one that can never durably
				// apply. Not cached for idempotency replay: no side effect
				// happened, and which node answers is not a stable outcome.
				return re.JSON(http.StatusServiceUnavailable, map[string]string{
					"error": "this node is a read-only replica; commands must go to the master",
				})
			default:
				return apis.NewBadRequestError(err.Error(), err)
			}
		}

		return respondJSON(http.StatusOK, map[string]any{"events": appended})
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
