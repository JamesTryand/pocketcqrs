package authverify

import (
	"errors"
	"net/http"
	"net/netip"
	"strings"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"

	"github.com/jamestryand/pocketcqrs/authforward"
)

// Register binds the global middleware that gives a secondary's own local
// routes a working re.Auth (F-13's fix, end-user half).
//
// Ordering, established from PocketBase's source rather than assumed:
// PocketBase's default middlewares carry negative priorities
// (loadAuthToken is -1020), route-group Bind handlers default to 0, and
// BuildMux sorts a route's chain ascending and stably with root-group
// handlers ahead of route-bound ones. So this middleware runs AFTER
// loadAuthToken — which, for a master-minted token, fails its local
// data.db lookup and silently leaves re.Auth nil (its documented
// contract) — and BEFORE every route handler and route-bound RequireAuth.
// Exactly the seam authforward already proved works.
func Register(e *core.ServeEvent, v *Verifier) {
	e.Router.Bind(&hook.Handler[*core.RequestEvent]{
		Id: "pcVerifyAuth",
		Func: func(re *core.RequestEvent) error {
			if re.Auth != nil {
				// verified against local data.db after all (e.g. a token
				// minted by this node's own CLI) — nothing to do
				return re.Next()
			}
			token := tokenFromRequest(re)
			if token == "" {
				return re.Next()
			}
			// anything about to be proxied whole to the master gets
			// authenticated there; verifying it here first would just double
			// the round-trips
			if authforward.ShouldForward(re.App, re.Request.Method, re.Request.URL.Path) ||
				isForwardedCommand(re.Request.Method, re.Request.URL.Path) {
				return re.Next()
			}

			verdict, err := v.VerifyCached(re.Request.Context(), token)
			switch {
			case errors.Is(err, ErrInvalidToken):
				// loadAuthToken's contract: an unusable token is not an
				// error, the request proceeds unauthenticated and any
				// RequireAuth on the route answers 401
				return re.Next()
			case err != nil:
				// no verdict at all: fail closed, and say so as 503 — a 401
				// would send the client to a login flow that cannot work
				// while the master is down either
				return re.JSON(http.StatusServiceUnavailable, map[string]string{
					"error": "token verification unavailable: cannot reach the master",
				})
			}

			rec, err := Materialize(re.App, verdict)
			if err != nil {
				re.App.Logger().Warn("authverify: dropping verified token", "error", err)
				return re.Next()
			}
			if err := checkSuperuserIP(re, rec); err != nil {
				return err
			}
			re.Auth = rec
			return re.Next()
		},
	})
}

// tokenFromRequest mirrors PocketBase's unexported getAuthTokenFromRequest:
// the Authorization header, with an optional Bearer prefix.
func tokenFromRequest(re *core.RequestEvent) string {
	token := re.Request.Header.Get("Authorization")
	if len(token) > 7 && strings.EqualFold(token[:7], "Bearer ") {
		return token[7:]
	}
	return token
}

// isForwardedCommand reports whether path is the gateway's command route
// (POST /api/cqrs/{aggregate}/{id}/{command}) — on a node running this
// middleware that request is proxied whole to the master (verify mode
// requires --cqrsMasterAddr). Ops routes of the same arity
// (deadletters/{id}/retry) are also skipped harmlessly: their superuser
// gate re-verifies for itself, fresh.
func isForwardedCommand(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	rest, ok := strings.CutPrefix(path, "/api/cqrs/")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	return len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != ""
}

// checkSuperuserIP re-applies PocketBase's superuser IP whitelist. Its own
// middleware ran earlier in the chain (priority -1015) while re.Auth was
// still nil, so a remotely-verified superuser would silently bypass it
// otherwise. Same per-node semantics as everything in Settings(): the list
// checked is THIS node's.
func checkSuperuserIP(re *core.RequestEvent, rec *core.Record) error {
	if !rec.IsSuperuser() {
		return nil
	}
	ips := re.App.Settings().SuperuserIPs
	if len(ips) > 0 && !ipInList(ips, re.RealIP()) {
		return re.ForbiddenError("", errors.New("superuser IP is not whitelisted"))
	}
	return nil
}

// ipInList mirrors PocketBase's unexported isIPInList: individual IPs and
// CIDR subnets.
func ipInList(ipsOrSubnets []string, ip string) bool {
	if len(ipsOrSubnets) == 0 || ip == "" {
		return false
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	for _, item := range ipsOrSubnets {
		if prefix, err := netip.ParsePrefix(item); err == nil {
			if prefix.Contains(addr) {
				return true
			}
			continue
		}
		if listed, err := netip.ParseAddr(item); err == nil && listed == addr {
			return true
		}
	}
	return false
}
