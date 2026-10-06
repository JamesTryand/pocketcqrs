package queryroute_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/kms"
	"github.com/jamestryand/pocketcqrs/match"
	"github.com/jamestryand/pocketcqrs/pii"
	"github.com/jamestryand/pocketcqrs/queryroute"
	"github.com/jamestryand/pocketcqrs/searchindex"
)

var (
	byEmail = searchindex.Field{Param: "email", Field: "email", Mode: match.Exact, Normalize: match.Email, PII: true}
	byName  = searchindex.Field{Param: "name", Field: "name", Mode: match.Prefix, MinPrefix: 2}
)

// TestMatchFiltersSearchThroughTheRoute drives the whole read path: events
// indexed into search.db, the read model's rows holding envelopes, and
// searches through GET /api/cqrs/rm/customers.
func TestMatchFiltersSearchThroughTheRoute(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	m := kms.NewMemory()
	deps := &pii.Deps{KMS: m, Cache: pii.NewCache(0)}

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	col := core.NewBaseCollection("customers")
	col.Fields.Add(&core.TextField{Name: "customerId"}, &core.TextField{Name: "email"}, &core.TextField{Name: "name"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	es, err := events.Open(filepath.Join(dir, "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { es.Close() })

	for _, c := range [][3]string{{"c1", "alice@example.com", "Alice"}, {"c2", "alan@example.com", "Alan"}, {"c3", "bob@example.com", "Bob"}} {
		enc, err := pii.NewFresh(c[1]).Encrypt(ctx, m, c[0], nil)
		if err != nil {
			t.Fatal(err)
		}
		env, _ := json.Marshal(enc)
		data, _ := json.Marshal(map[string]any{"customerId": c[0], "email": json.RawMessage(env), "name": c[2]})
		if _, err := es.Append(ctx, "customer", c[0], 0, []events.NewEvent{{Type: "CustomerRegistered", Data: data}}); err != nil {
			t.Fatal(err)
		}
		rec := core.NewRecord(col)
		rec.Set("customerId", c[0])
		rec.Set("email", string(env))
		rec.Set("name", c[2])
		if err := app.Save(rec); err != nil {
			t.Fatal(err)
		}
	}

	store, err := searchindex.Open(filepath.Join(dir, "search.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	engine := consumers.NewEngine(es, nil)
	idx, err := searchindex.Register(ctx, engine, store, searchindex.Spec{
		Collection: "customers", Events: []string{"CustomerRegistered"}, Fields: []searchindex.Field{byEmail, byName},
	}, deps, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	spec := customers
	spec.Match = []searchindex.Field{byEmail, byName}
	users := core.NewAuthCollection("people")
	users.Fields.Add(&core.TextField{Name: "role"}, &core.TextField{Name: "subjectId"})
	if err := app.Save(users); err != nil {
		t.Fatal(err)
	}
	token := func(role, subject string) string {
		rec := core.NewRecord(users)
		rec.SetEmail(role + subject + "@people.test")
		rec.SetPassword("1234567890")
		rec.Set("role", role)
		rec.Set("subjectId", subject)
		if err := app.Save(rec); err != nil {
			t.Fatal(err)
		}
		tok, _ := rec.NewAuthToken()
		return tok
	}
	staff, alice := token("support", ""), token("", "c1")

	router, _ := apis.NewRouter(app)
	se := &core.ServeEvent{App: app, Router: router}
	cfg := queryroute.Config{
		PII: deps, Indexes: map[string]*searchindex.Index{"customers": idx},
		ResolveOwnRole: func(a *core.Record) string { return a.GetString("role") },
		SubjectOf:      func(a *core.Record) string { return a.GetString("subjectId") },
	}
	if err := queryroute.Register(se, spec, cfg); err != nil {
		t.Fatal(err)
	}
	mux, _ := router.BuildMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f := &fixture{srv: srv, tokens: map[string]string{"staff": staff, "alice": alice}}

	get := func(who, q string) (int, map[string]map[string]any) {
		status, body := f.get(t, who, "/api/cqrs/rm/customers"+q)
		if status != http.StatusOK {
			return status, nil
		}
		return status, items(t, body)
	}
	if _, rows := get("staff", "?email=ALICE@Example.com"); len(rows) != 1 || rows["c1"]["email"] != "alice@example.com" {
		t.Fatalf("an exact search over personal data finds the row (and reveals it): %v", rows)
	}
	if _, rows := get("staff", "?name=al"); len(rows) != 2 || rows["c3"] != nil {
		t.Fatalf("a plain prefix search: %v", rows)
	}
	if _, rows := get("staff", "?name=al&email=alan@example.com"); len(rows) != 1 || rows["c2"] == nil {
		t.Fatalf("two searches intersect: %v", rows)
	}
	if _, rows := get("staff", "?email=nobody@example.com"); rows == nil || len(rows) != 0 {
		t.Fatalf("no match is an empty page: %v", rows)
	}
	if status, _ := get("staff", "?name=a"); status != http.StatusBadRequest {
		t.Fatalf("a prefix under minPrefixLength is a 400, got %d", status)
	}
	if _, rows := get("alice", "?name=al"); len(rows) != 1 || rows["c1"] == nil {
		t.Fatalf("a search can't widen self-access: %v", rows)
	}
}
