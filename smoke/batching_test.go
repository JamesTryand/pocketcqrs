//go:build smoke

package smoke

import (
	"net/http"
	"testing"
)

// TestBatchingNormalRequestUnchangedContract confirms the whole point of
// item 4's design: with --cqrsCommandBatching on, a command still returns
// 200 with its events synchronously, over a real HTTP round trip -- no
// client-visible change from the non-batching default.
//
// The explicit --cqrsCommandBatching flag below is redundant now that
// batching is the default -- left in place as a self-documenting assertion
// that batching is genuinely on here, independent of whatever main.go's
// default happens to be.
func TestBatchingNormalRequestUnchangedContract(t *testing.T) {
	h := startBackendFlags(t, nil, "--tutorial", "--cqrsCommandBatching")

	var report map[string]any
	h.apiOK(http.MethodPost, "/api/cqrs/task/b1/CreateTask",
		jsonBody(map[string]string{"title": "batched"}), &report)

	events, ok := report["events"].([]any)
	if !ok || len(events) != 1 {
		t.Fatalf("expected 1 event in the response, got: %+v", report)
	}

	var stream struct {
		Streams []struct {
			AggregateID string `json:"aggregateId"`
			Events      int64  `json:"events"`
		} `json:"streams"`
	}
	h.apiOK(http.MethodGet, "/api/cqrs/streams?aggregate=task&aggregateId=b1", nil, &stream)
	if len(stream.Streams) != 1 || stream.Streams[0].Events != 1 {
		t.Fatalf("expected exactly 1 durable event for b1, got: %+v", stream.Streams)
	}
}

// TestBatchingDomainRejectionReturns400 confirms a rejected command behaves
// identically to the non-batching path -- a real HTTP round trip through
// the queue, not just the in-process gateway test's version of this check.
func TestBatchingDomainRejectionReturns400(t *testing.T) {
	h := startBackendFlags(t, nil, "--tutorial", "--cqrsCommandBatching")

	h.apiOK(http.MethodPost, "/api/cqrs/task/b2/CreateTask", jsonBody(map[string]string{"title": "x"}), nil)

	status, body := h.api(http.MethodPost, "/api/cqrs/task/b2/CreateTask", jsonBody(map[string]string{"title": "x"}), nil)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for the duplicate create, got %d: %s", status, body)
	}
}

// TestBatchingCanBeExplicitlyDisabled confirms the opt-out still works now
// that batching is the default: --cqrsCommandBatching=false must return to
// the direct, one-transaction-per-command path, with the gateway's
// synchronous contract otherwise unchanged.
func TestBatchingCanBeExplicitlyDisabled(t *testing.T) {
	h := startBackendFlags(t, nil, "--tutorial", "--cqrsCommandBatching=false")

	var report map[string]any
	h.apiOK(http.MethodPost, "/api/cqrs/task/b3/CreateTask",
		jsonBody(map[string]string{"title": "direct"}), &report)

	events, ok := report["events"].([]any)
	if !ok || len(events) != 1 {
		t.Fatalf("expected 1 event in the response, got: %+v", report)
	}

	status, body := h.api(http.MethodPost, "/api/cqrs/task/b3/CreateTask", jsonBody(map[string]string{"title": "x"}), nil)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for the duplicate create on the direct path, got %d: %s", status, body)
	}
}
