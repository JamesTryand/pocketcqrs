package gateway_test

import (
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/gateway"
)

// Command outcomes (health/telemetry contract section 7): the gateway reports
// each command's HTTP status to Config.Metrics.

type recordedStatuses struct {
	mu       sync.Mutex
	statuses []int
}

func (r *recordedStatuses) RecordCommand(httpStatus int, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses = append(r.statuses, httpStatus)
}

func (r *recordedStatuses) all() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.statuses...)
}

func openStore(t *testing.T) *events.Store {
	t.Helper()
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestGatewayRecordsEachCommandsStatus(t *testing.T) {
	rec := &recordedStatuses{}
	srv := newTestGatewayWithConfig(t, openStore(t), gateway.Config{AllowAnonymous: true, Metrics: rec})

	postCreate(t, srv, "") // accepted
	postCreate(t, srv, "") // the decider refuses: rejected

	got := rec.all()
	if len(got) != 2 || got[0] != http.StatusOK || got[1] != http.StatusBadRequest {
		t.Errorf("recorded %v, want [200 400]", got)
	}
}

func TestGatewayRecordsAnUnauthenticatedCommandAs401(t *testing.T) {
	rec := &recordedStatuses{}
	srv := newTestGatewayWithConfig(t, openStore(t), gateway.Config{Metrics: rec})

	if status, _ := postCreate(t, srv, ""); status != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", status)
	}

	if got := rec.all(); len(got) != 1 || got[0] != http.StatusUnauthorized {
		t.Errorf("recorded %v, want [401]", got)
	}
}
