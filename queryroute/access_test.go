package queryroute_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/queryroute"
)

// timeEntries is read-access.json's read model on the Go target: managers
// read every entry, staff their own, and a project manager also those on
// the projects they manage (a granting scope through projectManagers).
var timeEntries = queryroute.Spec{
	Collection:   "timeEntries",
	Key:          "entryId",
	Columns:      []string{"entryId", "staffId", "projectId"},
	RequiredRole: []string{"manager"},
	SelfSubject:  "staffId",
	SelfParam:    "mine",
	Grants: []queryroute.Grant{{
		Collection: "projectManagers", MatchField: "staffId", SelectField: "projectId", LocalField: "projectId"}},
}

func accessFixture(t *testing.T, spec queryroute.Spec) (*httptest.Server, *tests.TestApp, map[string]string) {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	open := ""
	for _, name := range []string{"projectManagers", "timeEntries"} {
		col := core.NewBaseCollection(name)
		col.Fields.Add(&core.TextField{Name: "entryId"}, &core.TextField{Name: "staffId"}, &core.TextField{Name: "projectId"})
		col.ListRule, col.ViewRule = &open, &open // left public by a migration
		if err := app.Save(col); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(collection string, rows ...[3]string) {
		col, _ := app.FindCollectionByNameOrId(collection)
		for _, r := range rows {
			rec := core.NewRecord(col)
			rec.Set("entryId", r[0])
			rec.Set("staffId", r[1])
			rec.Set("projectId", r[2])
			if err := app.Save(rec); err != nil {
				t.Fatal(err)
			}
		}
	}
	insert("projectManagers", [3]string{"a1", "s9", "p1"}, [3]string{"a2", "s1", "p3"})
	insert("timeEntries", [3]string{"e1", "s1", "p1"}, [3]string{"e2", "s2", "p1"}, [3]string{"e3", "s3", "p2"}, [3]string{"e4", "", "p2"})

	people := core.NewAuthCollection("people")
	people.Fields.Add(&core.TextField{Name: "role"}, &core.TextField{Name: "staffId"})
	if err := app.Save(people); err != nil {
		t.Fatal(err)
	}
	tokens := map[string]string{}
	for _, u := range []struct{ name, role, staffID string }{
		{"manager", "manager", "m1"}, {"s1", "staff", "s1"}, {"s2", "staff", "s2"},
		{"s9", "staff", "s9"}, {"nobody", "staff", ""},
	} {
		rec := core.NewRecord(people)
		rec.SetEmail(u.name + "@people.test")
		rec.SetPassword("1234567890")
		rec.Set("role", u.role)
		rec.Set("staffId", u.staffID)
		if err := app.Save(rec); err != nil {
			t.Fatal(err)
		}
		tokens[u.name], _ = rec.NewAuthToken()
	}

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	cfg := queryroute.Config{
		ResolveOwnRole: func(a *core.Record) string { return a.GetString("role") },
		SubjectOf:      func(a *core.Record) string { return a.GetString("staffId") },
	}
	if err := queryroute.Register(&core.ServeEvent{App: app, Router: router}, spec, cfg); err != nil {
		t.Fatal(err)
	}
	mux, err := router.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, app, tokens
}

func keysOf(t *testing.T, srv *httptest.Server, token, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var p struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&p)
	var keys []string
	for _, it := range p.Items {
		if k, ok := it["entryId"].(string); ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return resp.StatusCode, strings.Join(keys, ",")
}

// The whole access rule of schema 3.8.0 on the Go target's route: the role
// reads every row; anyone else their own rows and those a granting scope
// admits for them (one projectManagers row must match both: s1 manages p3,
// which has no entries); the param narrows anyone to their own rows; and
// the record API is locked so the route is the only way to read.
func TestDeclaredAccessOnTheReadRoute(t *testing.T) {
	srv, _, tokens := accessFixture(t, timeEntries)
	path := queryroute.PathPrefix + "timeEntries"
	for _, c := range []struct {
		who, query string
		status     int
		want       string
	}{
		{"manager", "", 200, "e1,e2,e3,e4"},
		{"s1", "", 200, "e1"},
		{"s2", "", 200, "e2"},
		{"s9", "", 200, "e1,e2"},
		{"nobody", "", 403, ""},
		{"s9", "?mine=1", 200, ""},
		{"manager", "?mine=1", 200, ""},
		{"s1", "?mine=1", 200, "e1"},
	} {
		status, got := keysOf(t, srv, tokens[c.who], path+c.query)
		if status != c.status || got != c.want {
			t.Errorf("%s%s: got %d %q, want %d %q", c.who, c.query, status, got, c.status, c.want)
		}
	}
	if status, got := keysOf(t, srv, tokens["s2"], "/api/collections/timeEntries/records"); got != "" || status == 200 && got != "" {
		t.Errorf("the record API must be locked when access is declared: %d %q", status, got)
	}
}

// A route whose access depends on the caller's subject id can't work
// without the hook that resolves it, so registering it fails rather than
// serving it more (or less) openly than declared.
func TestSubjectRulesNeedTheSubjectHook(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	col := core.NewBaseCollection("timeEntries")
	col.Fields.Add(&core.TextField{Name: "entryId"}, &core.TextField{Name: "staffId"}, &core.TextField{Name: "projectId"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	router, _ := apis.NewRouter(app)
	cfg := queryroute.Config{ResolveOwnRole: func(*core.Record) string { return "" }}
	if err := queryroute.Register(&core.ServeEvent{App: app, Router: router}, timeEntries, cfg); err == nil {
		t.Fatal("registered a route with self/grant access and no SubjectOf hook")
	}
}
