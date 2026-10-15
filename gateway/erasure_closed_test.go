package gateway_test

import (
	"bytes"
	"context"
	"io"
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
	"github.com/jamestryand/pocketcqrs/pii"
)

// erasureGateway serves the built-in dataSubject aggregate through a gateway
// configured with cfg, and returns the server and the store behind it.
func erasureGateway(t *testing.T, cfg gateway.Config) (*httptest.Server, *events.Store) {
	t.Helper()
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
	pii.RegisterDataSubjects(registry)
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	gateway.RegisterRoutes(&core.ServeEvent{App: app, Router: router}, registry, cfg)
	mux, err := router.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, store
}

func postErase(t *testing.T, srv *httptest.Server, aggregate string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/cqrs/"+aggregate+"/subject-1/EraseSubject", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// E1: erasure destroys a subject's key, which can't be undone. With no
// Authorize hook every command is allowed, so before this any caller could
// erase anyone. The built-in dataSubject aggregate fails closed instead, as
// dotnetcqrs's gateway does (0.19.0): none of its commands is accepted until
// the host wires a policy, and nothing is recorded.
func TestErasureIsRefusedWhenNoPolicyIsWired(t *testing.T) {
	srv, store := erasureGateway(t, gateway.Config{AllowAnonymous: true})
	status, body := postErase(t, srv, pii.SubjectAggregate)
	if status != http.StatusForbidden {
		t.Fatalf("erasure with no policy wired: want 403, got %d: %s", status, body)
	}
	evs, err := store.Poll(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("a refused erasure recorded %d event(s): %+v", len(evs), evs)
	}
}

// A host that wires a policy decides who may erase; the gateway then lets an
// allowed erasure through as any other command.
func TestErasureFollowsAWiredPolicy(t *testing.T) {
	allow := func(_ *core.RequestEvent, _, _, _ string, _ []byte) (bool, error) { return true, nil }
	srv, store := erasureGateway(t, gateway.Config{AllowAnonymous: true, Authorize: allow})
	status, body := postErase(t, srv, pii.SubjectAggregate)
	if status != http.StatusOK {
		t.Fatalf("an erasure the policy allows: want 200, got %d: %s", status, body)
	}
	evs, err := store.Poll(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != pii.SubjectErasedEvent {
		t.Fatalf("want one SubjectErased, got %+v", evs)
	}
}
