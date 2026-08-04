package main

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// sampleCatalog mirrors the public GET /api/cqrs/catalog JSON. maxPosition
// sits above one consumer's checkpoint on purpose: behind-by is measured
// against the head of the log, and a fixture where every consumer is caught
// up would never exercise that path.
const sampleCatalog = `{
  "generatedAt": "2026-08-03 10:00:00.000Z",
  "mode": "running",
  "totals": {"events": 42, "maxPosition": 50, "streams": 7, "deadLettersPending": 2},
  "aggregates": [
    {"name": "task", "origin": "go", "streams": 3,
     "events": [{"type": "TaskCreated", "count": 5, "minVersion": 1, "maxVersion": 1}]}
  ],
  "consumers": [
    {"name": "tasks", "kind": "go-projection", "eventTypes": ["TaskCreated"],
     "collections": ["tasks"], "checkpoint": 50},
    {"name": "fulfillment", "kind": "reactor", "eventTypes": ["TaskCreated"],
     "checkpoint": 44}
  ],
  "collections": [
    {"name": "tasks", "guarded": true, "owner": "tasks", "fields": ["title:text"]}
  ],
  "functions": {"http": ["hello"], "cron": [{"name": "heartbeat", "schedule": "* * * * *"}]},
  "flows": [],
  "mermaid": "flowchart LR\n  agg_task([\"task\"])\n"
}`

// sampleLog is the fake event log the feed routes page through. Positions
// are deliberately non-contiguous: position is AUTOINCREMENT and rolled-back
// conflict retries burn values, so nothing may assume position == index.
var sampleLog = []Event{
	{Position: 1, ID: "e1", Aggregate: "task", AggregateID: "t1", Sequence: 1, Type: "TaskCreated",
		Data: json.RawMessage(`{"title":"first"}`), Metadata: json.RawMessage(`{"actor":"u1"}`), Version: 1, Created: "2026-08-03 10:00:00.000Z"},
	{Position: 3, ID: "e2", Aggregate: "task", AggregateID: "t1", Sequence: 2, Type: "TaskCompleted",
		Data: json.RawMessage(`{}`), Metadata: json.RawMessage(`{"actor":"u1"}`), Version: 1, Created: "2026-08-03 10:01:00.000Z"},
	{Position: 4, ID: "e3", Aggregate: "task", AggregateID: "t2", Sequence: 1, Type: "TaskCreated",
		Data: json.RawMessage(`{"title":"second"}`), Version: 1, Created: "2026-08-03 10:02:00.000Z"},
	{Position: 7, ID: "e4", Aggregate: "order", AggregateID: "o1", Sequence: 1, Type: "OrderPlaced",
		Data: json.RawMessage(`{"customer":"c1"}`), Metadata: json.RawMessage(`{"actor":"reactor:fulfillment"}`), Version: 2, Created: "2026-08-03 10:03:00.000Z"},
}

// queryLog applies the feed's filters the way the real endpoint does:
// ascending results either way, and `before` takes its batch off the top.
func queryLog(q url.Values) []Event {
	var out []Event
	for _, e := range sampleLog {
		if a := q.Get("aggregate"); a != "" && e.Aggregate != a {
			continue
		}
		if id := q.Get("aggregateId"); id != "" && e.AggregateID != id {
			continue
		}
		if ty := q.Get("type"); ty != "" && e.Type != ty {
			continue
		}
		if after, _ := strconv.ParseInt(q.Get("after"), 10, 64); after > 0 && e.Position <= after {
			continue
		}
		if before, _ := strconv.ParseInt(q.Get("before"), 10, 64); before > 0 && e.Position >= before {
			continue
		}
		out = append(out, e)
	}
	if limit, _ := strconv.Atoi(q.Get("limit")); limit > 0 && len(out) > limit {
		if q.Get("before") != "" {
			out = out[len(out)-limit:] // walking backwards: keep the newest
		} else {
			out = out[:limit]
		}
	}
	return out
}

// fakeBackend implements the public endpoints the dashboard consumes.
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
	// every operational route is superuser-scoped, exactly like the real API
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "tok123" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	// the admin barrier: a real mode value the tests can move. The catalog
	// reports it too, like the real one — a fixture pinned to "running"
	// would let a broken mode banner pass.
	mode := "running"

	// the catalog reports the live mode, like the real one: a fixture pinned
	// to "running" would let a broken mode banner pass
	mux.HandleFunc("GET /api/cqrs/catalog", authed(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Replace(sampleCatalog, `"mode": "running"`, `"mode": "`+mode+`"`, 1)))
	}))
	mux.HandleFunc("GET /api/cqrs/streams", authed(func(w http.ResponseWriter, r *http.Request) {
		streams := []StreamInfo{
			{Aggregate: "task", AggregateID: "t1", Events: 2, LastPosition: 3, Updated: "2026-08-03 10:01:00.000Z"},
			{Aggregate: "task", AggregateID: "t2", Events: 1, LastPosition: 4, Updated: "2026-08-03 10:02:00.000Z"},
			{Aggregate: "order", AggregateID: "o1", Events: 1, LastPosition: 7, Updated: "2026-08-03 10:03:00.000Z"},
		}
		if agg := r.URL.Query().Get("aggregate"); agg != "" {
			kept := streams[:0:0]
			for _, s := range streams {
				if s.Aggregate == agg {
					kept = append(kept, s)
				}
			}
			streams = kept
		}
		json.NewEncoder(w).Encode(map[string]any{"streams": streams})
	}))
	mux.HandleFunc("GET /api/cqrs/events", authed(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"events": queryLog(r.URL.Query())})
	}))
	mux.HandleFunc("GET /api/cqrs/deadletters", authed(func(w http.ResponseWriter, r *http.Request) {
		letters := []DeadLetter{{
			ID: 9, Consumer: "fn:task_audit.js", EventPos: 3, Event: sampleLog[1],
			Error: "boom: audit sink refused", Attempts: 2,
			FirstFailed: "2026-08-03 10:01:05.000Z", LastFailed: "2026-08-03 10:01:30.000Z",
		}}
		if r.URL.Query().Get("all") == "1" {
			letters = append(letters, DeadLetter{
				ID: 4, Consumer: "fn:task_audit.js", EventPos: 1, Event: sampleLog[0],
				Error: "boom: earlier failure", Attempts: 1,
				FirstFailed: "2026-08-03 09:00:00.000Z", LastFailed: "2026-08-03 09:00:00.000Z",
				Resolved: true,
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"deadLetters": letters})
	}))
	mux.HandleFunc("GET /api/cqrs/admin/mode", authed(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"mode": mode})
	}))
	mux.HandleFunc("POST /api/cqrs/admin/mode", authed(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Mode string `json:"mode"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Mode != "running" && body.Mode != "maintenance" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"message":"invalid mode"}`))
			return
		}
		mode = body.Mode
		json.NewEncoder(w).Encode(map[string]string{"mode": mode})
	}))
	mux.HandleFunc("POST /api/cqrs/admin/reload", authed(func(w http.ResponseWriter, r *http.Request) {
		schemaTier := "skipped: not in maintenance"
		rep := map[string]any{
			"effectsReloaded": []string{"fn:task_audit.js"},
			"httpReloaded":    []string{"hello"},
			"cronReloaded":    []string{"heartbeat.js"},
		}
		if mode == "maintenance" {
			schemaTier = "reloaded"
			rep["projectionsReloaded"] = []string{"notes"}
			rep["decidersReloaded"] = []string{"note"}
			rep["decidersRefused"] = []string{"broken (evolve threw on stream t1)"}
		}
		rep["mode"] = mode
		rep["schemaTier"] = schemaTier
		json.NewEncoder(w).Encode(rep)
	}))

	// dead-letter actions; #9 is poison and stays poison, #4 retries clean
	mux.HandleFunc("POST /api/cqrs/deadletters/{id}/retry", authed(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "9" {
			json.NewEncoder(w).Encode(DeadLetterResult{ID: 9, Consumer: "fn:task_audit.js",
				Resolved: false, Attempts: 3, Error: "boom: audit sink refused"})
			return
		}
		json.NewEncoder(w).Encode(DeadLetterResult{ID: 4, Consumer: "fn:task_audit.js", Resolved: true})
	}))
	mux.HandleFunc("POST /api/cqrs/deadletters/retry", authed(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"results": []DeadLetterResult{
			{ID: 9, Resolved: false, Attempts: 3, Error: "boom: audit sink refused"},
			{ID: 4, Resolved: true},
		}})
	}))
	mux.HandleFunc("POST /api/cqrs/deadletters/{id}/dismiss", authed(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": r.PathValue("id"), "resolved": true})
	}))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// htmxGet issues a request the way htmx does, so handlers take their
// fragment branch.
func htmxRequest(t *testing.T, client *http.Client, method, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("HX-Request", "true")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
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
		"Running.",          // mode banner
		"Events in log",     // totals cards
		">42</strong>",      // events total
		"stat-danger",       // pending dead letters highlighted
		`id="explorer"`,     // cytoscape container
		`id="catalog-data"`, // embedded catalog JSON
		"flowchart LR",      // mermaid source panel
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

// TestBrowsingPages walks the drill-down (aggregates -> streams -> events),
// the log browser and the consumers page against the fake backend.
func TestBrowsingPages(t *testing.T) {
	backend := fakeBackend(t)
	dash := httptest.NewServer(newServer(backend.URL).routes())
	t.Cleanup(dash.Close)
	client := dashboardClient(t)
	login(t, client, dash.URL)

	for _, tc := range []struct {
		path string
		want []string
	}{
		{"/aggregates", []string{
			">task<",                  // the aggregate, linked
			`href="/aggregates/task"`, // drill-down target
			">go<",                    // origin tag
			"TaskCreated &times;5",    // empirical event type and count
		}},
		{"/aggregates/task", []string{
			`href="/aggregates/task/t1"`, // stream drill-down
			`href="/aggregates/task/t2"`,
			">3<", // t1's last position
		}},
		{"/aggregates/task/t1", []string{
			"postit postit-event", // the timeline notation
			"TaskCreated",
			"TaskCompleted",
			"&#34;title&#34;: &#34;first&#34;", // indented payload, html-escaped
			"u1",                               // actor from the metadata
		}},
		{"/events", []string{
			"OrderPlaced",                 // the whole log, not one stream
			`href="/aggregates/order/o1"`, // stream link from a feed row
			"reactor:fulfillment",         // actor column
			"All aggregates",              // filter placeholder
		}},
		{"/events?aggregate=order", []string{"OrderPlaced"}},
		{"/consumers", []string{
			"caught up",                // tasks is at maxPosition
			"6 to go",                  // fulfillment trails it by 50-44
			"fn:task_audit.js",         // dead letter
			"boom: audit sink refused", // its error
			"pending",
			"?all=1", // the resolved-letters toggle
		}},
		{"/consumers?all=1", []string{"resolved", "boom: earlier failure", "Pending only"}},
	} {
		resp, err := client.Get(dash.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(t, resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: expected 200, got %s", tc.path, resp.Status)
		}
		for _, want := range tc.want {
			if !strings.Contains(body, want) {
				t.Errorf("%s: missing %q", tc.path, want)
			}
		}
	}
}

// TestEventsFilteredOut proves the empty state renders rather than a broken
// table when nothing matches.
func TestEventsFilteredOut(t *testing.T) {
	backend := fakeBackend(t)
	dash := httptest.NewServer(newServer(backend.URL).routes())
	t.Cleanup(dash.Close)
	client := dashboardClient(t)
	login(t, client, dash.URL)

	resp, err := client.Get(dash.URL + "/events?type=NoSuchEvent")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if !strings.Contains(body, "No events match this filter") {
		t.Fatalf("expected the empty state:\n%s", body)
	}
}

// TestFeedPagination walks the feed forwards and back through the rendered
// pager links, which is the only thing that exercises `before` end to end.
func TestFeedPagination(t *testing.T) {
	backend := fakeBackend(t)
	dash := httptest.NewServer(newServer(backend.URL).routes())
	t.Cleanup(dash.Close)
	client := dashboardClient(t)
	login(t, client, dash.URL)

	// two per page: the first page is the oldest two events, with a Newer
	// link but no Older one (there is nothing before the start of the log)
	body := get(t, client, dash.URL+"/events?limit=2")
	if !strings.Contains(body, "TaskCreated") || strings.Contains(body, "OrderPlaced") {
		t.Fatalf("first page should hold the two oldest events:\n%s", body)
	}
	newer := hrefAfter(t, body, "after=3")
	if newer == "" {
		t.Fatalf("expected a Newer link carrying after=3:\n%s", body)
	}

	body = get(t, client, dash.URL+newer)
	if !strings.Contains(body, "OrderPlaced") {
		t.Fatalf("second page should hold the newest events:\n%s", body)
	}
	older := hrefAfter(t, body, "before=4")
	if older == "" {
		t.Fatalf("expected an Older link carrying before=4:\n%s", body)
	}

	// paging back lands on the batch that ends where we started
	body = get(t, client, dash.URL+older)
	if !strings.Contains(body, "TaskCompleted") || strings.Contains(body, "OrderPlaced") {
		t.Fatalf("paging back should return the earlier batch:\n%s", body)
	}
}

// TestPaginateBounds pins the pager rules directly: the first page has no
// Older, a short batch exhausts its direction, and paging backwards drops
// After (it was only ever a floor guard).
func TestPaginateBounds(t *testing.T) {
	page := []Event{{Position: 10}, {Position: 20}}
	for _, tc := range []struct {
		name                 string
		filter               EventFilter
		evs                  []Event
		wantOlder, wantNewer bool
	}{
		{"empty page has neither", EventFilter{Limit: 2}, nil, false, false},
		{"first full page offers only newer", EventFilter{Limit: 2}, page, false, true},
		{"first short page offers neither", EventFilter{Limit: 5}, page, false, false},
		{"forward page always offers older", EventFilter{After: 5, Limit: 5}, page, true, false},
		{"backward page always offers newer", EventFilter{Before: 30, Limit: 5}, page, false, true},
		{"full backward page offers both", EventFilter{Before: 30, Limit: 2}, page, true, true},
	} {
		older, newer := paginate("/events", tc.filter, tc.evs)
		if (older != "") != tc.wantOlder || (newer != "") != tc.wantNewer {
			t.Errorf("%s: older=%q newer=%q", tc.name, older, newer)
		}
	}
	// walking back drops After but keeps the filters
	older, _ := paginate("/events", EventFilter{Aggregate: "task", After: 5, Limit: 5}, page)
	if strings.Contains(older, "after=") || !strings.Contains(older, "aggregate=task") {
		t.Errorf("Older link should drop after= and keep the filter: %q", older)
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

// ---- DASH.4: actions and live fragments ----

// TestModeToggle drives the barrier from the UI: the page offers the step
// that comes next, and the toggle is POST-then-redirect so a refresh cannot
// re-fire it.
func TestModeToggle(t *testing.T) {
	backend := fakeBackend(t)
	dash := httptest.NewServer(newServer(backend.URL).routes())
	t.Cleanup(dash.Close)
	client := dashboardClient(t)
	login(t, client, dash.URL)

	body := get(t, client, dash.URL+"/system")
	for _, want := range []string{"Running.", `value="maintenance"`, "Enter maintenance",
		"Reload effect functions", "Enter maintenance first"} {
		if !strings.Contains(body, want) {
			t.Errorf("running system page missing %q", want)
		}
	}

	resp, err := client.PostForm(dash.URL+"/system/mode", url.Values{"mode": {"maintenance"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/system" {
		t.Fatalf("expected 303 to /system, got %s", resp.Status)
	}

	body = get(t, client, dash.URL+"/system")
	for _, want := range []string{"Maintenance mode.", `value="running"`, "Return to running",
		"Reload all tiers"} {
		if !strings.Contains(body, want) {
			t.Errorf("maintenance system page missing %q", want)
		}
	}
	// the overview banner follows the same barrier
	if !strings.Contains(get(t, client, dash.URL+"/"), "Maintenance mode.") {
		t.Error("overview did not pick up maintenance mode")
	}
}

// TestModeRefused: the barrier is the safety-critical control here, so a
// value the backend rejects must be reported as a refused TOGGLE naming the
// value — not as a generic backend error an operator could read as "the
// system may or may not have moved".
func TestModeRefused(t *testing.T) {
	backend := fakeBackend(t)
	dash := httptest.NewServer(newServer(backend.URL).routes())
	t.Cleanup(dash.Close)
	client := dashboardClient(t)
	login(t, client, dash.URL)

	resp, err := client.PostForm(dash.URL+"/system/mode", url.Values{"mode": {"bogus"}})
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a refused mode, got %s", resp.Status)
	}
	// the rejected value is quoted by %q and then HTML-escaped, which is the
	// point: it is untrusted input echoed back to the operator
	for _, want := range []string{"The mode was not changed", "&#34;bogus&#34;", "still running"} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal notice missing %q", want)
		}
	}
	// and the system really did not move
	if !strings.Contains(get(t, client, dash.URL+"/system"), "Running.") {
		t.Error("the mode changed despite the refusal")
	}
}

// TestDeadLetterActionFailureReported: a backend that fails the action must
// say so in the panel rather than silently swapping an unchanged table.
func TestDeadLetterActionFailureReported(t *testing.T) {
	backend := fakeBackend(t)
	dash := httptest.NewServer(newServer(backend.URL).routes())
	t.Cleanup(dash.Close)
	client := dashboardClient(t)
	login(t, client, dash.URL)
	backend.Close()

	for _, path := range []string{"/consumers/deadletters/9/retry", "/consumers/deadletters/retry"} {
		resp := htmxRequest(t, client, http.MethodPost, dash.URL+path)
		body := readBody(t, resp)
		resp.Body.Close()
		if !strings.Contains(body, "The action failed") {
			t.Errorf("%s: a failed action was not reported to the operator", path)
		}
	}
}

// TestReloadReport checks both delivery paths of the reload report: the
// report is rendered, never redirected away.
func TestReloadReport(t *testing.T) {
	backend := fakeBackend(t)
	dash := httptest.NewServer(newServer(backend.URL).routes())
	t.Cleanup(dash.Close)
	client := dashboardClient(t)
	login(t, client, dash.URL)

	// running: the schema tier is skipped, and the page says so
	resp := htmxRequest(t, client, http.MethodPost, dash.URL+"/system/reload")
	body := readBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %s", resp.Status)
	}
	if strings.Contains(strings.ToLower(body), "<!doctype") {
		t.Error("htmx reload returned a whole page instead of a fragment")
	}
	if resp.Header.Get("Vary") != "HX-Request" {
		t.Errorf("branching on HX-Request without Vary: got %q", resp.Header.Get("Vary"))
	}
	for _, want := range []string{`id="reload-report"`, "Effect tier only", "fn:task_audit.js", "skipped: not in maintenance"} {
		if !strings.Contains(body, want) {
			t.Errorf("reload fragment missing %q", want)
		}
	}

	// without htmx the same POST re-renders the whole page WITH the report
	resp, err := client.PostForm(dash.URL+"/system/reload", nil)
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with the report inline, got %s", resp.Status)
	}
	if !strings.Contains(strings.ToLower(body), "<!doctype") || !strings.Contains(body, `id="reload-report"`) {
		t.Error("non-htmx reload should re-render the full page with the report")
	}

	// in maintenance both tiers move, and a refused decider is reported
	if _, err := client.PostForm(dash.URL+"/system/mode", url.Values{"mode": {"maintenance"}}); err != nil {
		t.Fatal(err)
	}
	resp = htmxRequest(t, client, http.MethodPost, dash.URL+"/system/reload")
	body = readBody(t, resp)
	resp.Body.Close()
	for _, want := range []string{"Reloaded behind the barrier", "Deciders refused", "broken (evolve threw", ">notes<"} {
		if !strings.Contains(body, want) {
			t.Errorf("maintenance reload report missing %q", want)
		}
	}
}

// TestDeadLetterActions covers retry, retry-all and dismiss on both paths.
// A retry that fails again is a RESULT, not an error: the panel comes back
// with the failure reported and the row still pending.
func TestDeadLetterActions(t *testing.T) {
	backend := fakeBackend(t)
	dash := httptest.NewServer(newServer(backend.URL).routes())
	t.Cleanup(dash.Close)
	client := dashboardClient(t)
	login(t, client, dash.URL)

	// still-poison retry, via htmx
	resp := htmxRequest(t, client, http.MethodPost, dash.URL+"/consumers/deadletters/9/retry")
	body := readBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %s", resp.Status)
	}
	if strings.Contains(strings.ToLower(body), "<!doctype") {
		t.Error("dead-letter action returned a page instead of the panel fragment")
	}
	for _, want := range []string{`id="deadletters-panel"`, "still failing after 3 attempts", "boom: audit sink refused"} {
		if !strings.Contains(body, want) {
			t.Errorf("retry panel missing %q", want)
		}
	}

	// a clean retry reports resolution
	resp = htmxRequest(t, client, http.MethodPost, dash.URL+"/consumers/deadletters/4/retry")
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "re-delivered and resolved") {
		t.Error("successful retry not reported")
	}

	// retry-all summarises a mixed batch
	resp = htmxRequest(t, client, http.MethodPost, dash.URL+"/consumers/deadletters/retry")
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "1 resolved, 1 still failing") {
		t.Errorf("retry-all summary missing: %s", body)
	}

	// dismiss, and the filter carries through the swap
	resp = htmxRequest(t, client, http.MethodPost, dash.URL+"/consumers/deadletters/9/dismiss?all=1")
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "dismissed without retrying") {
		t.Error("dismiss not reported")
	}
	if !strings.Contains(body, "Pending only") {
		t.Error("the ?all=1 filter was lost across the swap")
	}

	// without htmx the same action posts and redirects back to the page
	resp, err := client.PostForm(dash.URL+"/consumers/deadletters/9/retry", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/consumers" {
		t.Fatalf("expected a 303 back to /consumers, got %s", resp.Status)
	}
}

// TestLiveFragments checks the polling contract: pages carry a trigger, the
// fragments are fragments, and each one renders its OWN trigger — which is
// what keeps the loop alive, since htmx 4 has no stop-polling status code.
func TestLiveFragments(t *testing.T) {
	backend := fakeBackend(t)
	dash := httptest.NewServer(newServer(backend.URL).routes())
	t.Cleanup(dash.Close)
	client := dashboardClient(t)
	login(t, client, dash.URL)

	for _, tc := range []struct{ page, fragment, marker string }{
		{"/", "/fragments/overview", `id="overview-live"`},
		{"/consumers", "/fragments/checkpoints", `id="checkpoints-body"`},
	} {
		page := get(t, client, dash.URL+tc.page)
		if !strings.Contains(page, `hx-get="`+tc.fragment+`"`) || !strings.Contains(page, `hx-trigger="`+livePoll+`"`) {
			t.Errorf("%s does not poll %s", tc.page, tc.fragment)
		}

		body := get(t, client, dash.URL+tc.fragment)
		if strings.Contains(strings.ToLower(body), "<!doctype") {
			t.Errorf("%s returned a whole page", tc.fragment)
		}
		if !strings.Contains(body, tc.marker) {
			t.Errorf("%s missing %q", tc.fragment, tc.marker)
		}
		if !strings.Contains(body, `hx-trigger="`+livePoll+`"`) {
			t.Errorf("%s dropped its own trigger — polling would stop after one tick", tc.fragment)
		}
	}

	// the checkpoints fragment carries its out-of-band riders; the page must
	// NOT (inside a <table> they are invalid markup)
	frag := get(t, client, dash.URL+"/fragments/checkpoints")
	for _, want := range []string{`id="log-head" hx-swap-oob="true"`, `id="dl-pending" hx-swap-oob="true"`, "6 to go"} {
		if !strings.Contains(frag, want) {
			t.Errorf("checkpoints fragment missing %q", want)
		}
	}
	if strings.Contains(get(t, client, dash.URL+"/consumers"), "hx-swap-oob") {
		t.Error("the page rendered out-of-band riders inside its own table")
	}

	// the log browser deliberately does not poll: an unbounded page is the
	// START of the log, so a timer would re-fetch the same rows forever
	if strings.Contains(get(t, client, dash.URL+"/events"), "hx-trigger") {
		t.Error("the log browser should not poll")
	}
}

// TestFragmentExpiredSessionRedirectsBrowser is the defect this design
// exists to avoid: under a 2s poll, a 303 to /login would be followed by
// htmx's fetch and the whole sign-in page swapped into a table body, every
// two seconds. htmx honours an HX-Redirect response header instead.
func TestFragmentExpiredSessionRedirectsBrowser(t *testing.T) {
	backend := fakeBackend(t)
	dash := httptest.NewServer(newServer(backend.URL).routes())
	t.Cleanup(dash.Close)
	client := dashboardClient(t)

	for _, path := range []string{"/fragments/checkpoints", "/fragments/overview", "/fragments/deadletters"} {
		req, _ := http.NewRequest(http.MethodGet, dash.URL+path, nil)
		req.Header.Set("HX-Request", "true")
		req.AddCookie(&http.Cookie{Name: authCookieName, Value: "stale"})
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(t, resp)
		resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: expected 401, got %s", path, resp.Status)
		}
		if resp.Header.Get("HX-Redirect") != "/login" {
			t.Errorf("%s: expected HX-Redirect to /login, got %q", path, resp.Header.Get("HX-Redirect"))
		}
		if strings.Contains(body, "<form") {
			t.Errorf("%s: a sign-in page was returned to a fragment request", path)
		}
	}

	// a plain navigation still gets the ordinary redirect
	req, _ := http.NewRequest(http.MethodGet, dash.URL+"/fragments/checkpoints", nil)
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: "stale"})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("expected a 303 for a non-htmx request, got %s", resp.Status)
	}
}

// TestFragmentBackendOutageKeepsPolling: a backend that is down must not
// stop the loop, or the panel stays dead after the backend recovers.
func TestFragmentBackendOutageKeepsPolling(t *testing.T) {
	backend := fakeBackend(t)
	dash := httptest.NewServer(newServer(backend.URL).routes())
	t.Cleanup(dash.Close)
	client := dashboardClient(t)
	login(t, client, dash.URL)
	backend.Close() // the backend goes away mid-session

	for _, tc := range []struct{ path, marker string }{
		{"/fragments/checkpoints", `id="checkpoints-body"`},
		{"/fragments/overview", `id="overview-live"`},
	} {
		body := get(t, client, dash.URL+tc.path)
		if !strings.Contains(body, tc.marker) || !strings.Contains(body, `hx-trigger="`+livePoll+`"`) {
			t.Errorf("%s: an outage dropped the polling trigger", tc.path)
		}
		if !strings.Contains(body, "Could not reach the pocketcqrs backend") {
			t.Errorf("%s: the outage is not reported in the fragment", tc.path)
		}
	}
}

func get(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: expected 200, got %s", url, resp.Status)
	}
	return readBody(t, resp)
}

// hrefAfter returns the pager href containing the given query fragment.
func hrefAfter(t *testing.T, body, fragment string) string {
	t.Helper()
	re := regexp.MustCompile(`href="(/events\?[^"]*` + regexp.QuoteMeta(fragment) + `[^"]*)"`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.ReplaceAll(m[1], "&amp;", "&")
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
