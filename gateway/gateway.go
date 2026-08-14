// Package gateway exposes the write side over HTTP: commands in, events out.
package gateway

import (
	"bytes"
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
	// Forward, when set, proxies every command request to it instead of
	// deciding locally — the shape a --cqrsRole=secondary node uses to
	// reach the master (item 3). A plain net/http.Handler, deliberately:
	// any transport that can turn an *http.Request into a response written
	// to an http.ResponseWriter satisfies it — httputil.NewSingleHostReverseProxy
	// for the built-in HTTP default, or (in pocketcqrs-extensions) a NATS
	// request-reply adapter doing its own protocol translation behind the
	// same interface. Nil (the default) means this node decides commands
	// itself, exactly as before this field existed.
	//
	// When set, RegisterRoutes does not bind apis.RequireAuth regardless of
	// AllowAnonymous: this node's own auth records are not authoritative
	// (see F-12 — a secondary's _users/_superusers tables are independent,
	// divergent tables, not a replica of the master's), so checking a token
	// against them here could wrongly reject a valid one. The forwarded
	// request carries the caller's original Authorization header; the
	// destination's own gateway is what actually authenticates it.
	Forward http.Handler
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
//
// If Config.Forward is set, every command request is proxied to it
// immediately — no local Mode check, no local auth check, no local decide —
// and everything above this paragraph describes the destination's behavior,
// not this node's.
func RegisterRoutes(e *core.ServeEvent, registry *decider.Registry, cfg Config) {
	route := e.Router.POST("/api/cqrs/{aggregate}/{id}/{command}", func(re *core.RequestEvent) error {
		if cfg.Forward != nil {
			// PocketBase wraps the request body in a RereadableReadCloser
			// that silently rewinds on EOF instead of staying exhausted
			// (tools/router/rereadable_read_closer.go) — handed straight to
			// httputil.ReverseProxy, that rewind reads back through the
			// already-sent bytes a second time, and the outbound request
			// ends up with more body than its own Content-Length promised
			// ("ContentLength=2 with Body length 4" for a 2-byte "{}").
			// Replacing it with a plain, single-pass reader avoids the
			// interaction entirely.
			body, err := io.ReadAll(re.Request.Body)
			if err != nil {
				return apis.NewBadRequestError("failed reading request body", err)
			}
			re.Request.Body = io.NopCloser(bytes.NewReader(body))
			re.Request.ContentLength = int64(len(body))
			cfg.Forward.ServeHTTP(re.Response, re.Request)
			return nil
		}
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
				// this node is a --cqrsRole=secondary with no Forward
				// configured (Forward, when set, is checked before any of
				// this runs — see the top of the handler), so refuse
				// outright rather than accept a command that can never
				// durably apply. Not cached for idempotency replay: no side
				// effect happened, and which node answers is not stable.
				return re.JSON(http.StatusServiceUnavailable, map[string]string{
					"error": "this node is a read-only replica; commands must go to the master",
				})
			default:
				return apis.NewBadRequestError(err.Error(), err)
			}
		}

		return respondJSON(http.StatusOK, map[string]any{"events": appended})
	})

	if !cfg.AllowAnonymous && cfg.Forward == nil {
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
