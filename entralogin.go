package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/auth"
	"github.com/pocketbase/pocketbase/tools/hook"
	"github.com/pocketbase/pocketbase/tools/security"
	"golang.org/x/oauth2"

	"github.com/jamestryand/pocketcqrs/users"
)

// Item 12: Microsoft/Entra sign-in for the users collection (see users.go).
// This is a route pair around PocketBase's OWN native
// /api/collections/users/auth-with-oauth2 handler -- not a reimplementation
// of the OAuth2 token exchange. That distinction is load-bearing on a
// multi-node deployment: auth-with-oauth2 is already on authforward's
// forwarding suffix list (authforward.go's authFlowSuffixes), so calling
// the real endpoint gets F-12's split-brain protection for free, exactly
// like every other native auth flow already does. Reimplementing the
// exchange directly against tools/auth + app.Save would silently bypass
// that middleware -- it operates at the router level by URL path, not by
// which code happens to call it -- and write to whichever node's LOCAL
// data.db this handler's process happens to be, which on a --cqrsRole=
// secondary is exactly the wrong table (see authforward.go's package doc).
//
// The self-referential call target is --cqrsSelfAddr, an EXPLICIT operator-
// supplied address -- deliberately NOT derived from the incoming request's
// Host/X-Forwarded-* headers, which are client-controllable. Using a
// header-derived value for a server-side outbound call would be an SSRF
// primitive: a request carrying a forged X-Forwarded-Host could redirect
// this handler's own token-bearing POST anywhere. The PUBLIC redirect_uri
// (below) is a different job with a different control -- Microsoft's own
// redirect-URI allowlist -- so it stays header-derived; the self-call does
// not have an equivalent external check and needs its own.
const (
	entraProviderName      = "microsoft"
	entraLoginPath         = "/auth/microsoft/login"
	entraCallbackPath      = "/auth/microsoft/callback"
	entraStateCookieName   = "pc_oauth_state"
	entraSessionCookieName = "pc_users_session"
	entraStateTTL          = 10 * time.Minute
)

// entraExchangeTimeout bounds the self-referential POST to
// auth-with-oauth2 -- generous enough for a forwarded call to reach a
// master under normal load, short enough that a hung master doesn't hang
// this request indefinitely.
const entraExchangeTimeout = 15 * time.Second

// registerEntraLoginRoutes binds the Microsoft/Entra sign-in route pair for
// the users collection, plus the cookie-bridge middleware that makes the
// resulting session cookie usable on subsequent requests. Registered
// unconditionally, like the roles/users collections themselves -- if the
// users collection has no "microsoft" OAuth2 provider configured, the
// login route answers a clear 400 instead of silently 404ing.
//
// selfAddr is --cqrsSelfAddr: this node's own --http listen address, used
// ONLY for the internal exchange call (see the package doc above for why
// it can't be derived from the request).
func registerEntraLoginRoutes(e *core.ServeEvent, selfAddr string) {
	e.Router.GET(entraLoginPath, func(re *core.RequestEvent) error {
		collection, provider, err := entraProvider(re.App)
		if err != nil {
			return err
		}

		redirectURL := publicBaseURL(re.Request) + entraCallbackPath
		provider.SetContext(re.Request.Context())
		provider.SetRedirectURL(redirectURL)

		state := security.RandomString(30)
		claims := map[string]any{"state": state}
		var opts []oauth2.AuthCodeOption
		if provider.PKCE() {
			verifier := security.RandomString(43)
			claims["codeVerifier"] = verifier
			opts = append(opts,
				oauth2.SetAuthURLParam("code_challenge", security.S256Challenge(verifier)),
				oauth2.SetAuthURLParam("code_challenge_method", "S256"),
			)
		}

		stateToken, err := security.NewJWT(claims, collection.AuthToken.Secret, entraStateTTL)
		if err != nil {
			return apis.NewInternalServerError("failed to prepare sign-in state", err)
		}
		http.SetCookie(re.Response, &http.Cookie{
			Name:     entraStateCookieName,
			Value:    stateToken,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(entraStateTTL.Seconds()),
		})

		return re.Redirect(http.StatusFound, provider.BuildAuthURL(state, opts...))
	})

	e.Router.GET(entraCallbackPath, func(re *core.RequestEvent) error {
		if errParam := re.Request.URL.Query().Get("error"); errParam != "" {
			return apis.NewBadRequestError("Microsoft sign-in failed: "+errParam, nil)
		}
		code := re.Request.URL.Query().Get("code")
		if code == "" {
			return apis.NewBadRequestError("missing OAuth2 code", nil)
		}

		collection, _, err := entraProvider(re.App)
		if err != nil {
			return err
		}

		stateCookie, err := re.Request.Cookie(entraStateCookieName)
		if err != nil || stateCookie.Value == "" {
			return apis.NewBadRequestError("missing or expired sign-in state", nil)
		}
		clearCookie(re.Response, entraStateCookieName)

		claims, err := security.ParseJWT(stateCookie.Value, collection.AuthToken.Secret)
		if err != nil {
			return apis.NewBadRequestError("invalid sign-in state", err)
		}
		state, _ := claims["state"].(string)
		if state == "" || state != re.Request.URL.Query().Get("state") {
			return apis.NewBadRequestError("sign-in state mismatch", nil)
		}
		codeVerifier, _ := claims["codeVerifier"].(string)

		redirectURL := publicBaseURL(re.Request) + entraCallbackPath
		sessionToken, err := exchangeEntraCode(re.Request.Context(), selfAddr, code, codeVerifier, redirectURL)
		if err != nil {
			return err
		}

		http.SetCookie(re.Response, &http.Cookie{
			Name:     entraSessionCookieName,
			Value:    sessionToken,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		return re.Redirect(http.StatusFound, "/")
	})

	// Cookie-to-re.Auth bridge: without this, the session cookie set above
	// is decorative -- nothing reads it back. Mirrors authverify.Register's
	// binding pattern (default priority, only acts when re.Auth is still
	// nil, so it runs after PocketBase's own loadAuthToken and never
	// overrides an Authorization-header-authenticated request). This makes
	// EVERY route on this process cookie-authenticatable, ops routes
	// included -- but a users-collection record can never satisfy
	// authverify.RequireCapability or RequireSuperuser (no capabilities
	// field, not a superuser; see users.go and
	// TestUsersRecordCannotSatisfyCapabilityGate), so that reach has no
	// teeth there. See entralogin_test.go's equivalent proof for the
	// cookie path specifically.
	e.Router.Bind(&hook.Handler[*core.RequestEvent]{
		Id: "pcUsersSessionCookie",
		Func: func(re *core.RequestEvent) error {
			if re.Auth != nil {
				return re.Next()
			}
			ck, err := re.Request.Cookie(entraSessionCookieName)
			if err != nil || ck.Value == "" {
				return re.Next()
			}
			rec, err := re.App.FindAuthRecordByToken(ck.Value, core.TokenTypeAuth)
			if err != nil {
				return re.Next()
			}
			re.Auth = rec
			return re.Next()
		},
	})
}

// entraProvider looks up the users collection's configured "microsoft"
// OAuth2 provider, or a clear 400 if it isn't set up -- the same shape
// PocketBase's own recordAuthWithOAuth2 answers with for an unconfigured
// provider, not a 404 or 500 that would suggest the route itself is broken.
func entraProvider(app core.App) (*core.Collection, auth.Provider, error) {
	collection, err := app.FindCollectionByNameOrId(users.CollectionName)
	if err != nil {
		return nil, nil, apis.NewBadRequestError("users collection is not provisioned", err)
	}
	if !collection.OAuth2.Enabled {
		return nil, nil, apis.NewBadRequestError("Microsoft sign-in is not configured on the users collection", nil)
	}
	providerConfig, ok := collection.OAuth2.GetProviderConfig(entraProviderName)
	if !ok {
		return nil, nil, apis.NewBadRequestError("Microsoft sign-in is not configured on the users collection", nil)
	}
	provider, err := providerConfig.InitProvider()
	if err != nil {
		return nil, nil, apis.NewInternalServerError("failed to init Microsoft provider", err)
	}
	return collection, provider, nil
}

// exchangeEntraCode calls PocketBase's own real auth-with-oauth2 endpoint
// at selfAddr -- never reimplemented locally, see the package doc above --
// and returns the resulting auth token. On a --cqrsRole=secondary running
// --cqrsForwardAuth, this request is transparently proxied to the master
// by authforward's global middleware before PocketBase's own handler ever
// runs on this node; exchangeEntraCode itself has no master/secondary
// awareness at all, which is the point.
func exchangeEntraCode(ctx context.Context, selfAddr, code, codeVerifier, redirectURL string) (string, error) {
	body, err := json.Marshal(map[string]string{
		"provider":     entraProviderName,
		"code":         code,
		"codeVerifier": codeVerifier,
		"redirectURL":  redirectURL,
	})
	if err != nil {
		return "", apis.NewInternalServerError("failed to build exchange request", err)
	}

	target := "http://" + selfAddr + "/api/collections/" + users.CollectionName + "/auth-with-oauth2"
	ctx, cancel := context.WithTimeout(ctx, entraExchangeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return "", apis.NewInternalServerError("failed to build exchange request", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", apis.NewApiError(http.StatusBadGateway,
			"failed to reach this node's own --cqrsSelfAddr for the OAuth2 exchange", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", apis.NewInternalServerError("failed reading exchange response", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", apis.NewApiError(resp.StatusCode,
			fmt.Sprintf("OAuth2 exchange failed: %s", string(respBody)), nil)
	}

	var parsed struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil || parsed.Token == "" {
		return "", apis.NewInternalServerError("malformed exchange response", err)
	}
	return parsed.Token, nil
}

// publicBaseURL derives the scheme+host the BROWSER used to reach this
// request, honoring X-Forwarded-Proto/X-Forwarded-Host from a reverse
// proxy. Used ONLY to build the OAuth2 redirect_uri Microsoft is told
// about -- see the package doc above for why this must NOT also be used
// for the self-referential exchange call, even though both need "this
// server's own address" in some sense. redirect_uri has an external check
// (Microsoft's own allowlist for the app registration); a self-POST built
// from the same client-controllable headers would not.
func publicBaseURL(r *http.Request) string {
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	return scheme + "://" + host
}

// clearCookie expires the named cookie immediately, mirroring
// pocketcqrs-dashboard/server.go's clearAuthCookie shape.
func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
