package queryroute_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/queryroute"
)

// post sends a read-model query as POST, its terms in a JSON body.
func (f *fixture) post(t *testing.T, who, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if who != "" {
		req.Header.Set("Authorization", f.tokens[who])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// samePage asserts a POST answers exactly what the GET with the same terms
// answers: status and body.
func samePage(t *testing.T, f *fixture, who, query, body string) {
	t.Helper()
	gs, gb := f.get(t, who, "/api/cqrs/rm/customers"+query)
	ps, pb := f.post(t, who, "/api/cqrs/rm/customers", body)
	if gs != ps || gb != pb {
		t.Errorf("%s as %s: GET %s gave %d %s; POST %s gave %d %s", query, who, query, gs, gb, body, ps, pb)
	}
}

func TestPostAnswersWhatGetAnswers(t *testing.T) {
	f := setup(t, nil, nil)
	for _, c := range [][2]string{
		{"", `{}`},
		{"?name=Bob&sort=-name", `{"name":"Bob","sort":"-name"}`},
		{"?perPage=2&page=2", `{"perPage":2,"page":2}`},
		{"?perPage=2&page=2", `{"perPage":"2","page":"2"}`},
		{"?customerId=c2", `{"customerId":"c2"}`},
		// the same refusals, with the same messages
		{"?email=alice@example.com", `{"email":"alice@example.com"}`},
		{"?sort=email", `{"sort":"email"}`},
		{"?nope=1", `{"nope":1}`},
		{"?perPage=0", `{"perPage":0}`},
	} {
		samePage(t, f, "agent", c[0], c[1])
	}
	// self-access: alice sees her own row either way, and a filter can't widen it
	samePage(t, f, "alice", "", `{}`)
	samePage(t, f, "alice", "?customerId=c2", `{"customerId":"c2"}`)
	if status, body := f.post(t, "agent", "/api/cqrs/rm/customers", ""); status != 200 || len(items(t, body)) != 3 {
		t.Fatalf("an empty body is no terms, as on the command route: %d %s", status, body)
	}
}

func TestPostSearchesAPersonalFieldLikeGet(t *testing.T) {
	f := searchSetup(t, nil)
	for _, c := range [][2]string{
		{"?email=ALICE@Example.com", `{"email":"ALICE@Example.com"}`},
		{"?name=al", `{"name":"al"}`},
		{"?name=al&email=alan@example.com", `{"name":"al","email":"alan@example.com"}`},
		{"?email=nobody@example.com", `{"email":"nobody@example.com"}`},
		{"?name=a", `{"name":"a"}`},
	} {
		samePage(t, f, "agent", c[0], c[1])
	}
	samePage(t, f, "alice", "?name=al", `{"name":"al"}`)
	status, body := f.post(t, "agent", "/api/cqrs/rm/customers", `{"email":"ALICE@Example.com"}`)
	if rows := items(t, body); status != 200 || len(rows) != 1 || rows["c1"]["email"] != "alice@example.com" {
		t.Fatalf("a hashed search by POST finds the row and reveals it: %d %s", status, body)
	}
}

func TestPostRefusesWhoGetRefuses(t *testing.T) {
	f := setup(t, nil, nil)
	if status, body := f.post(t, "nobody", "/api/cqrs/rm/customers", `{}`); status != 403 {
		t.Fatalf("no role, no subject: want 403, got %d %s", status, body)
	}
	if status, _ := f.post(t, "", "/api/cqrs/rm/customers", `{}`); status != 401 {
		t.Fatalf("an anonymous caller must authenticate, got %d", status)
	}
	if status, body := f.post(t, "root", "/api/cqrs/rm/customers", `{}`); status != 200 || len(items(t, body)) != 3 {
		t.Fatalf("a superuser sees everything: %d %s", status, body)
	}
}

func TestPostBodyThatIsNotAnObjectIs400(t *testing.T) {
	f := setup(t, nil, nil)
	for _, body := range []string{
		`[]`, `"name"`, `1`, `null`, `true`, `{"name":`, `{"name":"Bob"} {}`, `name=Bob`,
		`{"name":["Bob"]}`, `{"name":{"x":1}}`, `{"name":null}`, `{"name":true}`,
	} {
		if status, resp := f.post(t, "agent", "/api/cqrs/rm/customers", body); status != 400 {
			t.Errorf("%s: want 400, got %d %s", body, status, resp)
		}
	}
	big := `{"name":"` + strings.Repeat("x", 1<<20) + `"}`
	if status, _ := f.post(t, "agent", "/api/cqrs/rm/customers", big); status != http.StatusRequestEntityTooLarge {
		t.Errorf("a body over the cap: want 413, got %d", status)
	}
}

func TestRefusePersonalTermsInURL(t *testing.T) {
	off := searchSetup(t, nil)
	if status, body := off.get(t, "agent", "/api/cqrs/rm/customers?email=alice@example.com"); status != 200 || len(items(t, body)) != 1 {
		t.Fatalf("off by default: a personal term in the URL still searches: %d %s", status, body)
	}

	f := searchSetup(t, func(c *queryroute.Config) { c.RefusePersonalTermsInURL = true })
	status, body := f.get(t, "agent", "/api/cqrs/rm/customers?email=alice@example.com")
	if status != 400 || !strings.Contains(body, "POST") || strings.Contains(body, "alice") {
		t.Fatalf("on: a personal term in the URL is a 400 naming POST (not echoing the term): %d %s", status, body)
	}
	if status, body := f.get(t, "agent", "/api/cqrs/rm/customers?name=al"); status != 200 || len(items(t, body)) != 2 {
		t.Fatalf("on: a plain search in the URL still works: %d %s", status, body)
	}
	if status, body := f.post(t, "agent", "/api/cqrs/rm/customers", `{"email":"alice@example.com"}`); status != 200 || len(items(t, body)) != 1 {
		t.Fatalf("on: the same search by POST works: %d %s", status, body)
	}
	// the live route is unaffected (it has no POST yet)
	s := openLive(t, f.srv.URL, f.tokens["agent"], "/api/cqrs/rm/customers/live?email="+url.QueryEscape("alice@example.com"))
	if s.status != 200 {
		t.Fatalf("on: the live route still answers, got %d", s.status)
	}
	if ev, data := s.next(); ev != "view" || viewKeys(t, data, "customerId") != "c1" {
		t.Fatalf("on: the live route's first view is the search's result: %s %s", ev, data)
	}
}

// A generated host builds Config in generated code, so the refusal can also
// be switched on per deployment from the environment.
func TestRefusePersonalTermsInURLFromEnvironment(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(queryroute.RefusePersonalTermsInURLEnv, v)
			f := searchSetup(t, nil)
			status, body := f.get(t, "agent", "/api/cqrs/rm/customers?email=alice@example.com")
			if status != 400 || !strings.Contains(body, "POST") {
				t.Fatalf("%s=%s: a personal term in the URL is a 400 naming POST: %d %s",
					queryroute.RefusePersonalTermsInURLEnv, v, status, body)
			}
			if status, body := f.post(t, "agent", "/api/cqrs/rm/customers", `{"email":"alice@example.com"}`); status != 200 || len(items(t, body)) != 1 {
				t.Fatalf("the same search by POST works: %d %s", status, body)
			}
		})
	}
	for _, v := range []string{"", "0", "false", "yes"} {
		t.Run("off-"+v, func(t *testing.T) {
			t.Setenv(queryroute.RefusePersonalTermsInURLEnv, v)
			f := searchSetup(t, nil)
			if status, body := f.get(t, "agent", "/api/cqrs/rm/customers?email=alice@example.com"); status != 200 || len(items(t, body)) != 1 {
				t.Fatalf("%s=%q leaves it off: %d %s", queryroute.RefusePersonalTermsInURLEnv, v, status, body)
			}
		})
	}
}
