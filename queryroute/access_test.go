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

// tickets is read-access.json's read model on the Go target: supervisors
// read every ticket, agents their own, and a team lead also those of the
// teams they lead (a granting scope through teamLeads).
var tickets = queryroute.Spec{
	Collection:   "tickets",
	Key:          "ticketId",
	Columns:      []string{"ticketId", "agentId", "teamId"},
	RequiredRole: []string{"supervisor"},
	SelfSubject:  "agentId",
	SelfParam:    "mine",
	Grants: []queryroute.Grant{{
		Collection: "teamLeads", MatchField: "agentId", SelectField: "teamId", LocalField: "teamId"}},
}

func accessFixture(t *testing.T, spec queryroute.Spec) (*httptest.Server, *tests.TestApp, map[string]string) {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	open := ""
	for _, name := range []string{"teamLeads", "tickets"} {
		col := core.NewBaseCollection(name)
		col.Fields.Add(&core.TextField{Name: "ticketId"}, &core.TextField{Name: "agentId"}, &core.TextField{Name: "teamId"})
		col.ListRule, col.ViewRule = &open, &open // left public by a migration
		if err := app.Save(col); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(collection string, rows ...[3]string) {
		col, _ := app.FindCollectionByNameOrId(collection)
		for _, r := range rows {
			rec := core.NewRecord(col)
			rec.Set("ticketId", r[0])
			rec.Set("agentId", r[1])
			rec.Set("teamId", r[2])
			if err := app.Save(rec); err != nil {
				t.Fatal(err)
			}
		}
	}
	insert("teamLeads", [3]string{"l1", "a9", "g1"}, [3]string{"l2", "a1", "g3"})
	insert("tickets", [3]string{"t1", "a1", "g1"}, [3]string{"t2", "a2", "g1"}, [3]string{"t3", "a3", "g2"}, [3]string{"t4", "", "g2"})

	people := core.NewAuthCollection("people")
	people.Fields.Add(&core.TextField{Name: "role"}, &core.TextField{Name: "agentId"})
	if err := app.Save(people); err != nil {
		t.Fatal(err)
	}
	tokens := map[string]string{}
	for _, u := range []struct{ name, role, agentID string }{
		{"supervisor", "supervisor", "sup1"}, {"a1", "agent", "a1"}, {"a2", "agent", "a2"},
		{"a9", "agent", "a9"}, {"nobody", "agent", ""},
	} {
		rec := core.NewRecord(people)
		rec.SetEmail(u.name + "@people.test")
		rec.SetPassword("1234567890")
		rec.Set("role", u.role)
		rec.Set("agentId", u.agentID)
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
		SubjectOf:      func(a *core.Record) string { return a.GetString("agentId") },
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
		if k, ok := it["ticketId"].(string); ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return resp.StatusCode, strings.Join(keys, ",")
}

// The whole access rule of schema 3.8.0 on the Go target's route: the role
// reads every row; anyone else their own rows and those a granting scope
// admits for them (one teamLeads row must match both: a1 leads g3,
// which has no tickets); the param narrows anyone to their own rows; and
// the record API is locked so the route is the only way to read.
func TestDeclaredAccessOnTheReadRoute(t *testing.T) {
	srv, _, tokens := accessFixture(t, tickets)
	path := queryroute.PathPrefix + "tickets"
	for _, c := range []struct {
		who, query string
		status     int
		want       string
	}{
		{"supervisor", "", 200, "t1,t2,t3,t4"},
		{"a1", "", 200, "t1"},
		{"a2", "", 200, "t2"},
		{"a9", "", 200, "t1,t2"},
		{"nobody", "", 403, ""},
		{"a9", "?mine=1", 200, ""},
		{"supervisor", "?mine=1", 200, ""},
		{"a1", "?mine=1", 200, "t1"},
	} {
		status, got := keysOf(t, srv, tokens[c.who], path+c.query)
		if status != c.status || got != c.want {
			t.Errorf("%s%s: got %d %q, want %d %q", c.who, c.query, status, got, c.status, c.want)
		}
	}
	if status, got := keysOf(t, srv, tokens["a2"], "/api/collections/tickets/records"); got != "" || status == 200 && got != "" {
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
	col := core.NewBaseCollection("tickets")
	col.Fields.Add(&core.TextField{Name: "ticketId"}, &core.TextField{Name: "agentId"}, &core.TextField{Name: "teamId"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	router, _ := apis.NewRouter(app)
	cfg := queryroute.Config{ResolveOwnRole: func(*core.Record) string { return "" }}
	if err := queryroute.Register(&core.ServeEvent{App: app, Router: router}, tickets, cfg); err == nil {
		t.Fatal("registered a route with self/grant access and no SubjectOf hook")
	}
}
