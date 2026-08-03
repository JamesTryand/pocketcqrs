package main

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// sampleCatalog mirrors the public GET /api/cqrs/catalog JSON.
const sampleCatalog = `{
  "generatedAt": "2026-08-03 10:00:00.000Z",
  "mode": "running",
  "totals": {"events": 42, "streams": 7, "deadLettersPending": 2},
  "aggregates": [
    {"name": "task", "origin": "go", "streams": 3,
     "events": [{"type": "TaskCreated", "count": 5, "minVersion": 1, "maxVersion": 1}]}
  ],
  "consumers": [
    {"name": "tasks", "kind": "go-projection", "eventTypes": ["TaskCreated"],
     "collections": ["tasks"], "checkpoint": 42}
  ],
  "collections": [
    {"name": "tasks", "guarded": true, "owner": "tasks", "fields": ["title:text"]}
  ],
  "functions": {"http": ["hello"], "cron": [{"name": "heartbeat", "schedule": "* * * * *"}]},
  "flows": [],
  "mermaid": "flowchart LR\n  agg_task([\"task\"])\n"
}`

// fakeBackend implements the two public endpoints the dashboard consumes.
func fakeBackend(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/collections/_superusers/auth-with-password", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Identity string `json:"identity"`
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Identity == "admin@example.com" && body.Password == "secret" {
			w.Write([]byte(`{"token":"tok123"}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"code":400,"message":"Failed to authenticate."}`))
	})
	mux.HandleFunc("GET /api/cqrs/catalog", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "tok123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(sampleCatalog))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// dashboardClient returns an http.Client that does not follow redirects
// (so 303s are assertable) but keeps cookies across requests.
func dashboardClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func TestOverviewRequiresLogin(t *testing.T) {
	backend := fakeBackend(t)
	dash := httptest.NewServer(newServer(backend.URL).routes())
	t.Cleanup(dash.Close)

	resp, err := dashboardClient(t).Get(dash.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("expected 303 to /login, got %s (%s)", resp.Status, resp.Header.Get("Location"))
	}
}

func TestLoginFailure(t *testing.T) {
	backend := fakeBackend(t)
	dash := httptest.NewServer(newServer(backend.URL).routes())
	t.Cleanup(dash.Close)

	resp, err := dashboardClient(t).PostForm(dash.URL+"/login", map[string][]string{
		"identity": {"admin@example.com"}, "password": {"wrong"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %s", resp.Status)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Sign-in failed") {
		t.Fatalf("expected error callout in body:\n%s", body)
	}
}

func TestLoginThenOverview(t *testing.T) {
	backend := fakeBackend(t)
	dash := httptest.NewServer(newServer(backend.URL).routes())
	t.Cleanup(dash.Close)
	client := dashboardClient(t)

	// wrong credentials set no cookie; good credentials redirect with one
	resp, err := client.PostForm(dash.URL+"/login", map[string][]string{
		"identity": {"admin@example.com"}, "password": {"secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("expected 303 to /, got %s", resp.Status)
	}
	if !hasCookie(resp, authCookieName) {
		t.Fatal("expected auth cookie to be set")
	}

	resp, err = client.Get(dash.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %s", resp.Status)
	}
	body := readBody(t, resp)
	for _, want := range []string{
		"Running.",         // mode banner
		"Events in log",    // totals cards
		">42</strong>",     // events total
		"stat-danger",      // pending dead letters highlighted
		`id="explorer"`,    // cytoscape container
		`id="catalog-data"`, // embedded catalog JSON
		"flowchart LR",     // mermaid source panel
		"/assets/webawesome/webawesome.loader.js",
		"/assets/vendor/cytoscape.min.js",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("overview missing %q", want)
		}
	}
	// the embedded catalog JSON is valid and carries the public fields
	var cat map[string]any
	start := strings.Index(body, `id="catalog-data"`)
	if start < 0 {
		t.Fatal("catalog-data script block missing")
	}
	jsonStart := strings.Index(body[start:], ">") + start + 1
	jsonEnd := strings.Index(body[jsonStart:], "</script>") + jsonStart
	if err := json.Unmarshal([]byte(body[jsonStart:jsonEnd]), &cat); err != nil {
		t.Fatalf("embedded catalog JSON does not parse: %v", err)
	}
	if cat["mode"] != "running" || cat["mermaid"] == nil {
		t.Fatalf("embedded catalog JSON missing fields: %v", cat)
	}
}

func TestExpiredTokenReturnsToLogin(t *testing.T) {
	backend := fakeBackend(t)
	dash := httptest.NewServer(newServer(backend.URL).routes())
	t.Cleanup(dash.Close)
	client := dashboardClient(t)

	req, _ := http.NewRequest(http.MethodGet, dash.URL+"/", nil)
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: "stale"})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("expected 303 to /login, got %s", resp.Status)
	}
	if !hasCookieDeleted(resp, authCookieName) {
		t.Fatal("expected the stale auth cookie to be cleared")
	}
}

func TestPlaceholderPages(t *testing.T) {
	backend := fakeBackend(t)
	dash := httptest.NewServer(newServer(backend.URL).routes())
	t.Cleanup(dash.Close)
	client := dashboardClient(t)

	login(t, client, dash.URL)
	for _, path := range []string{"/aggregates", "/events", "/consumers"} {
		resp, err := client.Get(dash.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(t, resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "DASH.3") {
			t.Fatalf("%s: expected 200 with DASH.3 note, got %s", path, resp.Status)
		}
	}
}

func TestVendoredAssetsServed(t *testing.T) {
	dash := httptest.NewServer(newServer("http://127.0.0.1:1").routes())
	t.Cleanup(dash.Close)
	for _, path := range []string{
		"/assets/app.css",
		"/assets/explorer.js",
		"/assets/webawesome/webawesome.loader.js",
		"/assets/webawesome/styles/themes/default.css",
		"/assets/webawesome/components/input/input.js",
		"/assets/vendor/htmx.min.js",
		"/assets/vendor/cytoscape.min.js",
	} {
		resp, err := dashboardClient(t).Get(dash.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: expected 200, got %s", path, resp.Status)
		}
	}
}

// TestVendoredWebAwesomeHasNoBareImports guards the self-contained
// frontend: the npm `dist/` build of Web Awesome contains bare module
// specifiers ("@shoelace-style/animations", "lit", …) that browsers cannot
// resolve — only the `dist-cdn/` build is importable directly. Scan every
// embedded webawesome JS file so a wrong-tree vendor fails loudly.
func TestVendoredWebAwesomeHasNoBareImports(t *testing.T) {
	bare := regexp.MustCompile(`(?:from\s+["']|import\s*\(\s*["']|import\s+["'])(?:@|lit)`)
	err := fs.WalkDir(assetsFS, "assets/webawesome", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".js") {
			return nil
		}
		raw, err := assetsFS.ReadFile(path)
		if err != nil {
			return err
		}
		if m := bare.Find(raw); m != nil {
			t.Errorf("%s: bare module specifier near %q — vendor from dist-cdn/, not dist/", path, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ---- helpers ----

func login(t *testing.T, client *http.Client, dashURL string) {
	t.Helper()
	resp, err := client.PostForm(dashURL+"/login", map[string][]string{
		"identity": {"admin@example.com"}, "password": {"secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login failed: %s", resp.Status)
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func hasCookie(resp *http.Response, name string) bool {
	for _, c := range resp.Cookies() {
		if c.Name == name && c.Value != "" {
			return true
		}
	}
	return false
}

func hasCookieDeleted(resp *http.Response, name string) bool {
	for _, c := range resp.Cookies() {
		if c.Name == name && c.MaxAge < 0 {
			return true
		}
	}
	return false
}
