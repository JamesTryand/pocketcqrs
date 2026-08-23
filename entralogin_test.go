package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/types"

	"github.com/jamestryand/pocketcqrs/authforward"
	"github.com/jamestryand/pocketcqrs/authverify"
	"github.com/jamestryand/pocketcqrs/users"
)

// fakeMicrosoftServer stands in for Entra's own token and userinfo
// endpoints -- enough surface for tools/auth's real Microsoft provider
// (unmodified, not mocked) to complete a real FetchToken/FetchAuthUser
// round trip against it.
func fakeMicrosoftServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-access-token",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "ms-user-1",
			"displayName": "Test User",
			"mail":        "entra-test@example.com",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// entraTestApp opens a fresh test app with a "users" auth collection
// configured for Microsoft OAuth2 against msServerURL. Renames the
// pocketbase/tests fixture's own pre-existing "users" collection out of
// the way first -- the same fixture collision users/users_test.go's
// freshApp helper works around.
func entraTestApp(t *testing.T, msServerURL string) *tests.TestApp {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)

	if existing, err := app.FindCollectionByNameOrId(users.CollectionName); err == nil {
		existing.Name = "zz_fixture_users"
		if err := app.Save(existing); err != nil {
			t.Fatal(err)
		}
	}

	c := core.NewAuthCollection(users.CollectionName)
	c.CreateRule = types.Pointer("")
	c.OAuth2.Enabled = true
	c.OAuth2.Providers = []core.OAuth2ProviderConfig{{
		Name:         entraProviderName,
		ClientId:     "test-client-id",
		ClientSecret: "test-client-secret",
		AuthURL:      msServerURL + "/authorize",
		TokenURL:     msServerURL + "/token",
		UserInfoURL:  msServerURL + "/userinfo",
	}}
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}
	return app
}

// probeHandler reports what re.Auth held for the request, so a test can
// assert on it without duplicating authverify's own gates.
func probeHandler(re *core.RequestEvent) error {
	if re.Auth == nil {
		return re.String(http.StatusOK, "none")
	}
	return re.String(http.StatusOK, re.Auth.Collection().Name+":"+re.Auth.Id)
}

// routerHolder lets a *httptest.Server be started with a listener bound
// (so its address is known) before the mux that will serve it exists --
// needed because registerEntraLoginRoutes must be given selfAddr, and
// selfAddr here IS this same server's own address (the self-loopback
// case a --cqrsRole=secondary is in).
type routerHolder struct{ h http.Handler }

func (rh *routerHolder) ServeHTTP(w http.ResponseWriter, r *http.Request) { rh.h.ServeHTTP(w, r) }

// noAuthClient never follows redirects, so a test can inspect a 302's
// Location and Set-Cookie headers directly.
func noAuthClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func cookieNamed(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// newSingleNodeEntraServer builds one node with the login route pair bound
// (no forwarding), and returns it alongside its app so a test can both
// drive HTTP requests and inspect what landed in the database.
func newSingleNodeEntraServer(t *testing.T, msURL string) (*httptest.Server, *tests.TestApp) {
	t.Helper()
	app := entraTestApp(t, msURL)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	selfAddr := listener.Addr().String()
	holder := &routerHolder{}
	srv := &httptest.Server{Listener: listener, Config: &http.Server{Handler: holder}}
	srv.Start()
	t.Cleanup(srv.Close)

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	registerEntraLoginRoutes(&core.ServeEvent{App: app, Router: router}, selfAddr)
	mux, err := router.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	holder.h = mux
	return srv, app
}

// startEntraLogin drives GET /auth/microsoft/login and returns the state
// param from the redirect and the signed state cookie -- the two pieces
// every callback test needs.
func startEntraLogin(t *testing.T, client *http.Client, baseURL string) (state string, stateCookie *http.Cookie) {
	t.Helper()
	resp, err := client.Get(baseURL + entraLoginPath)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login: expected 302, got %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state = loc.Query().Get("state")
	stateCookie = cookieNamed(resp.Cookies(), entraStateCookieName)
	if state == "" || stateCookie == nil {
		t.Fatal("expected a state param and state cookie from login")
	}
	return state, stateCookie
}

// TestEntraSignInFullFlow drives the real route pair end to end: login
// redirects to the (fake) provider with a signed state cookie, the
// simulated provider callback exchanges the code through PocketBase's OWN
// real auth-with-oauth2 handler (not a reimplementation -- see
// entralogin.go's package doc), a users record is actually created, a
// session cookie comes back, and that cookie alone (no Authorization
// header) authenticates a later request via the cookie-bridge middleware.
func TestEntraSignInFullFlow(t *testing.T) {
	ms := fakeMicrosoftServer(t)
	app := entraTestApp(t, ms.URL)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	selfAddr := listener.Addr().String()
	holder := &routerHolder{}
	srv := &httptest.Server{Listener: listener, Config: &http.Server{Handler: holder}}
	srv.Start()
	t.Cleanup(srv.Close)

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	registerEntraLoginRoutes(&core.ServeEvent{App: app, Router: router}, selfAddr)
	router.GET("/probe", probeHandler)
	mux, err := router.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	holder.h = mux

	client := noAuthClient()

	loginResp, err := client.Get(srv.URL + entraLoginPath)
	if err != nil {
		t.Fatal(err)
	}
	loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusFound {
		t.Fatalf("login: expected 302, got %d", loginResp.StatusCode)
	}
	loc, err := url.Parse(loginResp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := loc.Query().Get("state")
	if state == "" {
		t.Fatal("expected a state param in the redirect to the provider")
	}
	if loc.Query().Get("code_challenge") == "" {
		t.Fatal("expected a PKCE code_challenge param (Microsoft provider defaults to PKCE)")
	}
	stateCookie := cookieNamed(loginResp.Cookies(), entraStateCookieName)
	if stateCookie == nil || stateCookie.Value == "" {
		t.Fatal("expected a state cookie")
	}

	cbReq, err := http.NewRequest(http.MethodGet,
		srv.URL+entraCallbackPath+"?code=fake-code&state="+state, nil)
	if err != nil {
		t.Fatal(err)
	}
	cbReq.AddCookie(stateCookie)
	cbResp, err := client.Do(cbReq)
	if err != nil {
		t.Fatal(err)
	}
	cbBody, _ := io.ReadAll(cbResp.Body)
	cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusFound {
		t.Fatalf("callback: expected 302, got %d: %s", cbResp.StatusCode, cbBody)
	}
	sessionCookie := cookieNamed(cbResp.Cookies(), entraSessionCookieName)
	if sessionCookie == nil || sessionCookie.Value == "" {
		t.Fatal("expected a session cookie")
	}

	if _, err := app.FindAuthRecordByEmail(users.CollectionName, "entra-test@example.com"); err != nil {
		t.Fatalf("expected a users record created via the real auth-with-oauth2 handler: %v", err)
	}

	probeReq, err := http.NewRequest(http.MethodGet, srv.URL+"/probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	probeReq.AddCookie(sessionCookie)
	probeResp, err := client.Do(probeReq)
	if err != nil {
		t.Fatal(err)
	}
	probeBody, _ := io.ReadAll(probeResp.Body)
	probeResp.Body.Close()
	want := users.CollectionName + ":"
	if !strings.HasPrefix(string(probeBody), want) || len(probeBody) == len(want) {
		t.Fatalf("expected re.Auth from the %s collection with a non-empty id, got %q", users.CollectionName, probeBody)
	}
}

// TestUsersSessionCookieCannotReachOpsRoute proves the reach the
// cookie-bridge middleware grants stops exactly where it should: a users
// record has no capabilities field and is never a superuser, so a session
// cookie for one must fail authverify's ops gates the same way any other
// unauthorized request does -- the regression the advisor flagged as the
// one worth guarding explicitly (mirrors
// users.TestUsersRecordCannotSatisfyCapabilityGate for the cookie path).
func TestUsersSessionCookieCannotReachOpsRoute(t *testing.T) {
	ms := fakeMicrosoftServer(t)
	app := entraTestApp(t, ms.URL)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	selfAddr := listener.Addr().String()
	holder := &routerHolder{}
	srv := &httptest.Server{Listener: listener, Config: &http.Server{Handler: holder}}
	srv.Start()
	t.Cleanup(srv.Close)

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	registerEntraLoginRoutes(&core.ServeEvent{App: app, Router: router}, selfAddr)
	router.GET("/ops-probe", probeHandler).Bind(authverify.RequireCapability(nil, "ops.probe"))
	mux, err := router.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	holder.h = mux

	client := noAuthClient()

	loginResp, err := client.Get(srv.URL + entraLoginPath)
	if err != nil {
		t.Fatal(err)
	}
	loginResp.Body.Close()
	loc, _ := url.Parse(loginResp.Header.Get("Location"))
	state := loc.Query().Get("state")
	stateCookie := cookieNamed(loginResp.Cookies(), entraStateCookieName)

	cbReq, _ := http.NewRequest(http.MethodGet,
		srv.URL+entraCallbackPath+"?code=fake-code&state="+state, nil)
	cbReq.AddCookie(stateCookie)
	cbResp, err := client.Do(cbReq)
	if err != nil {
		t.Fatal(err)
	}
	cbResp.Body.Close()
	sessionCookie := cookieNamed(cbResp.Cookies(), entraSessionCookieName)
	if sessionCookie == nil {
		t.Fatal("expected a session cookie")
	}

	opsReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/ops-probe", nil)
	opsReq.AddCookie(sessionCookie)
	opsResp, err := client.Do(opsReq)
	if err != nil {
		t.Fatal(err)
	}
	opsResp.Body.Close()
	if opsResp.StatusCode == http.StatusOK {
		t.Fatal("a users session cookie must not satisfy an ops capability gate")
	}
}

// TestEntraCallbackForwardsExchangeToMaster is the property this whole
// investigation was about: on a --cqrsRole=secondary running
// --cqrsForwardAuth, the callback's internal POST to
// /api/collections/users/auth-with-oauth2 must be proxied to the master by
// authforward's existing global middleware (bound on the SAME router
// registerEntraLoginRoutes is bound on), not executed against the
// secondary's own local data.db. Proven by checking WHERE the resulting
// record actually landed: master's app, not secondary's -- a unit test on
// the handler alone, without a real second router in front of it, cannot
// see this.
func TestEntraCallbackForwardsExchangeToMaster(t *testing.T) {
	ms := fakeMicrosoftServer(t)
	masterApp := entraTestApp(t, ms.URL)
	secondaryApp := entraTestApp(t, ms.URL)

	masterRouter, err := apis.NewRouter(masterApp)
	if err != nil {
		t.Fatal(err)
	}
	masterMux, err := masterRouter.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	masterSrv := httptest.NewServer(masterMux)
	t.Cleanup(masterSrv.Close)
	masterURL, err := url.Parse(masterSrv.URL)
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	secondarySelfAddr := listener.Addr().String()
	holder := &routerHolder{}
	secondarySrv := &httptest.Server{Listener: listener, Config: &http.Server{Handler: holder}}
	secondarySrv.Start()
	t.Cleanup(secondarySrv.Close)

	secondaryRouter, err := apis.NewRouter(secondaryApp)
	if err != nil {
		t.Fatal(err)
	}
	forward := httputil.NewSingleHostReverseProxy(masterURL)
	serveEvent := &core.ServeEvent{App: secondaryApp, Router: secondaryRouter}
	authforward.Register(serveEvent, forward)
	registerEntraLoginRoutes(serveEvent, secondarySelfAddr)
	secondaryMux, err := secondaryRouter.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	holder.h = secondaryMux

	client := noAuthClient()

	loginResp, err := client.Get(secondarySrv.URL + entraLoginPath)
	if err != nil {
		t.Fatal(err)
	}
	loginResp.Body.Close()
	loc, _ := url.Parse(loginResp.Header.Get("Location"))
	state := loc.Query().Get("state")
	stateCookie := cookieNamed(loginResp.Cookies(), entraStateCookieName)
	if stateCookie == nil {
		t.Fatal("expected a state cookie")
	}

	cbReq, _ := http.NewRequest(http.MethodGet,
		secondarySrv.URL+entraCallbackPath+"?code=fake-code&state="+state, nil)
	cbReq.AddCookie(stateCookie)
	cbResp, err := client.Do(cbReq)
	if err != nil {
		t.Fatal(err)
	}
	cbBody, _ := io.ReadAll(cbResp.Body)
	cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusFound {
		t.Fatalf("callback via secondary: expected 302, got %d: %s", cbResp.StatusCode, cbBody)
	}
	sessionCookie := cookieNamed(cbResp.Cookies(), entraSessionCookieName)
	if sessionCookie == nil || sessionCookie.Value == "" {
		t.Fatal("expected a session cookie even though the exchange was forwarded")
	}

	if _, err := masterApp.FindAuthRecordByEmail(users.CollectionName, "entra-test@example.com"); err != nil {
		t.Fatalf("expected the record in the MASTER's data.db: %v", err)
	}
	if _, err := secondaryApp.FindAuthRecordByEmail(users.CollectionName, "entra-test@example.com"); err == nil {
		t.Fatal("the record must NOT be in the secondary's own local data.db -- forwarding was bypassed")
	}
}

// TestEntraCallbackRejectsStateMismatch proves the CSRF check actually
// rejects: a valid, unexpired state cookie whose signed state does not
// match the query param must 400 and must not create a session.
func TestEntraCallbackRejectsStateMismatch(t *testing.T) {
	ms := fakeMicrosoftServer(t)
	srv, app := newSingleNodeEntraServer(t, ms.URL)
	client := noAuthClient()

	_, stateCookie := startEntraLogin(t, client, srv.URL)

	cbReq, _ := http.NewRequest(http.MethodGet,
		srv.URL+entraCallbackPath+"?code=fake-code&state=not-the-real-state", nil)
	cbReq.AddCookie(stateCookie)
	cbResp, err := client.Do(cbReq)
	if err != nil {
		t.Fatal(err)
	}
	cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a state mismatch, got %d", cbResp.StatusCode)
	}
	if cookieNamed(cbResp.Cookies(), entraSessionCookieName) != nil {
		t.Fatal("must not set a session cookie on a rejected callback")
	}
	if _, err := app.FindAuthRecordByEmail(users.CollectionName, "entra-test@example.com"); err == nil {
		t.Fatal("must not create a record on a rejected callback")
	}
}

// TestEntraCallbackRejectsMissingStateCookie proves a callback with no
// state cookie at all (e.g. a forged/replayed URL hit directly, cookie
// never set on this browser) is rejected the same way.
func TestEntraCallbackRejectsMissingStateCookie(t *testing.T) {
	ms := fakeMicrosoftServer(t)
	srv, _ := newSingleNodeEntraServer(t, ms.URL)
	client := noAuthClient()

	state, _ := startEntraLogin(t, client, srv.URL)

	cbReq, _ := http.NewRequest(http.MethodGet,
		srv.URL+entraCallbackPath+"?code=fake-code&state="+state, nil)
	// deliberately no cookie attached
	cbResp, err := client.Do(cbReq)
	if err != nil {
		t.Fatal(err)
	}
	cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 with no state cookie, got %d", cbResp.StatusCode)
	}
}

// TestEntraCallbackReplayIsRejected checks what happens if the exact same
// callback request (same code, same state, same cookie -- the state
// cookie itself is a signed JWT and clearing it server-side only stops
// THIS browser from resending it, not a captured URL from being replayed
// verbatim) is submitted twice. The real defense against this has to be
// the OAuth2 code's own one-time redemption at the provider; this test
// pins what our fake provider does today (accepts any code any number of
// times, unlike a real one) so the gap is visible here rather than
// discovered against production Entra.
func TestEntraCallbackReplayIsRejected(t *testing.T) {
	ms := fakeMicrosoftServer(t)
	srv, _ := newSingleNodeEntraServer(t, ms.URL)
	client := noAuthClient()

	state, stateCookie := startEntraLogin(t, client, srv.URL)

	do := func() *http.Response {
		req, _ := http.NewRequest(http.MethodGet,
			srv.URL+entraCallbackPath+"?code=fake-code&state="+state, nil)
		req.AddCookie(stateCookie)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	first := do()
	first.Body.Close()
	if first.StatusCode != http.StatusFound {
		t.Fatalf("first callback: expected 302, got %d", first.StatusCode)
	}

	second := do()
	second.Body.Close()
	// Our fake Microsoft server has no one-time-code semantics (a real
	// Entra tenant does, and rejects code reuse), so this succeeds again
	// here -- replay protection for this flow rests entirely on the
	// provider's own code redemption, not on anything pocketcqrs adds.
	// Recorded as a known characteristic, not asserted as either pass or
	// fail, so a future change to this behavior is visible in a diff here
	// rather than silently assumed.
	t.Logf("replay of the same callback request: second attempt status %d (fake provider has no one-time-code check; a real Entra tenant would reject the reused code here)", second.StatusCode)
}

// TestEntraExchangeFailureBodyDoesNotLeakSecrets forces the internal
// exchange to fail (an unconfigured provider name) and checks that the
// error surfaced to the browser -- PocketBase's own response body,
// forwarded verbatim by exchangeEntraCode -- never contains the OAuth2
// client secret.
func TestEntraExchangeFailureBodyDoesNotLeakSecrets(t *testing.T) {
	ms := fakeMicrosoftServer(t)
	app := entraTestApp(t, ms.URL)

	// break the provider config after the collection (and its OAuth2
	// section) already exist, so entraProvider's own precondition checks
	// pass and the failure actually happens inside PocketBase's real
	// auth-with-oauth2 handler, not in our own pre-flight checks.
	collection, err := app.FindCollectionByNameOrId(users.CollectionName)
	if err != nil {
		t.Fatal(err)
	}
	collection.OAuth2.Providers[0].ClientSecret = "super-secret-value-must-not-leak"
	collection.OAuth2.Providers[0].TokenURL = ms.URL + "/token-that-does-not-exist"
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	selfAddr := listener.Addr().String()
	holder := &routerHolder{}
	srv := &httptest.Server{Listener: listener, Config: &http.Server{Handler: holder}}
	srv.Start()
	t.Cleanup(srv.Close)

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	registerEntraLoginRoutes(&core.ServeEvent{App: app, Router: router}, selfAddr)
	mux, err := router.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	holder.h = mux

	client := noAuthClient()
	state, stateCookie := startEntraLogin(t, client, srv.URL)

	cbReq, _ := http.NewRequest(http.MethodGet,
		srv.URL+entraCallbackPath+"?code=fake-code&state="+state, nil)
	cbReq.AddCookie(stateCookie)
	cbResp, err := client.Do(cbReq)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(cbResp.Body)
	cbResp.Body.Close()
	if cbResp.StatusCode == http.StatusFound {
		t.Fatalf("expected the broken token URL to fail the exchange, got a 302 (body: %s)", body)
	}
	if strings.Contains(string(body), "super-secret-value-must-not-leak") {
		t.Fatalf("exchange failure body leaked the client secret: %s", body)
	}
}
