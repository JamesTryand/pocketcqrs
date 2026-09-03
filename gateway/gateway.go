// Package gateway exposes the write side over HTTP: commands in, events out.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/batching"
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
	// Batching, when set, routes commands through the durable intake queue
	// + batching event writer (item 4) instead of deciding inline: the
	// handler enqueues the command then blocks until its own command's
	// batch commits, returning the same synchronous (events, error) result
	// HandleWithMeta always has. No correlation id, no polling, no
	// client-visible contract change — just variable added latency under
	// load, bounded by BatchTimeout. Nil (the default) is today's direct
	// registry.HandleWithMeta call, unchanged.
	Batching *batching.Writer
	// BatchTimeout bounds how long a request waits for its batch to
	// commit when Batching is set. Required (non-zero) whenever Batching
	// is set; has no effect otherwise.
	BatchTimeout time.Duration
	// ExternalCallerCollection, when set, names a PocketBase auth
	// collection whose authenticated records are external service
	// integrations (e.g. pocketcqrs-extensions' extcaller) rather than end
	// users or reactors. A record in this collection gets its commands'
	// actor stamped as "extcall:<name>" (its own "name" field) instead of
	// its raw record id, and -- only for this collection, never for an
	// arbitrary authenticated caller -- may additionally supply
	// Causation-Id/Correlation-Id request headers, merged into every
	// resulting event's metadata the same way a local reactor's dispatch
	// already carries them (see reactors.Dispatch). Everything here is
	// derived from re.Auth (already verified by RequireAuth) and this
	// config value, never from anything else the request supplies -- a
	// caller cannot claim a service identity, or the elevated header
	// trust that comes with one, that it does not hold the credential
	// for. Empty (the default) disables this entirely: actorMeta falls
	// back to today's behavior. See actorMeta.
	//
	// A record in this collection with an empty "name" field degrades to
	// the raw record id as its actor -- deliberately, not silently: it
	// still authenticates and its command still applies, it just won't
	// get the "extcall:" label or ReactorFlows visibility until "name" is
	// set. Provisioning tooling (pocketcqrs-extensions' planned Phase 6
	// CLI) should always set it; this package has no logger to warn
	// through, so the degradation is a documented default, not silent
	// inertness left unstated.
	ExternalCallerCollection string
	// Authorize, when set, is consulted for every command after Mode and
	// before Idempotency-Key lookup — see the authorize package's own
	// Authorizer for the shape a real deployment supplies (a policy table
	// built from an emschema.Document's requiredRole/fieldGatedRole/
	// requiredOwnership/scope declarations, platform/command-authorization,
	// ported from dotnetcqrs's own MapCqrsGateway `authorize` hook). Checked
	// BEFORE the idempotency lookup, deliberately: an unauthorized attempt
	// has no side effect (same reasoning a domain rejection already gets —
	// see the Idempotency-Key handling below), so nothing about it should
	// ever be cached as a replayable response, and a later retry by an
	// actor whose role has since changed must be re-evaluated, not served
	// a stale verdict.
	//
	// Returning (false, nil) rejects the request with 403; a non-nil error
	// rejects with 500 (this package has no logger to report it through, so
	// the error becomes the response body, same posture Config.Mode's own
	// error path already takes). Nil (the default) means every
	// authenticated caller may issue every command — today's unchanged
	// behavior for a deployment that wires nothing here, or for any command
	// a wired Authorizer's own table has no policy for. Not consulted at
	// all when cfg.Forward is set (the destination decides; see the top of
	// RegisterRoutes's handler).
	Authorize func(re *core.RequestEvent, aggregate, command, id string, payload []byte) (bool, error)
}

// RegisterRoutes binds the command endpoint:
//
//	POST /api/cqrs/{aggregate}/{id}/{command}
//	body: the command payload as JSON (may be empty)
//
// Unless AllowAnonymous is set, a valid PocketBase record or superuser auth
// token is required; the authenticated record is stamped into every
// resulting event's metadata as {"actor": ..., "actorCollection": ...} —
// actor is the record's raw id, except for a caller Config.
// ExternalCallerCollection recognizes, which gets "extcall:<name>" instead
// (see actorMeta). That same recognized caller may also supply
// Causation-Id/Correlation-Id request headers, merged into the resulting
// events' metadata the same way a local reactor's own dispatch already
// carries them.
//
// While the system is in maintenance mode (see Config.Mode), commands are
// rejected with 503: the write side is paused so schema-bearing functions
// can be reloaded safely.
//
// Returns 200 with the appended events, 400 for domain/validation errors,
// 401 without a token, 404 for unknown aggregates, 409 for concurrency
// conflicts, 503 in maintenance mode, 504 if Config.Batching is set and the
// command's batch does not commit within BatchTimeout.
//
// If Config.Batching is set, a command is enqueued and the handler blocks
// until its own batch commits (item 4) instead of deciding inline — every
// status above still applies identically, since the batching path produces
// the same (events, error) shape the direct path always has.
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

		if cfg.Authorize != nil {
			authorized, err := cfg.Authorize(re, aggregate, cmdName, id, payload)
			if err != nil {
				return re.JSON(http.StatusInternalServerError,
					map[string]string{"error": "authorization check failed: " + err.Error()})
			}
			if !authorized {
				return re.JSON(http.StatusForbidden, map[string]string{
					"error": "not authorized to issue " + cmdName + " on " + aggregate,
				})
			}
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

		var appended []events.Event
		if cfg.Batching != nil {
			appended, err = handleViaBatching(re, cfg, aggregate, id, cmdName, payload)
		} else {
			appended, err = registry.HandleWithMeta(re.Request.Context(), aggregate, id,
				decider.Command{Name: cmdName, Payload: json.RawMessage(payload)},
				actorMeta(re, cfg))
		}
		if err != nil {
			if errors.Is(err, errBatchTimeout) {
				return re.JSON(http.StatusGatewayTimeout, map[string]string{
					"error": "batch commit did not complete in time",
				})
			}
			var qf *batching.ErrQueueFull
			if errors.As(err, &qf) {
				// Not routed through respondJSON: nothing was applied, and
				// future depth is unrelated to this moment's — caching this
				// as the permanent idempotency-replay answer would wrongly
				// block a later retry that might get in. Same reasoning
				// ErrReadOnly below already uses.
				re.Response.Header().Set("Retry-After", "1")
				return re.JSON(http.StatusServiceUnavailable, map[string]any{
					"error": "the command queue is full",
					"hint":  "nothing was applied; safe to retry shortly (use an Idempotency-Key so a retry cannot double-apply once it does get in)",
					"depth": qf.Depth,
				})
			}
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

// errBatchTimeout is handleViaBatching's sentinel for "the batch never
// committed within Config.BatchTimeout" — mapped to 504 by RegisterRoutes,
// not the generic 400 every other decide error gets.
var errBatchTimeout = errors.New("gateway: batch commit did not complete in time")

// handleViaBatching enqueues a command through cfg.Batching and blocks
// until its own batch commits, returning the same (events, error) shape
// registry.HandleWithMeta always has.
//
// "now" is captured here, once, before enqueueing — never left for a later
// decide attempt (including a post-crash replay) to derive fresh. That is
// what makes the batching writer's determinism guarantee hold in the first
// place (see batching.Writer.Enqueue's doc comment); getting this wrong
// here would silently defeat it regardless of how correct the writer
// itself is.
func handleViaBatching(re *core.RequestEvent, cfg Config, aggregate, id, cmdName string, payload []byte) ([]events.Event, error) {
	meta := actorMeta(re, cfg)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["now"] = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")

	_, wait, err := cfg.Batching.Enqueue(re.Request.Context(), aggregate, id, cmdName, json.RawMessage(payload), meta)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(re.Request.Context(), cfg.BatchTimeout)
	defer cancel()

	select {
	case outcome := <-wait:
		return outcome.Events, outcome.Err
	case <-ctx.Done():
		return nil, errBatchTimeout
	}
}

// actorMeta extracts the command-issuer identity from the request, if any,
// and -- only for a caller cfg.ExternalCallerCollection recognizes -- the
// optional causation/correlation metadata it supplies.
//
// Everything here is derived from re.Auth (already verified by
// RequireAuth) and cfg (this deployment's own boot-time configuration),
// never from anything else the request contains: actor is not
// caller-assertable, and Causation-Id/Correlation-Id are only ever honored
// for a caller whose collection membership already proves it holds a
// recognized service-account credential -- the same pattern actor itself
// already uses. See Config.ExternalCallerCollection's doc comment.
func actorMeta(re *core.RequestEvent, cfg Config) map[string]any {
	if re.Auth == nil {
		return nil
	}

	// PocketBase's default record id (core.GenerateDefaultRandomId) is a
	// 15-character string drawn from "abcdefghijklmnopqrstuvwxyz0123456789"
	// -- never a ":" -- so an ordinary caller's actor (the bare id, below)
	// practically can't collide with the "reactor:"/"extcall:" prefixes
	// ReactorFlows and catalog.go's consumer-Kind switch both match on.
	// Not a hard guarantee against every conceivable custom/imported id,
	// but true for every id this codebase itself ever generates.
	actor := re.Auth.Id
	isExternalCaller := cfg.ExternalCallerCollection != "" &&
		re.Auth.Collection().Name == cfg.ExternalCallerCollection
	if isExternalCaller {
		if name := re.Auth.GetString("name"); name != "" {
			actor = "extcall:" + name
		}
		// name == "" falls through with actor still the raw record id --
		// deliberate degradation, not a silent failure to derive it. See
		// Config.ExternalCallerCollection's doc comment.
	}

	meta := map[string]any{
		"actor":           actor,
		"actorCollection": re.Auth.Collection().Name,
	}

	// Gated to a recognized external caller, not opened to every
	// authenticated caller: these feed ReactorFlows/the catalog explorer's
	// display, which an arbitrary caller fabricating edges would pollute
	// for no legitimate reason. Unlike Idempotency-Key (no trust
	// implication -- only affects which cached response replays), these
	// affect what the flow diagrams show, so they get the same trust gate
	// actor's own derivation does.
	if isExternalCaller {
		if v := re.Request.Header.Get("Causation-Id"); v != "" {
			meta["causationId"] = v
		}
		if v := re.Request.Header.Get("Correlation-Id"); v != "" {
			meta["correlationId"] = v
		}
	}

	return meta
}
