package authverify

import (
	"errors"
	"net/http"
	"slices"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
)

// RequireSuperuser is the ops routes' auth gate. With no verifier (the
// master, or a secondary not running verify mode) it is exactly
// apis.RequireSuperuserAuth(). With one, it re-verifies the bearer token
// against the master on EVERY request — VerifyFresh, no cache read, no
// grace — because this is the one surface where a stale "yes" is not an
// availability tradeoff but a hole: an operator revoking a
// suspected-compromised admin needs that to bite immediately, not after a
// TTL window (the design analysis's reason to keep ops on shape C).
//
// The fresh verdict also OVERWRITES whatever re.Auth already holds — a
// cached verdict from the global middleware, or a token PocketBase's own
// loadAuthToken verified against this node's local, unrelated _superusers
// table. On a secondary running verify mode, only the master's current
// answer admits anyone here.
func RequireSuperuser(v *Verifier) *hook.Handler[*core.RequestEvent] {
	if v == nil {
		return apis.RequireSuperuserAuth()
	}
	return &hook.Handler[*core.RequestEvent]{
		Id: "pcRequireSuperuserRemote",
		Func: func(re *core.RequestEvent) error {
			token := tokenFromRequest(re)
			if token == "" {
				return re.UnauthorizedError("The request requires valid superuser authorization token.", nil)
			}
			verdict, err := v.VerifyFresh(re.Request.Context(), token)
			switch {
			case errors.Is(err, ErrInvalidToken):
				return re.UnauthorizedError("The request requires valid superuser authorization token.", nil)
			case err != nil:
				return apis.NewApiError(http.StatusServiceUnavailable,
					"Superuser verification unavailable: cannot reach the master.", err)
			}
			if verdict.CollectionName != core.CollectionNameSuperusers {
				return re.ForbiddenError("The authorized record is not allowed to perform this action.", nil)
			}
			rec, err := Materialize(re.App, verdict)
			if err != nil {
				return re.UnauthorizedError("The request requires valid superuser authorization token.", err)
			}
			if err := checkSuperuserIP(re, rec); err != nil {
				return err
			}
			re.Auth = rec
			return re.Next()
		},
	}
}

// capabilitiesField mirrors roles.CapabilitiesField. Not imported directly:
// authverify is generic verification infrastructure and deliberately knows
// nothing about the roles package or any other specific auth collection —
// RequireCapability accepts a grant from ANY authenticated record carrying
// this field, superuser or not, which is what makes it Item 11's general
// model rather than a fixed poweruser check. (A future role/permission
// field on a "users" collection, per the decision doc's Option 3, composes
// here with zero code change.)
const capabilitiesField = "capabilities"

// hasCapability reports whether rec is a superuser (which always passes,
// same as every other ops gate) or carries capability in its own
// capabilitiesField.
func hasCapability(rec *core.Record, capability string) bool {
	return rec.IsSuperuser() || slices.Contains(rec.GetStringSlice(capabilitiesField), capability)
}

// RequireCapability is Item 11's general-purpose ops gate: it passes a
// genuine PocketBase superuser exactly as RequireSuperuser does, and
// ADDITIONALLY passes any other authenticated record whose own
// "capabilities" JSON field contains capability — regardless of which auth
// collection that record belongs to. Everything else about the shape
// mirrors RequireSuperuser deliberately, including on the multi-node path:
// v == nil (the common single-node case) delegates to the request's
// already-loaded re.Auth (PocketBase's own loadAuthToken middleware
// populates it for ANY auth collection, not just superusers); with a
// Verifier it re-verifies FRESH on every request, no cache, no grace — a
// revoked capability grant (token revoked, OR the capabilities field
// edited to remove it) must bite immediately here, same reasoning as the
// superuser gate.
func RequireCapability(v *Verifier, capability string) *hook.Handler[*core.RequestEvent] {
	if v == nil {
		return &hook.Handler[*core.RequestEvent]{
			Id: "pcRequireCapability:" + capability,
			Func: func(re *core.RequestEvent) error {
				if re.Auth == nil {
					return re.UnauthorizedError("The request requires a valid authorization token.", nil)
				}
				if !hasCapability(re.Auth, capability) {
					return re.ForbiddenError("The authorized record is not allowed to perform this action.", nil)
				}
				return re.Next()
			},
		}
	}
	return &hook.Handler[*core.RequestEvent]{
		Id: "pcRequireCapabilityRemote:" + capability,
		Func: func(re *core.RequestEvent) error {
			token := tokenFromRequest(re)
			if token == "" {
				return re.UnauthorizedError("The request requires a valid authorization token.", nil)
			}
			verdict, err := v.VerifyFresh(re.Request.Context(), token)
			switch {
			case errors.Is(err, ErrInvalidToken):
				return re.UnauthorizedError("The request requires a valid authorization token.", nil)
			case err != nil:
				return apis.NewApiError(http.StatusServiceUnavailable,
					"Capability verification unavailable: cannot reach the master.", err)
			}
			rec, err := Materialize(re.App, verdict)
			if err != nil {
				return re.UnauthorizedError("The request requires a valid authorization token.", err)
			}
			// checkSuperuserIP no-ops for a non-superuser record; safe
			// unconditionally, same as RequireSuperuser's own call.
			if err := checkSuperuserIP(re, rec); err != nil {
				return err
			}
			if !hasCapability(rec, capability) {
				return re.ForbiddenError("The authorized record is not allowed to perform this action.", nil)
			}
			re.Auth = rec
			return re.Next()
		},
	}
}
