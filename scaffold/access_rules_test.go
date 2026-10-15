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

// accessDomain is read-access.json's shape: time entries a manager reads in
// full, a staff member reads their own of, and a project manager also reads
// on the projects they manage (a scope through project managers that
// grants access).
func accessDomain() Domain {
	return Domain{
		Aggregate:    "entry",
		RoleField:    "role",
		SubjectField: "staffId",
		Commands: []Command{{Name: "LogEntry", Once: true,
			Events: []Event{{Name: "EntryLogged", Fields: []Field{{Name: "staffId", Type: "text"}, {Name: "projectId", Type: "text"}}}}}},
		ReadModels: []ReadModel{
			{
				Collection:   "timeEntries",
				Key:          "entryId",
				Fields:       []Field{{Name: "staffId", Type: "text"}, {Name: "projectId", Type: "text"}},
				On:           []string{"EntryLogged"},
				RequiredRole: []string{"manager"},
				SelfAccess:   &SelfAccess{SubjectField: "staffId", Param: "mine"},
				Scopes: []ReadModelScope{{Param: "pmStaffId", GrantsAccess: true, Via: ReadModelScopeVia{
					Collection: "projectManagers", MatchParamTo: "staffId", SelectField: "projectId", FilterLocalField: "projectId"}}},
			},
			{
				Collection:   "projectManagers",
				Key:          "assignmentId",
				Fields:       []Field{{Name: "staffId", Type: "text"}, {Name: "projectId", Type: "text"}},
				On:           []string{"EntryLogged"},
				RequiredRole: []string{"manager"},
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
	people.Fields.Add(&core.TextField{Name: "role"}, &core.TextField{Name: "staffId"})
	if err := app.Save(people); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"projectManagers", "timeEntries"} {
		col := core.NewBaseCollection(name)
		col.Fields.Add(&core.TextField{Name: "key"}, &core.TextField{Name: "staffId"}, &core.TextField{Name: "projectId"})
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
			rec.Set("staffId", r[1])
			rec.Set("projectId", r[2])
			if err := app.Save(rec); err != nil {
				t.Fatal(err)
			}
		}
	}
	// s9 manages p1; s1 manages p3, which has no entries
	insert("projectManagers", [3]string{"a1", "s9", "p1"}, [3]string{"a2", "s1", "p3"})
	insert("timeEntries", [3]string{"e1", "s1", "p1"}, [3]string{"e2", "s2", "p1"}, [3]string{"e3", "s3", "p2"}, [3]string{"e4", "", "p2"})

	tokens := map[string]string{}
	for _, u := range []struct{ name, role, staffID string }{
		{"manager", "manager", "m1"}, {"s1", "staff", "s1"}, {"s2", "staff", "s2"},
		{"s9", "staff", "s9"}, {"nobody", "staff", ""},
	} {
		rec := core.NewRecord(people)
		rec.SetEmail(u.name + "@example.com")
		rec.SetPassword("secret-password")
		rec.Set("role", u.role)
		rec.Set("staffId", u.staffID)
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
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/collections/timeEntries/records?perPage=100", nil)
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
		"manager": "e1,e2,e3,e4", // the role reads every row
		"s1":      "e1",          // own rows; managing p3 admits nothing (the join is per row)
		"s2":      "e2",          // own rows
		"s9":      "e1,e2",       // no own rows; manages p1
		"nobody":  "",            // no subject id: matches nothing, not the subject-less row
		"":        "",            // signed out
	} {
		if got := strings.Join(list(caller), ","); got != want {
			t.Errorf("%q reads %q, want %q (rule: %s)", caller, got, want, rules["timeEntries"])
		}
	}
}
