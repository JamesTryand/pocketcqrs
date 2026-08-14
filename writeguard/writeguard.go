// Package writeguard rejects out-of-band record writes on guarded
// (projection-owned) collections. There is no user-facing escape hatch:
// the only writer is the projection engine, whose server-side calls carry
// an internal context marker that never crosses the HTTP/API boundary.
package writeguard

import (
	"context"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

// AuthOrigins is PocketBase's own "_authOrigins" collection, safe to add to
// Register's collections alongside the projection-owned ones (F-6).
// PocketBase writes it directly during login (`authAlert`, tracking device
// fingerprints for the "new device" alert) — outside the event log, on any
// node that serves the login request. That write's own error is already
// caught and only logged as a warning by PocketBase itself
// (apis.recordAuthResponse: "Failed to send login alert"), never surfaced to
// the caller, so guarding it costs nothing: login keeps working exactly as
// before, and what was a silent, unguarded write becomes a loud, denied one
// like every other out-of-band write this package stops.
//
// This does NOT extend to PocketBase's other auth-adjacent collections
// (_users, _superusers, _otps, _mfas): guarding those would block PocketBase's
// own stock signup/password-change/OTP/MFA endpoints outright, since those
// writes ARE the point of the request, not a best-effort side effect. Whether
// user identity should be a PocketBase auth record or a modelled aggregate is
// an open, project-specific decision (see NEEDS.md) — not something core can
// decide generically by extending this list.
const AuthOrigins = "_authOrigins"

type markerKey struct{}

// MarkInternal returns a context flagged as an internal (projection) write.
func MarkInternal(ctx context.Context) context.Context {
	return context.WithValue(ctx, markerKey{}, true)
}

// IsInternal reports whether ctx carries the internal-write marker.
func IsInternal(ctx context.Context) bool {
	v, _ := ctx.Value(markerKey{}).(bool)
	return v
}

// Register denies create/update/delete on the named collections for every
// caller except internal writers (see MarkInternal). Superusers are denied
// too: state changes must flow through the command API so they become events.
//
// Registering NO collections guards nothing. PocketBase treats a tagged hook
// with an empty tag list as "match every collection"
// (hook.TaggedHook.CanTriggerOn), so binding here without names would deny
// every record write in the app — superuser creation included — rather than
// the nothing the caller asked for. Callers legitimately pass an empty list
// (an instance with no projections yet), so this is a guard, not a bug in
// them.
func Register(app core.App, collections ...string) {
	if len(collections) == 0 {
		return
	}

	deny := func(e *core.RecordEvent) error {
		if IsInternal(e.Context) {
			return e.Next()
		}
		return apis.NewForbiddenError(
			"Direct writes to '"+e.Record.Collection().Name+"' are disabled; "+
				"state changes must go through the command API (POST /api/cqrs/{aggregate}/{id}/{command}).",
			nil,
		)
	}

	app.OnRecordCreate(collections...).BindFunc(deny)
	app.OnRecordUpdate(collections...).BindFunc(deny)
	app.OnRecordDelete(collections...).BindFunc(deny)
}
