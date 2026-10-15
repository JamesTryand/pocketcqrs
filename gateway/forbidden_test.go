package gateway_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/gateway"
)

// A decider refusing WHO sent a command (schema 3.15.0: only the owner its
// own state names) is answered 403, distinct from a refusal of what the
// command says (400).
func TestForbiddenVerdictIs403(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	registry := decider.NewRegistry(store)
	decider.Register(registry, "expense", &decider.Decider[struct{}]{
		InitialState: func() struct{} { return struct{}{} },
		Decide: func(cmd decider.Command, _ struct{}) ([]events.NewEvent, error) {
			if cmd.Name == "Forbidden" {
				return nil, decider.Forbidden("only its own staff member may change it")
			}
			return nil, decider.Forbidden("unreachable")
		},
		Evolve: func(s struct{}, _ events.Event) (struct{}, error) { return s, nil },
	})
	router, _ := apis.NewRouter(app)
	gateway.RegisterRoutes(&core.ServeEvent{App: app, Router: router}, registry, gateway.Config{AllowAnonymous: true})
	mux, _ := router.BuildMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/api/cqrs/expense/x1/Forbidden", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusForbidden || body["error"] != "only its own staff member may change it" {
		t.Fatalf("want 403 with the message, got %d %v", resp.StatusCode, body)
	}
}
