package authforward

import (
	"net/http"
	"testing"

	"github.com/pocketbase/pocketbase/tests"
)

func openTestApp(t *testing.T) *tests.TestApp {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	return app
}

func TestShouldForwardAuthFlowSuffixes(t *testing.T) {
	app := openTestApp(t)
	cases := []struct {
		method, path string
	}{
		{http.MethodPost, "/api/collections/_superusers/auth-with-password"},
		{http.MethodPost, "/api/collections/_superusers/auth-with-oauth2"},
		{http.MethodPost, "/api/collections/_superusers/auth-refresh"},
		{http.MethodPost, "/api/collections/_superusers/request-otp"},
		{http.MethodPost, "/api/collections/_superusers/auth-with-otp"},
		{http.MethodPost, "/api/collections/_superusers/request-password-reset"},
		{http.MethodPost, "/api/collections/_superusers/confirm-password-reset"},
		{http.MethodPost, "/api/collections/_superusers/request-verification"},
		{http.MethodPost, "/api/collections/_superusers/confirm-verification"},
		{http.MethodPost, "/api/collections/_superusers/request-email-change"},
		{http.MethodPost, "/api/collections/_superusers/confirm-email-change"},
		{http.MethodPost, "/api/collections/_superusers/impersonate/abc123"},
	}
	for _, c := range cases {
		if !ShouldForward(app, c.method, c.path) {
			t.Errorf("expected %s %s to be forwarded", c.method, c.path)
		}
	}
}

func TestShouldNotForwardAuthMethodsSchemaEndpoint(t *testing.T) {
	// GET, config/schema only, identical on every node -- safe and better
	// to serve locally, never forwarded
	app := openTestApp(t)
	if ShouldForward(app, http.MethodGet, "/api/collections/_superusers/auth-methods") {
		t.Fatal("expected auth-methods to be served locally, not forwarded")
	}
}

func TestShouldForwardWriteShapedRecordsOnAuthCollection(t *testing.T) {
	app := openTestApp(t)
	cases := []struct{ method string }{
		{http.MethodPost}, {http.MethodPatch}, {http.MethodDelete},
	}
	for _, c := range cases {
		path := "/api/collections/_superusers/records"
		if c.method == http.MethodPatch || c.method == http.MethodDelete {
			path += "/someid"
		}
		if !ShouldForward(app, c.method, path) {
			t.Errorf("expected %s %s to be forwarded", c.method, path)
		}
	}
}

func TestShouldForwardReadsOnAuthCollectionRecords(t *testing.T) {
	// reads forward too: a secondary's auth-collection rows are its own
	// local, divergent data, so listing/viewing them locally serves the
	// wrong rows -- the same split-brain F-12 fixed for writes
	app := openTestApp(t)
	if !ShouldForward(app, http.MethodGet, "/api/collections/_superusers/records") {
		t.Fatal("expected a GET to _superusers/records to be forwarded")
	}
	if !ShouldForward(app, http.MethodGet, "/api/collections/_superusers/records/someid") {
		t.Fatal("expected a GET to _superusers/records/{id} to be forwarded")
	}
	if !ShouldForward(app, http.MethodHead, "/api/collections/_superusers/records") {
		t.Fatal("expected a HEAD to _superusers/records to be forwarded")
	}
}

func TestShouldNotForwardNonAuthCollection(t *testing.T) {
	// _authOrigins is a real, existing PocketBase collection that is NOT
	// itself auth-type (it stores fingerprints ABOUT auth records) --
	// exactly the negative case that matters: not everything under
	// /api/collections/ should forward, only auth-type collections.
	app := openTestApp(t)
	if ShouldForward(app, http.MethodPost, "/api/collections/_authOrigins/records") {
		t.Fatal("expected _authOrigins (not an auth-type collection) to stay local")
	}
}

func TestShouldNotForwardUnknownCollection(t *testing.T) {
	app := openTestApp(t)
	if ShouldForward(app, http.MethodPost, "/api/collections/does-not-exist/records") {
		t.Fatal("expected an unknown collection to stay local, not forwarded")
	}
}

func TestShouldNotForwardUnrelatedRoutes(t *testing.T) {
	app := openTestApp(t)
	cases := []struct{ method, path string }{
		{http.MethodGet, "/api/health"},
		{http.MethodPost, "/api/cqrs/task/t1/Create"},
		{http.MethodGet, "/api/cqrs/events"},
		{http.MethodGet, "/api/collections/_superusers"}, // the collection itself, not a sub-route
	}
	for _, c := range cases {
		if ShouldForward(app, c.method, c.path) {
			t.Errorf("expected %s %s to stay local", c.method, c.path)
		}
	}
}

func TestSplitCollectionPath(t *testing.T) {
	cases := []struct {
		path                 string
		wantCollection, rest string
		wantOK               bool
	}{
		{"/api/collections/_superusers/records", "_superusers", "records", true},
		{"/api/collections/_superusers/records/abc", "_superusers", "records/abc", true},
		{"/api/collections/_superusers", "", "", false},
		{"/api/collections/", "", "", false},
		{"/api/health", "", "", false},
		{"/api/cqrs/task/t1/Create", "", "", false},
	}
	for _, c := range cases {
		collection, rest, ok := splitCollectionPath(c.path)
		if ok != c.wantOK || collection != c.wantCollection || rest != c.rest {
			t.Errorf("splitCollectionPath(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.path, collection, rest, ok, c.wantCollection, c.rest, c.wantOK)
		}
	}
}
