// Package authforward routes PocketBase's own native auth-collection
// traffic to the master on a --cqrsRole=secondary node (F-12, the
// remainder of item 5).
//
// _users, _superusers, _authOrigins, _otps, _mfas and any custom auth-type
// collection are NOT projected from events.db by anything in this
// project -- a secondary's copy of these tables is not a stale replica of
// the master's, it is a different table with different rows, since only
// events.db is replicated (see docs/reference/cli.md's "Multi-node"
// section). Extending writeguard to deny writes on them (as item 5
// originally proposed) does not fix this: a login on a secondary reads
// the wrong local data before any write even happens. The actual fix is
// routing this traffic to the master entirely -- reads and writes alike
// -- which is what this package does.
//
// This is a global router middleware, not a route the way the CQRS
// gateway's Forward (item 3) is: PocketBase registers these routes itself
// (apis.NewRouter), so there is no route for this package to bind auth
// -skipping/forwarding logic onto directly. Bound early via
// core.ServeEvent.Router.BindFunc, it intercepts before PocketBase's own
// handler runs at all -- verified empirically (not just reasoned about)
// that a global middleware bound after apis.NewRouter has already
// registered the default routes still intercepts them, since middleware
// composition happens at BuildMux() time, after every OnServe hook
// (PocketBase's own route registration and this package's binding alike)
// has run.
package authforward

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

// authFlowSuffixes are the fixed, PocketBase-defined route suffixes under
// /api/collections/{collection}/... that only make sense against an
// auth-type collection (verified against apis/record_auth.go's
// bindRecordAuthApi). auth-methods is deliberately excluded: it returns
// schema/config, identical on every node since all nodes run the same
// migrations, so it is safe -- and better -- to serve locally.
var authFlowSuffixes = map[string]bool{
	"auth-refresh":           true,
	"auth-with-password":     true,
	"auth-with-oauth2":       true,
	"request-otp":            true,
	"auth-with-otp":          true,
	"request-password-reset": true,
	"confirm-password-reset": true,
	"request-verification":   true,
	"confirm-verification":   true,
	"request-email-change":   true,
	"confirm-email-change":   true,
}

// Register binds the forwarding middleware: any request matching
// ShouldForward is proxied whole-cloth to target and short-circuits before
// PocketBase's own handler runs; everything else proceeds normally.
//
// target is deliberately the same http.Handler the CQRS gateway's
// Config.Forward uses (an httputil.ReverseProxy at the master's address) --
// one "forward to master" building block, mounted at two places.
func Register(e *core.ServeEvent, target http.Handler) {
	e.Router.BindFunc(func(re *core.RequestEvent) error {
		if !ShouldForward(re.App, re.Request.Method, re.Request.URL.Path) {
			return re.Next()
		}

		// see gateway.RegisterRoutes' identical fix: PocketBase wraps the
		// request body in a RereadableReadCloser that silently rewinds on
		// EOF instead of staying exhausted, which httputil.ReverseProxy
		// reads through mid-proxy, corrupting the outbound Content-Length.
		body, err := io.ReadAll(re.Request.Body)
		if err != nil {
			return apis.NewBadRequestError("failed reading request body", err)
		}
		re.Request.Body = io.NopCloser(bytes.NewReader(body))
		re.Request.ContentLength = int64(len(body))

		target.ServeHTTP(re.Response, re.Request)
		return nil
	})
}

// ShouldForward reports whether path (method-qualified) is PocketBase's own
// native traffic against an auth-type collection: one of the fixed
// auth-flow suffixes, or any request to an auth-type collection's plain
// records endpoint -- reads included, since F-12's split-brain applies to
// them identically (a secondary listing _users would list its own local,
// divergent rows, not the fleet's). Only auth-methods stays local, being
// schema/config identical on every node. A pure function of
// (app, method, path) deliberately -- not dependent on the request's
// PathValue extraction, which this package's own global middleware binding
// cannot be certain has already run by the time it executes. Exported so
// authverify's middleware can skip requests this package is about to proxy
// whole to the master anyway.
func ShouldForward(app core.App, method, path string) bool {
	collection, rest, ok := splitCollectionPath(path)
	if !ok {
		return false
	}

	if authFlowSuffixes[rest] || strings.HasPrefix(rest, "impersonate/") {
		return true
	}

	if rest != "records" && !strings.HasPrefix(rest, "records/") {
		return false
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPatch, http.MethodDelete:
	default:
		return false
	}

	c, err := app.FindCachedCollectionByNameOrId(collection)
	return err == nil && c != nil && c.IsAuth()
}

// splitCollectionPath splits "/api/collections/{collection}/{rest}" into
// its two parts. ok is false for anything that doesn't match this shape
// (including PocketCQRS's own /api/cqrs/... route, which this package must
// never intercept).
func splitCollectionPath(path string) (collection, rest string, ok bool) {
	const prefix = "/api/collections/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	trimmed := path[len(prefix):]
	idx := strings.IndexByte(trimmed, '/')
	if idx < 0 {
		return "", "", false
	}
	return trimmed[:idx], trimmed[idx+1:], true
}
