// Package gateway exposes the write side over HTTP: commands in, events out.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
	"github.com/pocketbase/pocketbase/tools/router"

	"github.com/jamestryand/pocketcqrs/batching"
	"github.com/jamestryand/pocketcqrs/cmdauth"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/idempotency"
	"github.com/jamestryand/pocketcqrs/kms"
	"github.com/jamestryand/pocketcqrs/pii"
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
	// reach the master. A plain net/http.Handler, deliberately:
	// any transport that can turn an *http.Request into a response written
	// to an http.ResponseWriter satisfies it — httputil.NewSingleHostReverseProxy
	// for the built-in HTTP default, or (in pocketcqrs-extensions) a NATS
	// request-reply adapter doing its own protocol translation behind the
	// same interface. Nil (the default) means this node decides commands
	// itself, exactly as before this field existed.
	//
	// When set, RegisterRoutes does not bind apis.RequireAuth regardless of
	// AllowAnonymous: this node's own auth records are not authoritative
	// (a secondary's _users/_superusers tables are independent,
	// divergent tables, not a replica of the master's), so checking a token
	// against them here could wrongly reject a valid one. The forwarded
	// request carries the caller's original Authorization header; the
	// destination's own gateway is what actually authenticates it.
	Forward http.Handler
	// Metrics, when set, counts and times every command this node decides,
	// by the HTTP status it answered (health/telemetry contract section 7;
	// opsport.Metrics satisfies it). It wraps the auth check too, so a 401
	// counts as rejected. Not bound when Forward is set: a forwarded command
	// is counted by the master that decides it. Nil records nothing.
	Metrics CommandRecorder
	// Batching, when set, routes commands through the durable intake queue
	// + batching event writer instead of deciding inline: the
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
	// requiredOwnership/scope declarations, command authorization,
	// ported from dotnetcqrs's own MapCqrsGateway `authorize` hook). Checked
	// BEFORE the idempotency lookup, deliberately: an unauthorized attempt
	// has no side effect (same reasoning a domain rejection already gets —
	// see the Idempotency-Key handling below), so nothing about it should
	// ever be cached as a replayable response, and a later retry by an
	// actor whose role has since changed must be re-evaluated, not served
	// a stale verdict.
	//
	// Returning (false, nil) rejects the request with 403, as does a
	// *cmdauth.Refusal error, answered with its own message; any other
	// non-nil error is 503 with Retry-After, since nothing was decided
	// (this package has no logger to report it through, so the error
	// becomes the problem's detail, as Config.Mode's own error path does). Nil (the default) means every
	// authenticated caller may issue every command — today's unchanged
	// behavior for a deployment that wires nothing here, or for any command
	// a wired Authorizer's own table has no policy for (except the built-in
	// dataSubject aggregate's, which the gateway refuses with no hook and the
	// Authorizer refuses unless dataSubjects.erasure admits them). Not consulted at
	// all when cfg.Forward is set (the destination decides; see the top of
	// RegisterRoutes's handler).
	Authorize func(re *core.RequestEvent, aggregate, command, id string, payload []byte) (bool, error)
}

// ErasureRefused is the answer to a dataSubject command when nothing has
// decided who may erase whom: with no Authorize hook here, and from the stock
// server's own hook (authorize.Stock), which keeps the same refusal.
const ErasureRefused = "erasure commands are refused until the host wires an Authorize policy for them; nothing was recorded"

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
// The answers are the command gateway contract v0.1 (gateway/contract_test.go
// is its conformance probe): 200 with {"events":[...]} ([] for none), 400
// for a domain rejection (an error returned by the decider's Decide -- see
// decider.IsRejection) or a malformed command, 401 without a valid token,
// 403 for a caller who may not send it, 404 for unknown aggregates, 409 for
// concurrency conflicts, 410 for an erased subject, 422 for a reused
// Idempotency-Key, 503 with Retry-After whenever nothing was applied for
// want of a dependency before the decision (maintenance mode, the system
// mode, the Authorize hook or the idempotency record unreadable, the stream
// failing to load, the event store locked, the key service down, a full
// queue, a read-only node), 504 if Config.Batching is set and the command's
// batch does not commit within BatchTimeout, and 500 for any other failure
// (fold, append, a JS decide that timed out) -- never 400, which a client
// reads as "the domain said no". Every answer but a 200 is an RFC 9457
// problem body (application/problem+json: status, title, detail), the
// decider's message verbatim in detail, and the two generated refusals
// carrying reason "does-not-exist" / "already-exists"
// (decider.RefusalReason).
//
// If Config.Batching is set, a command is enqueued and the handler blocks
// until its own batch commits instead of deciding inline — every
// status above still applies identically, since the batching path produces
// the same (events, error) shape the direct path always has.
//
// If Config.Idempotency is set and a request carries an Idempotency-Key
// header, a retry with the same key and request replays the original 200
// verbatim rather than re-deciding the command (nothing else is recorded:
// a 409 is decided again); the same key with a different
// aggregate/id/command/body returns 422, and a key over
// idempotency.MaxKeyLength characters 400.
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
				return problem(re, http.StatusBadRequest, "failed reading request body: "+err.Error(), nil)
			}
			re.Request.Body = io.NopCloser(bytes.NewReader(body))
			re.Request.ContentLength = int64(len(body))
			cfg.Forward.ServeHTTP(re.Response, re.Request)
			return nil
		}
		if cfg.Mode != nil {
			mode, err := cfg.Mode(re.Request.Context())
			if err != nil {
				// fail closed: the barrier's state must be known. Nothing
				// was decided, so it is 503, not 500 (G3)
				return unavailable(re, "failed reading system mode: "+err.Error(),
					map[string]any{"hint": "nothing was applied; safe to retry shortly"})
			}
			if mode == events.ModeMaintenance {
				return unavailable(re, "system is in maintenance mode: domain commands are temporarily rejected",
					map[string]any{"hint": "retry after maintenance ends (pocketcqrs system maintenance off)"})
			}
		}

		aggregate := re.Request.PathValue("aggregate")
		id := re.Request.PathValue("id")
		cmdName := re.Request.PathValue("command")

		if !registry.Has(aggregate) {
			return problem(re, http.StatusNotFound, "unknown aggregate: "+aggregate, nil)
		}

		payload, err := io.ReadAll(re.Request.Body)
		if err != nil {
			return problem(re, http.StatusBadRequest, "failed reading request body: "+err.Error(), nil)
		}
		if len(payload) == 0 {
			payload = []byte(`{}`)
		}
		// Personal data arrives as plaintext and the platform encrypts it.
		// An external caller sending an envelope would be choosing the
		// ciphertext, and the subject, that get stored: refused. Reactors
		// and other in-process dispatch, which legitimately carry stored
		// envelopes along, never come through here.
		if hasPIIEnvelope(payload) {
			return problem(re, http.StatusBadRequest, `command payload contains a "$pii" object; send personal data as plaintext, the platform encrypts it`, nil)
		}
		if n := utf8.RuneCountInString(re.Request.Header.Get("Idempotency-Key")); n > idempotency.MaxKeyLength {
			return problem(re, http.StatusBadRequest,
				fmt.Sprintf("Idempotency-Key is %d characters; at most %d are accepted", n, idempotency.MaxKeyLength), nil)
		}

		// Erasure destroys a key and can't be undone, and with no Authorize
		// hook every command is allowed, so the built-in dataSubject
		// aggregate fails closed: none of its commands is accepted until the
		// host wires a policy deciding who may erase whom (E1, as
		// dotnetcqrs's gateway since 0.19.0). Nothing is recorded.
		if cfg.Authorize == nil && strings.EqualFold(aggregate, pii.SubjectAggregate) {
			return problem(re, http.StatusForbidden, ErasureRefused, nil)
		}

		if cfg.Authorize != nil {
			authorized, err := cfg.Authorize(re, aggregate, cmdName, id, payload)
			var refusal *cmdauth.Refusal
			if errors.As(err, &refusal) {
				return problem(re, http.StatusForbidden, refusal.Error(), nil)
			}
			if err != nil {
				// the check could not be made (a role lookup, say): nothing
				// was decided, so 503, not 500 (G3)
				return unavailable(re, "authorization check failed: "+err.Error(),
					map[string]any{"hint": "nothing was applied; safe to retry shortly"})
			}
			if !authorized {
				return problem(re, http.StatusForbidden, cmdauth.NotAuthorized(cmdName, aggregate), nil)
			}
		}

		// Idempotency-Key handling (G7). Only a 200 is recorded and
		// replayed: every other answer applied nothing, so deciding it again
		// is already safe, and a 409 in particular must be decided again
		// (the retry is a new decision, not the lost race replayed).
		var idemKey, idemHash string
		if cfg.Idempotency != nil {
			idemKey = re.Request.Header.Get("Idempotency-Key")
		}
		if idemKey != "" {
			idemHash = idempotency.Hash(aggregate, id, cmdName, string(payload))
			result, err := cfg.Idempotency.Lookup(re.Request.Context(), idemKey, idemHash)
			switch {
			case errors.Is(err, idempotency.ErrKeyReused):
				return problem(re, http.StatusUnprocessableEntity,
					"Idempotency-Key already used for a different request", nil)
			case err != nil:
				return unavailable(re, "idempotency lookup failed: "+err.Error(),
					map[string]any{"hint": "nothing was applied; safe to retry shortly"})
			case result != nil:
				return re.Blob(result.Status, "application/json", result.Body)
			}
		}

		var appended []events.Event
		if cfg.Batching != nil {
			appended, err = handleViaBatching(re, cfg, aggregate, id, cmdName, payload)
		} else {
			appended, err = registry.HandleWithMeta(re.Request.Context(), aggregate, id,
				decider.Command{Name: cmdName, Payload: json.RawMessage(payload)},
				actorMeta(re, cfg))
		}
		// None of the answers below but the final 200 is recorded for
		// Idempotency-Key replay (G7): each one applied nothing, or (a 500,
		// a 504) may or may not have, and the retry must find out.
		if err != nil {
			if errors.Is(err, errBatchTimeout) {
				return problem(re, http.StatusGatewayTimeout, "batch commit did not complete in time", nil)
			}
			var qf *batching.ErrQueueFull
			if errors.As(err, &qf) {
				// nothing was applied, and future depth is unrelated to this
				// moment's
				return unavailable(re, "the command queue is full", map[string]any{
					"hint":  "nothing was applied; safe to retry shortly (use an Idempotency-Key so a retry cannot double-apply once it does get in)",
					"depth": qf.Depth,
				})
			}
			switch {
			case errors.Is(err, decider.ErrUnknownAggregate):
				return problem(re, http.StatusNotFound, "unknown aggregate: "+aggregate, nil)
			case errors.Is(err, events.ErrConcurrency):
				// lost the race for the stream; nothing applied. A retry,
				// even under the same key, is a new decision
				return problem(re, http.StatusConflict, err.Error(), nil)
			case errors.Is(err, events.ErrReadOnly):
				// this node is a --cqrsRole=secondary with no Forward
				// configured (Forward, when set, is checked before any of
				// this runs — see the top of the handler), so refuse
				// outright rather than accept a command that can never
				// durably apply.
				return unavailable(re, "this node is a read-only replica; commands must go to the master", nil)
			case kms.IsSubjectErased(err):
				// the data subject was erased: permanent, unlike a 409 —
				// a returning person is a new subject with a new id.
				return problem(re, http.StatusGone, err.Error(), nil)
			case kms.IsUnavailable(err):
				// the key service couldn't be used: nothing was applied
				return unavailable(re, "dependency unavailable: the key service is unavailable; retry later",
					map[string]any{"hint": "nothing was applied; safe to retry shortly"})
			case decider.IsForbidden(err):
				// the decider refused WHO sent it (schema 3.15.0: only the
				// owner its own state names).
				return problem(re, http.StatusForbidden, err.Error(), nil)
			case decider.IsRejection(err):
				// the decider's own verdict: the only 400 on this path. Its
				// message goes out verbatim (G4) — never through
				// PocketBase's error helpers, which sentence-case it — and
				// the two generated refusals name their reason (G5).
				var ext map[string]any
				if reason := decider.RefusalReason(err, aggregate); reason != "" {
					ext = map[string]any{"reason": reason}
				}
				return problem(re, http.StatusBadRequest, err.Error(), ext)
			case events.IsUnavailable(err):
				// the event store stayed locked past its busy_timeout:
				// nothing was applied, and a retry may well succeed
				return unavailable(re, "the event store is busy: "+err.Error(),
					map[string]any{"hint": "nothing was applied; safe to retry shortly"})
			case decider.IsLoadFailure(err):
				// the stream could not be loaded (read or upcast) before
				// deciding: nothing was decided, so 503, not 500 (G3)
				return unavailable(re, "dependency unavailable: the stream could not be loaded: "+err.Error(),
					map[string]any{"hint": "nothing was applied; safe to retry shortly"})
			default:
				// anything else failed on the way to or from the decider
				// (folding the stream, the append, a JS decide that timed
				// out): the platform's fault, not the caller's, so it must
				// not look like a domain rejection; it may or may not have
				// applied, so the caller retries with the same key
				return problem(re, http.StatusInternalServerError, "command failed: "+err.Error(), nil)
			}
		}

		if appended == nil {
			appended = []events.Event{} // no events is [], never null (G6)
		}
		raw, err := json.Marshal(map[string]any{"events": appended})
		if err != nil {
			return problem(re, http.StatusInternalServerError, "command applied but its answer could not be encoded: "+err.Error(), nil)
		}
		if idemKey != "" {
			// best-effort: a failed save must not stop the caller getting
			// its answer, it only means a future retry misses the replay
			_ = cfg.Idempotency.Save(re.Request.Context(), idemKey, idemHash, http.StatusOK, raw)
		}
		return re.Blob(http.StatusOK, "application/json", raw)
	})

	if !cfg.AllowAnonymous && cfg.Forward == nil {
		route.Bind(requireAuth())
	}
	if cfg.Metrics != nil && cfg.Forward == nil {
		route.Bind(&hook.Handler[*core.RequestEvent]{
			Id: "cqrsCommandMetrics",
			// before RequireAuth (priority 0), so it times and counts a 401 too
			Priority: -1,
			Func: func(re *core.RequestEvent) error {
				started := time.Now()
				err := re.Next()
				cfg.Metrics.RecordCommand(responseStatus(re, err), time.Since(started))
				return err
			},
		})
	}
}

// ProblemContentType is the media type of every non-2xx answer the command
// gateway makes itself (RFC 9457; command gateway contract v0.1, G4).
const ProblemContentType = "application/problem+json"

// problem writes an RFC 9457 problem body: status, title (the status's own
// phrase, as RFC 9457 asks of a problem with no type of its own), detail
// verbatim, and any extension members (reason, hint, depth). It writes the
// answer itself rather than returning one of PocketBase's errors, whose body
// is another shape and whose message is sentence-cased.
func problem(re *core.RequestEvent, status int, detail string, ext map[string]any) error {
	body := make(map[string]any, len(ext)+3)
	for k, v := range ext {
		body[k] = v
	}
	body["status"] = status
	body["title"] = http.StatusText(status)
	body["detail"] = detail
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	re.Response.Header().Set("Content-Type", ProblemContentType)
	return re.Blob(status, ProblemContentType, raw)
}

// unavailable is a 503 problem: not taken, nothing applied, retry after the
// Retry-After seconds (G3, G8).
func unavailable(re *core.RequestEvent, detail string, ext map[string]any) error {
	re.Response.Header().Set("Retry-After", "1")
	return problem(re, http.StatusServiceUnavailable, detail, ext)
}

// requireAuth is apis.RequireAuth with a problem body: a request with no
// credential, or one PocketBase could not verify (it leaves re.Auth nil), is
// a 401 (G2). It keeps RequireAuth's id and priority, so the metrics hook
// (priority -1) still runs first and counts the 401.
func requireAuth() *hook.Handler[*core.RequestEvent] {
	return &hook.Handler[*core.RequestEvent]{
		Id: apis.DefaultRequireAuthMiddlewareId,
		Func: func(re *core.RequestEvent) error {
			if re.Auth == nil {
				return problem(re, http.StatusUnauthorized, "the request requires a valid authorization token", nil)
			}
			return re.Next()
		},
	}
}

// CommandRecorder receives each decided command's HTTP status and duration,
// from receipt to response.
type CommandRecorder interface {
	RecordCommand(httpStatus int, d time.Duration)
}

// responseStatus is the status a handler answered: the one it wrote, or, when
// it returned an error for PocketBase to render, that error's (500 for an
// error that carries none).
func responseStatus(re *core.RequestEvent, err error) int {
	if err != nil {
		var apiErr *router.ApiError
		if errors.As(err, &apiErr) {
			return apiErr.Status
		}
		return http.StatusInternalServerError
	}
	if status := re.Status(); status != 0 {
		return status
	}
	return http.StatusOK
}

// errBatchTimeout is handleViaBatching's sentinel for "the batch never
// committed within Config.BatchTimeout" — mapped to 504 by RegisterRoutes.
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

// hasPIIEnvelope reports whether any object anywhere in payload has a
// property named "$pii" (compared after JSON unescaping, so "\u0024pii" is
// caught too). A body that doesn't parse isn't judged here; dispatch reports
// it.
func hasPIIEnvelope(payload []byte) bool {
	var v any
	if err := json.Unmarshal(payload, &v); err != nil {
		return false
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch t := v.(type) {
		case map[string]any:
			if _, ok := t["$pii"]; ok {
				return true
			}
			for _, c := range t {
				if walk(c) {
					return true
				}
			}
		case []any:
			for _, c := range t {
				if walk(c) {
					return true
				}
			}
		}
		return false
	}
	return walk(v)
}
