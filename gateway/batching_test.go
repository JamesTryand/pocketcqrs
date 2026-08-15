package gateway_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/batching"
	"github.com/jamestryand/pocketcqrs/commandqueue"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/gateway"
	"github.com/jamestryand/pocketcqrs/idempotency"
)

func newTestGatewayWithBatching(t *testing.T, batchTimeout time.Duration, idem *idempotency.Store) (*httptest.Server, *batching.Writer) {
	t.Helper()

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)

	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	queue, err := commandqueue.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { queue.Close() })

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

	writer := batching.NewWriter(store, queue, registry, nil)

	pbRouter, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	gateway.RegisterRoutes(&core.ServeEvent{App: app, Router: pbRouter}, registry, gateway.Config{
		AllowAnonymous: true,
		Batching:       writer,
		BatchTimeout:   batchTimeout,
		Idempotency:    idem,
	})

	mux, err := pbRouter.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, writer
}

func TestGatewayBatchingReturnsSameSynchronousShapeAsDirect(t *testing.T) {
	srv, writer := newTestGatewayWithBatching(t, 5*time.Second, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer.Start(ctx)

	status, body := postCreate(t, srv, "")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	if !strings.Contains(body, "TaskCreated") {
		t.Fatalf("expected the created event in the response, got: %s", body)
	}
}

func TestGatewayBatchingDomainRejectionReturns400(t *testing.T) {
	srv, writer := newTestGatewayWithBatching(t, 5*time.Second, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer.Start(ctx)

	status1, body1 := postCreate(t, srv, "")
	if status1 != http.StatusOK {
		t.Fatalf("expected first create to succeed, got %d: %s", status1, body1)
	}
	status2, body2 := postCreate(t, srv, "")
	if status2 != http.StatusBadRequest {
		t.Fatalf("expected 400 for the second create, got %d: %s", status2, body2)
	}
}

func TestGatewayBatchingTimesOutWhenWriterNeverRuns(t *testing.T) {
	srv, _ := newTestGatewayWithBatching(t, 50*time.Millisecond, nil)
	// deliberately no writer.Start call -- nothing ever processes the queue

	status, body := postCreate(t, srv, "")
	if status != http.StatusGatewayTimeout {
		t.Fatalf("expected 504, got %d: %s", status, body)
	}
	if !strings.Contains(body, "did not complete in time") {
		t.Fatalf("expected the timeout message, got: %s", body)
	}
}

func TestGatewayBatchingWithIdempotencyKeyReplaysAfterCommit(t *testing.T) {
	idem, err := idempotency.Open(filepath.Join(t.TempDir(), "idempotency.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idem.Close() })

	srv, writer := newTestGatewayWithBatching(t, 5*time.Second, idem)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer.Start(ctx)

	status1, body1 := postCreate(t, srv, "key-1")
	if status1 != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status1, body1)
	}

	// retried with the same key: replayed verbatim -- if the decider had
	// re-run, the "already exists" rule would reject it with 400
	status2, body2 := postCreate(t, srv, "key-1")
	if status2 != http.StatusOK {
		t.Fatalf("expected the replayed 200, got %d: %s", status2, body2)
	}
	if body1 != body2 {
		t.Fatalf("replayed body differs from the original:\n first:  %s\n second: %s", body1, body2)
	}
}
