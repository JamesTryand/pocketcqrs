package gateway_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/gateway"
	"github.com/jamestryand/pocketcqrs/idempotency"
)

// taskState is a minimal decider: Create succeeds once per stream and
// rejects a second attempt (a domain rejection with no side effect, which
// is why the gateway does not need to cache it for idempotency).
type taskState struct{ Exists bool }

func newTestGateway(t *testing.T, idem *idempotency.Store) *httptest.Server {
	t.Helper()
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return newTestGatewayWithStore(t, store, idem)
}

func newTestGatewayWithStore(t *testing.T, store *events.Store, idem *idempotency.Store) *httptest.Server {
	t.Helper()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)

	registry := decider.NewRegistry(store)
	decider.Register(registry, "task", &decider.Decider[taskState]{
		InitialState: func() taskState { return taskState{} },
		Decide: func(cmd decider.Command, s taskState) ([]events.NewEvent, error) {
			if s.Exists {
				return nil, fmt.Errorf("task already exists")
			}
			return []events.NewEvent{{Type: "TaskCreated", Data: json.RawMessage(`{}`)}}, nil
		},
		Evolve: func(s taskState, ev events.Event) (taskState, error) {
			if ev.Type == "TaskCreated" {
				s.Exists = true
			}
			return s, nil
		},
	})

	pbRouter, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	gateway.RegisterRoutes(&core.ServeEvent{App: app, Router: pbRouter}, registry,
		gateway.Config{AllowAnonymous: true, Idempotency: idem})

	mux, err := pbRouter.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func postCreate(t *testing.T, srv *httptest.Server, idemKey string) (status int, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/cqrs/task/t1/Create", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func TestGatewayWithoutIdempotencyKeyBehavesAsBefore(t *testing.T) {
	srv := newTestGateway(t, nil)

	status, _ := postCreate(t, srv, "")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	// second create, still no key: the decider's own rule rejects it
	status, body := postCreate(t, srv, "")
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", status, body)
	}
}

func TestGatewayReplaysSameKeySameRequest(t *testing.T) {
	idem, err := idempotency.Open(filepath.Join(t.TempDir(), "idempotency.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idem.Close() })
	srv := newTestGateway(t, idem)

	status1, body1 := postCreate(t, srv, "key-1")
	if status1 != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status1, body1)
	}

	// retried with the same key: replayed verbatim, decider never re-runs
	// (if it had, the "already exists" rule would reject it with 400)
	status2, body2 := postCreate(t, srv, "key-1")
	if status2 != http.StatusOK {
		t.Fatalf("expected replayed 200, got %d: %s", status2, body2)
	}
	if body1 != body2 {
		t.Fatalf("replayed body differs from the original:\n first:  %s\n second: %s", body1, body2)
	}
}

func TestGatewaySameKeyDifferentRequestIsRejected(t *testing.T) {
	idem, err := idempotency.Open(filepath.Join(t.TempDir(), "idempotency.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idem.Close() })
	srv := newTestGateway(t, idem)

	if status, body := postCreate(t, srv, "key-1"); status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/cqrs/task/t2/Create", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Idempotency-Key", "key-1") // same key, different aggregate id
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", resp.StatusCode)
	}
}

func TestGatewayDifferentKeysBothApply(t *testing.T) {
	idem, err := idempotency.Open(filepath.Join(t.TempDir(), "idempotency.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idem.Close() })
	srv := newTestGateway(t, idem)

	if status, body := postCreate(t, srv, "key-1"); status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/cqrs/task/t2/Create", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Idempotency-Key", "key-2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for an independent key, got %d", resp.StatusCode)
	}
}

func TestGatewayOnReadOnlyStoreRefusesWithServiceUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	writer, err := events.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	writer.Close()

	ro, err := events.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ro.Close() })

	srv := newTestGatewayWithStore(t, ro, nil)

	status, body := postCreate(t, srv, "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", status, body)
	}
	if !strings.Contains(body, "read-only replica") {
		t.Fatalf("expected the read-only-replica message, got: %s", body)
	}
}
