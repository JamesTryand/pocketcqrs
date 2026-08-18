package authverify

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
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

// newSuperuser creates a fresh superuser on app and mints a real auth
// token for it. Fresh rather than a fixture one, so a second test app
// (cloned from the same fixture) does NOT have the record — the exact
// shape F-13 is about: the collection exists on every node, the row only
// on the master.
func newSuperuser(t *testing.T, app core.App, email string) (*core.Record, string) {
	t.Helper()
	col, err := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	if err != nil {
		t.Fatal(err)
	}
	rec := core.NewRecord(col)
	rec.SetEmail(email)
	rec.SetPassword("1234567890")
	if err := app.Save(rec); err != nil {
		t.Fatal(err)
	}
	token, err := rec.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	return rec, token
}

// serveNode builds a real PocketBase router for app — default middlewares
// included, exactly what a running node has — applies register to it, and
// serves the result.
func serveNode(t *testing.T, app *tests.TestApp, register func(*core.ServeEvent)) *httptest.Server {
	t.Helper()
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	register(&core.ServeEvent{App: app, Router: router})
	mux, err := router.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func postVerify(t *testing.T, base, token string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"token": token})
	resp, err := http.Post(base+verifyPath, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestEndpointVerifiesARealToken(t *testing.T) {
	app := openTestApp(t)
	rec, token := newSuperuser(t, app, "f13-endpoint@example.com")
	srv := serveNode(t, app, func(e *core.ServeEvent) { RegisterEndpoint(e, nil) })

	resp := postVerify(t, srv.URL, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for a valid token, got %d", resp.StatusCode)
	}
	var verdict Verdict
	if err := json.NewDecoder(resp.Body).Decode(&verdict); err != nil {
		t.Fatal(err)
	}
	if verdict.CollectionName != core.CollectionNameSuperusers {
		t.Fatalf("wrong collection: %q", verdict.CollectionName)
	}
	var fields map[string]any
	if err := json.Unmarshal(verdict.Record, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["id"] != rec.Id {
		t.Fatalf("wrong record: %v", fields["id"])
	}
	if fields["email"] != "f13-endpoint@example.com" {
		t.Fatalf("expected the holder's own email in the verdict, got %v", fields["email"])
	}
	raw := string(verdict.Record)
	if strings.Contains(raw, "tokenKey") || strings.Contains(raw, rec.TokenKey()) {
		t.Fatal("verdict must never carry key material")
	}
	if strings.Contains(raw, "password") {
		t.Fatal("verdict must never carry the password field")
	}
}

func TestEndpointRejectsGarbageAndForeignTokens(t *testing.T) {
	app := openTestApp(t)
	srv := serveNode(t, app, func(e *core.ServeEvent) { RegisterEndpoint(e, nil) })

	// a token some OTHER node minted: structurally valid, wrong signature
	otherApp := openTestApp(t)
	_, foreignToken := newSuperuser(t, otherApp, "f13-foreign@example.com")

	for name, token := range map[string]string{
		"garbage": "not-a-jwt-at-all",
		"foreign": foreignToken,
	} {
		resp := postVerify(t, srv.URL, token)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s token: expected 401, got %d", name, resp.StatusCode)
		}
	}
}
