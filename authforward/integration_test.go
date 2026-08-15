package authforward

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

// newTestRouter builds a real PocketBase router with authforward.Register
// bound, proxying to a fake "master" handler, and returns a real HTTP
// server driving it -- proving Register actually intercepts through the
// full router/middleware machinery, not just that shouldForward's logic is
// correct in isolation.
func newTestRouter(t *testing.T, masterHandler http.Handler) *httptest.Server {
	t.Helper()

	app := openTestApp(t)

	master := httptest.NewServer(masterHandler)
	t.Cleanup(master.Close)
	masterURL, err := url.Parse(master.URL)
	if err != nil {
		t.Fatal(err)
	}

	pbRouter, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	Register(&core.ServeEvent{App: app, Router: pbRouter}, httputil.NewSingleHostReverseProxy(masterURL))

	mux, err := pbRouter.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestRegisterForwardsAuthWithPasswordToMaster(t *testing.T) {
	reached := false
	srv := newTestRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if r.URL.Path != "/api/collections/_superusers/auth-with-password" {
			t.Errorf("unexpected path reaching master: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusTeapot) // distinctive, so we know THIS response came back
	}))

	resp, err := http.Post(srv.URL+"/api/collections/_superusers/auth-with-password",
		"application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if !reached {
		t.Fatal("expected the request to reach the master")
	}
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("expected the master's distinctive response, got %d", resp.StatusCode)
	}
}

func TestRegisterDoesNotForwardHealthCheck(t *testing.T) {
	reached := false
	srv := newTestRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))

	resp, err := http.Get(srv.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if reached {
		t.Fatal("expected /api/health to be served locally, not forwarded to master")
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the local health check to still work, got %d", resp.StatusCode)
	}
}

func TestRegisterDoesNotForwardAuthMethods(t *testing.T) {
	reached := false
	srv := newTestRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))

	resp, err := http.Get(srv.URL + "/api/collections/_superusers/auth-methods")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if reached {
		t.Fatal("expected auth-methods to be served locally (schema/config only), not forwarded")
	}
	// served locally by PocketBase's own handler -- some real response,
	// not a proxy pass-through
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		t.Fatal("expected a real local response body")
	}
}

func TestRegisterForwardsWriteToAuthCollectionRecords(t *testing.T) {
	reached := false
	srv := newTestRouter(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusTeapot)
	}))

	resp, err := http.Post(srv.URL+"/api/collections/_superusers/records", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if !reached {
		t.Fatal("expected a write to _superusers/records to reach the master")
	}
	if resp.StatusCode != http.StatusTeapot {
		t.Fatalf("expected the master's distinctive response, got %d", resp.StatusCode)
	}
}
