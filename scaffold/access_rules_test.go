package scaffold

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/types"
)

// accessDomain is read-access.json's shape: tickets a supervisor reads in
// full, an agent reads their own of, and a team lead also reads for the
// teams they lead (a scope through team leads that grants access).
func accessDomain() Domain {
	return Domain{
		Aggregate:    "ticket",
		RoleField:    "role",
		SubjectField: "agentId",
		Commands: []Command{{Name: "OpenTicket", Once: true,
			Events: []Event{{Name: "TicketOpened", Fields: []Field{{Name: "agentId", Type: "text"}, {Name: "teamId", Type: "text"}}}}}},
		ReadModels: []ReadModel{
			{
				Collection:   "tickets",
				Key:          "ticketId",
				Fields:       []Field{{Name: "agentId", Type: "text"}, {Name: "teamId", Type: "text"}},
				On:           []string{"TicketOpened"},
				RequiredRole: []string{"supervisor"},
				SelfAccess:   &SelfAccess{SubjectField: "agentId", Param: "mine"},
				Scopes: []ReadModelScope{{Param: "leadAgentId", GrantsAccess: true, Via: ReadModelScopeVia{
					Collection: "teamLeads", MatchParamTo: "agentId", SelectField: "teamId", FilterLocalField: "teamId"}}},
			},
			{
				Collection:   "teamLeads",
				Key:          "assignmentId",
				Fields:       []Field{{Name: "agentId", Type: "text"}, {Name: "teamId", Type: "text"}},
				On:           []string{"TicketOpened"},
				RequiredRole: []string{"supervisor"},
			},
		},
	}
}

var ruleLine = regexp.MustCompile(`(?m)^//@rule (\S+) (.+)$`)

// TestGeneratedReadRulesRunInPocketBase runs the rules the JS generator
// emits through PocketBase's own record API, as each kind of caller: the
// rule that will actually be enforced, not a description of it.
func TestGeneratedReadRulesRunInPocketBase(t *testing.T) {
	files, err := accessDomain().Generate()
	if err != nil {
		t.Fatal(err)
	}
	rules := map[string]string{}
	for _, f := range files {
		for _, m := range ruleLine.FindAllStringSubmatch(f.Source, -1) {
			rules[m[1]] = m[2]
		}
	}
	if len(rules) != 2 {
		t.Fatalf("want a rule per read model, got %v", rules)
	}

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)

	people := core.NewAuthCollection("people")
	people.Fields.Add(&core.TextField{Name: "role"}, &core.TextField{Name: "agentId"})
	if err := app.Save(people); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"teamLeads", "tickets"} {
		col := core.NewBaseCollection(name)
		col.Fields.Add(&core.TextField{Name: "key"}, &core.TextField{Name: "agentId"}, &core.TextField{Name: "teamId"})
		col.ListRule = types.Pointer(rules[name])
		col.ViewRule = types.Pointer(rules[name])
		if err := app.Save(col); err != nil {
			t.Fatalf("PocketBase refused the generated rule for %s (%q): %v", name, rules[name], err)
		}
	}
	insert := func(collection string, rows ...[3]string) {
		col, _ := app.FindCollectionByNameOrId(collection)
		for _, r := range rows {
			rec := core.NewRecord(col)
			rec.Set("key", r[0])
			rec.Set("agentId", r[1])
			rec.Set("teamId", r[2])
			if err := app.Save(rec); err != nil {
				t.Fatal(err)
			}
		}
	}
	// a9 leads g1; a1 leads g3, which has no tickets
	insert("teamLeads", [3]string{"l1", "a9", "g1"}, [3]string{"l2", "a1", "g3"})
	insert("tickets", [3]string{"t1", "a1", "g1"}, [3]string{"t2", "a2", "g1"}, [3]string{"t3", "a3", "g2"}, [3]string{"t4", "", "g2"})

	tokens := map[string]string{}
	for _, u := range []struct{ name, role, agentID string }{
		{"supervisor", "supervisor", "sup1"}, {"a1", "agent", "a1"}, {"a2", "agent", "a2"},
		{"a9", "agent", "a9"}, {"nobody", "agent", ""},
	} {
		rec := core.NewRecord(people)
		rec.SetEmail(u.name + "@example.com")
		rec.SetPassword("secret-password")
		rec.Set("role", u.role)
		rec.Set("agentId", u.agentID)
		if err := app.Save(rec); err != nil {
			t.Fatal(err)
		}
		tok, err := rec.NewAuthToken()
		if err != nil {
			t.Fatal(err)
		}
		tokens[u.name] = tok
	}

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	mux, err := router.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	list := func(caller string) []string {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/collections/tickets/records?perPage=100", nil)
		if caller != "" {
			req.Header.Set("Authorization", tokens[caller])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var out struct {
			Items []struct {
				Key string `json:"key"`
			} `json:"items"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("%s: %s", caller, body)
		}
		keys := []string{}
		for _, it := range out.Items {
			keys = append(keys, it.Key)
		}
		sort.Strings(keys)
		return keys
	}

	for caller, want := range map[string]string{
		"supervisor": "t1,t2,t3,t4", // the role reads every row
		"a1":         "t1",          // own rows; leading g3 admits nothing (the join is per row)
		"a2":         "t2",          // own rows
		"a9":         "t1,t2",       // no own rows; leads g1
		"nobody":     "",            // no subject id: matches nothing, not the subject-less row
		"":           "",            // signed out
	} {
		if got := strings.Join(list(caller), ","); got != want {
			t.Errorf("%q reads %q, want %q (rule: %s)", caller, got, want, rules["tickets"])
		}
	}
}
