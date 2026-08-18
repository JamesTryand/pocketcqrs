package authverify

import (
	"errors"
	"net/http"

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
