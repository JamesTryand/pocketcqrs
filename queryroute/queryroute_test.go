package queryroute_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/kms"
	"github.com/jamestryand/pocketcqrs/pii"
	"github.com/jamestryand/pocketcqrs/queryroute"
)

var customers = queryroute.Spec{
	Collection:   "customers",
	Key:          "customerId",
	Columns:      []string{"customerId", "email", "name"},
	PII:          map[string]string{"email": "customerId"},
	RequiredRole: []string{"support"},
	SelfSubject:  "customerId",
}

type fixture struct {
	srv    *httptest.Server
	app    *tests.TestApp
	kms    *kms.Memory
	tokens map[string]string
}

// downKMS answers every decrypt as the facade being unreachable.
type downKMS struct{ *kms.Memory }

func (downKMS) DecryptBatch(context.Context, string, []string) ([]kms.BatchItem, kms.KeyState, error) {
	return nil, kms.KeyFound, &kms.UnavailableError{Op: "decrypt-batch", Err: errors.New("connection refused")}
}

func setup(t *testing.T, mutate func(app core.App, m *kms.Memory), client func(*kms.Memory) kms.Client) *fixture {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	m := kms.NewMemory()

	col := core.NewBaseCollection("customers")
	col.Fields.Add(&core.TextField{Name: "customerId"}, &core.TextField{Name: "email"}, &core.TextField{Name: "name"})
	open := "" // a migration that left the collection public: Register must close it
	col.ListRule, col.ViewRule = &open, &open
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	put := func(id, email, name string) {
		rec := core.NewRecord(col)
		rec.Set("customerId", id)
		rec.Set("name", name)
		if email != "" {
			v, err := pii.NewFresh(email).Encrypt(context.Background(), m, id, nil)
			if err != nil {
				t.Fatal(err)
			}
			env, _ := json.Marshal(v)
			rec.Set("email", string(env))
		}
		if err := app.Save(rec); err != nil {
			t.Fatal(err)
		}
	}
	put("c1", "alice@example.com", "Alice")
	put("c2", "bob@example.com", "Bob")
	put("c3", "carol@example.com", "Carol")
	_ = m.DestroyKey(context.Background(), "c3") // carol was erased
	if mutate != nil {
		mutate(app, m)
	}

	users := core.NewAuthCollection("people")
	users.Fields.Add(&core.TextField{Name: "role"}, &core.TextField{Name: "subjectId"})
	if err := app.Save(users); err != nil {
		t.Fatal(err)
	}
	tokens := map[string]string{}
	mk := func(label, role, subject string) {
		rec := core.NewRecord(users)
		rec.SetEmail(label + "@people.test")
		rec.SetPassword("1234567890")
		rec.Set("role", role)
		rec.Set("subjectId", subject)
		if err := app.Save(rec); err != nil {
			t.Fatal(err)
		}
		tok, err := rec.NewAuthToken()
		if err != nil {
			t.Fatal(err)
		}
		tokens[label] = tok
	}
	mk("staff", "support", "")
	mk("alice", "", "c1")
	mk("nobody", "", "")
	su, _ := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	root := core.NewRecord(su)
	root.SetEmail("root@people.test")
	root.SetPassword("1234567890")
	if err := app.Save(root); err != nil {
		t.Fatal(err)
	}
	tokens["root"], _ = root.NewAuthToken()

	var c kms.Client = m
	if client != nil {
		c = client(m)
	}
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	se := &core.ServeEvent{App: app, Router: router}
	cfg := queryroute.Config{
		PII:            &pii.Deps{KMS: c, Cache: pii.NewCache(0)},
		ResolveOwnRole: func(a *core.Record) string { return a.GetString("role") },
		SubjectOf:      func(a *core.Record) string { return a.GetString("subjectId") },
	}
	if err := queryroute.Register(se, customers, cfg); err != nil {
		t.Fatal(err)
	}
	mux, err := router.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &fixture{srv: srv, app: app, kms: m, tokens: tokens}
}

func (f *fixture) get(t *testing.T, who, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, f.srv.URL+path, nil)
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

type page struct {
	Items []map[string]any `json:"items"`
}

func items(t *testing.T, body string) map[string]map[string]any {
	t.Helper()
	var p page
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	out := map[string]map[string]any{}
	for _, it := range p.Items {
		out[it["customerId"].(string)] = it
	}
	return out
}

func TestRoleSeesEveryRowRevealedAndErasedIsMarked(t *testing.T) {
	f := setup(t, nil, nil)
	status, body := f.get(t, "staff", "/api/cqrs/rm/customers")
	if status != 200 {
		t.Fatalf("%d %s", status, body)
	}
	rows := items(t, body)
	if len(rows) != 3 || rows["c1"]["email"] != "alice@example.com" || rows["c2"]["email"] != "bob@example.com" {
		t.Fatalf("staff sees every row, revealed: %s", body)
	}
	if red, ok := rows["c3"]["email"].(map[string]any); !ok || red["$redacted"] != true {
		t.Fatalf(`an erased value is {"$redacted":true}, not null or plaintext: %s`, body)
	}
	if _, calls, _ := f.kms.Calls(); calls != 3 {
		t.Fatalf("one decrypt-batch per subject on the page (3), got %d", calls)
	}
}

func TestASubjectSeesOnlyTheirOwnRow(t *testing.T) {
	f := setup(t, nil, nil)
	status, body := f.get(t, "alice", "/api/cqrs/rm/customers")
	if status != 200 {
		t.Fatalf("%d %s", status, body)
	}
	rows := items(t, body)
	if len(rows) != 1 || rows["c1"]["email"] != "alice@example.com" {
		t.Fatalf("self-access is exactly your own rows: %s", body)
	}
	// and a filter can't widen it
	if _, body := f.get(t, "alice", "/api/cqrs/rm/customers?customerId=c2"); len(items(t, body)) != 0 {
		t.Fatalf("a filter must not escape self-access: %s", body)
	}
}

func TestNoRoleNoSubjectIsForbiddenAndAnonymousIsRefused(t *testing.T) {
	f := setup(t, nil, nil)
	if status, body := f.get(t, "nobody", "/api/cqrs/rm/customers"); status != 403 {
		t.Fatalf("want 403, got %d %s", status, body)
	}
	if status, _ := f.get(t, "", "/api/cqrs/rm/customers"); status != 401 {
		t.Fatalf("an anonymous caller must authenticate, got %d", status)
	}
	if status, body := f.get(t, "root", "/api/cqrs/rm/customers"); status != 200 || len(items(t, body)) != 3 {
		t.Fatalf("a superuser sees everything: %d %s", status, body)
	}
}

func TestPersonalDataCannotBeFilteredOrSortedOn(t *testing.T) {
	f := setup(t, nil, nil)
	for _, q := range []string{"?email=alice@example.com", "?sort=email", "?sort=-email", "?nope=1", "?perPage=0"} {
		if status, body := f.get(t, "staff", "/api/cqrs/rm/customers"+q); status != 400 {
			t.Errorf("%s: want 400, got %d %s", q, status, body)
		}
	}
	status, body := f.get(t, "staff", "/api/cqrs/rm/customers?name=Bob&sort=-name")
	if rows := items(t, body); status != 200 || len(rows) != 1 || rows["c2"] == nil {
		t.Fatalf("plain columns filter normally: %d %s", status, body)
	}
}

func TestRegisteringLocksPocketBasesOwnRecordAPI(t *testing.T) {
	f := setup(t, nil, nil)
	col, _ := f.app.FindCollectionByNameOrId("customers")
	if col.ListRule != nil || col.ViewRule != nil {
		t.Fatal("List/View must be superuser-only once the route owns the collection")
	}
	if status, body := f.get(t, "root", "/api/collections/customers/records"); status != 200 {
		t.Fatalf("the record API itself is there, for superusers: %d %s", status, body)
	}
	for _, who := range []string{"staff", "alice"} {
		status, body := f.get(t, who, "/api/collections/customers/records")
		if status != http.StatusForbidden || strings.Contains(body, "customerId") {
			t.Fatalf("%s: the record API must refuse (403), got %d %s", who, status, body)
		}
	}
}

func TestPlaintextInAPIIColumnFailsClosed(t *testing.T) {
	f := setup(t, func(app core.App, _ *kms.Memory) {
		col, _ := app.FindCollectionByNameOrId("customers")
		rec := core.NewRecord(col)
		rec.Set("customerId", "c9")
		rec.Set("email", "leaked@example.com")
		_ = app.Save(rec)
	}, nil)
	status, body := f.get(t, "staff", "/api/cqrs/rm/customers")
	if status != 500 || strings.Contains(body, "leaked") {
		t.Fatalf("plaintext in a pii column must never be served: %d %s", status, body)
	}
}

func TestKeyServiceDownIs503(t *testing.T) {
	f := setup(t, nil, func(m *kms.Memory) kms.Client { return downKMS{m} })
	status, body := f.get(t, "staff", "/api/cqrs/rm/customers")
	if status != 503 || strings.Contains(body, "alice") {
		t.Fatalf("want 503 with nothing revealed, got %d %s", status, body)
	}
}

func TestRegisterRefusesAMisconfiguredRoute(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()
	router, _ := apis.NewRouter(app)
	se := &core.ServeEvent{App: app, Router: router}
	if err := queryroute.Register(se, customers, queryroute.Config{}); err == nil {
		t.Fatal("a pii read model without pii.Deps must be refused")
	}
	bad := customers
	bad.PII = map[string]string{"email": "nosuchcolumn"}
	if err := queryroute.Register(se, bad, queryroute.Config{PII: &pii.Deps{KMS: kms.NewMemory()}}); err == nil {
		t.Fatal("a subject column that isn't a column must be refused")
	}
}
