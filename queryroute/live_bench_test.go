package queryroute_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/kms"
	"github.com/jamestryand/pocketcqrs/pii"
	"github.com/jamestryand/pocketcqrs/queryroute"
)

// benchmarkPush measures one live push's work, which is one read of the
// route: a page of 200 personal-data rows revealed for a role holder.
// With a warm reveal cache (capacity 0: the default) a push decrypts
// nothing; with a one-entry cache nearly every value goes to the key
// service each time (here the in-memory one, so this is the floor of the
// cold cost: a real facade adds a network round trip per page).
func benchmarkPush(b *testing.B, cacheCapacity int) {
	app, err := tests.NewTestApp()
	if err != nil {
		b.Fatal(err)
	}
	defer app.Cleanup()
	m := kms.NewMemory()
	col := core.NewBaseCollection("customers")
	col.Fields.Add(&core.TextField{Name: "customerId"}, &core.TextField{Name: "email"}, &core.TextField{Name: "name"})
	if err := app.Save(col); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("c%03d", i)
		v, err := pii.NewFresh(id+"@example.com").Encrypt(context.Background(), m, id, nil)
		if err != nil {
			b.Fatal(err)
		}
		env, _ := json.Marshal(v)
		rec := core.NewRecord(col)
		rec.Set("customerId", id)
		rec.Set("name", "n"+id)
		rec.Set("email", string(env))
		if err := app.Save(rec); err != nil {
			b.Fatal(err)
		}
	}
	people := core.NewAuthCollection("people")
	people.Fields.Add(&core.TextField{Name: "role"})
	if err := app.Save(people); err != nil {
		b.Fatal(err)
	}
	agent := core.NewRecord(people)
	agent.SetEmail("agent@people.test")
	agent.SetPassword("1234567890")
	agent.Set("role", "support")
	if err := app.Save(agent); err != nil {
		b.Fatal(err)
	}
	token, _ := agent.NewAuthToken()

	cache := pii.NewCache(cacheCapacity)
	router, _ := apis.NewRouter(app)
	spec := queryroute.Spec{Collection: "customers", Key: "customerId", Columns: []string{"customerId", "email", "name"},
		PII: map[string]string{"email": "customerId"}, RequiredRole: []string{"support"}}
	cfg := queryroute.Config{PII: &pii.Deps{KMS: m, Cache: cache},
		ResolveOwnRole: func(a *core.Record) string { return a.GetString("role") }}
	if err := queryroute.Register(&core.ServeEvent{App: app, Router: router}, spec, cfg); err != nil {
		b.Fatal(err)
	}
	mux, _ := router.BuildMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	read := func() {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+queryroute.PathPrefix+"customers?perPage=200", nil)
		req.Header.Set("Authorization", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b.Fatalf("status %d", resp.StatusCode)
		}
	}
	read() // warm it
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		read()
	}
}

func BenchmarkLivePush200RowsWarmCache(b *testing.B) { benchmarkPush(b, 0) }
func BenchmarkLivePush200RowsColdCache(b *testing.B) { benchmarkPush(b, 1) }
