//go:build smoke

package smoke

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/outbound"

	"github.com/jamestryand/pocketcqrs/extcaller"
)

// echoRule builds a Rule that, on TaskCreated, GETs thirdPartyURL and
// dispatches a CreateTask for a deterministic "echo-<sourceAggregateID>"
// task — the same "deterministic target id" pattern reactors.go's own doc
// comment describes, layered under the Idempotency-Key protection
// gatewayclient always sets.
func echoRule(thirdPartyURL string) extcaller.Rule {
	return extcaller.Rule{
		EventType: "TaskCreated",
		BuildRequest: func(ev events.Event) (outbound.Request, error) {
			return outbound.Request{Method: "GET", URL: thirdPartyURL + "/lookup"}, nil
		},
		HandleResponse: func(ev events.Event, resp *outbound.Response) ([]extcaller.FollowUp, error) {
			payload, _ := json.Marshal(map[string]string{"title": "echo of " + ev.AggregateID})
			return []extcaller.FollowUp{{
				Aggregate: "task",
				ID:        "echo-" + ev.AggregateID,
				Name:      "CreateTask",
				Payload:   payload,
			}}, nil
		},
	}
}

// TestBurstRecoversWithinRetryBudget is PLAN.md Phase 2's explicit test: a
// burst of events while the target is down, recovering within the retry
// budget, resumes cleanly with no lost or duplicated dispatch.
//
// The fake third party is made deterministically flaky (fails the first 2
// calls total, then always succeeds) rather than time-based, so the test
// doesn't race wall-clock recovery against the engine's own tick/backoff
// timing: the consumers.Engine processes one consumer's events strictly in
// position order, one at a time (consumers.go's RunOnce), so the first
// task's event absorbs both failures across its own retry budget and
// recovers on its final attempt — exactly the "buffering falls out of the
// checkpointed engine, not a bespoke queue" property this component is
// built on.
func TestBurstRecoversWithinRetryBudget(t *testing.T) {
	var calls int32
	third := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer third.Close()

	h := startBackend(t)
	stores := h.openStores(t)

	outClient, err := outbound.New(outbound.Config{
		AllowedHosts: []string{thirdPartyHost(t, third.URL)},
		AllowPrivate: true,
		Timeout:      2 * time.Second,
		MaxInFlight:  4,
		MaxBodyBytes: 1 << 16,
	})
	if err != nil {
		t.Fatalf("outbound.New: %v", err)
	}

	logger := func(msg string, args ...any) { t.Logf("%s %v", msg, args) }
	consumer, err := extcaller.New(extcaller.Config{
		Name:        "smoke-burst",
		Rules:       []extcaller.Rule{echoRule(third.URL)},
		Outbound:    outClient,
		Gateway:     h.gatewayClient(),
		DeadLetters: stores.Local,
		Retry:       extcaller.RetryPolicy{MaxAttempts: 3, Backoff: 20 * time.Millisecond},
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("extcaller.New: %v", err)
	}

	engine := stores.Engine(logger)
	engine.Register(consumer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.Start(ctx)

	// Burst: create 3 tasks. t1's event absorbs the two configured
	// failures (see the atomic counter above) and recovers on attempt 3;
	// t2 and t3 process after the target has already recovered.
	for i := 1; i <= 3; i++ {
		h.command("task", fmt.Sprintf("t%d", i), "CreateTask", map[string]string{"title": fmt.Sprintf("task %d", i)})
	}

	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("echo-t%d", i)
		eventually(t, id+" to exist", func() bool { return h.taskExists(id) })
	}

	letters, err := stores.Local.DeadLetters(context.Background(), false)
	if err != nil {
		t.Fatalf("DeadLetters: %v", err)
	}
	if len(letters) != 0 {
		t.Errorf("expected 0 dead letters (all 3 should recover within the retry budget), got %d: %+v",
			len(letters), letters)
	}
}

// TestRedeliveryDoesNotDuplicateDispatch directly exercises this session's
// key design finding: the gateway's HTTP route gives an out-of-process
// caller no way to use reactors.Dispatch's in-process CommandApplied dedup,
// so gatewayclient always sets a deterministic Idempotency-Key instead. This
// test simulates the one redelivery scenario that matters — a crash between
// a successful follow-up dispatch and the next checkpoint save — by calling
// Consumer.Apply twice directly for the same real, committed event, and
// asserting the target deployment shows exactly one resulting task, not two.
func TestRedeliveryDoesNotDuplicateDispatch(t *testing.T) {
	third := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer third.Close()

	h := startBackend(t)
	stores := h.openStores(t)

	outClient, err := outbound.New(outbound.Config{
		AllowedHosts: []string{thirdPartyHost(t, third.URL)},
		AllowPrivate: true,
		Timeout:      2 * time.Second,
		MaxInFlight:  4,
		MaxBodyBytes: 1 << 16,
	})
	if err != nil {
		t.Fatalf("outbound.New: %v", err)
	}

	logger := func(msg string, args ...any) { t.Logf("%s %v", msg, args) }
	consumer, err := extcaller.New(extcaller.Config{
		Name:        "smoke-redelivery",
		Rules:       []extcaller.Rule{echoRule(third.URL)},
		Outbound:    outClient,
		Gateway:     h.gatewayClient(),
		DeadLetters: stores.Local,
		Retry:       extcaller.RetryPolicy{MaxAttempts: 1},
		Logger:      logger,
	})
	if err != nil {
		t.Fatalf("extcaller.New: %v", err)
	}

	h.command("task", "t-redelivery", "CreateTask", map[string]string{"title": "redelivery source"})

	var ev events.Event
	eventually(t, "the TaskCreated event to appear in events.db", func() bool {
		batch, err := stores.Source.Poll(context.Background(), 0, 100)
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		for _, e := range batch {
			if e.Type == "TaskCreated" && e.AggregateID == "t-redelivery" {
				ev = e
				return true
			}
		}
		return false
	})

	// Simulate redelivery: Apply the same committed event twice, exactly as
	// the engine would if a process crash landed between the follow-up
	// dispatch succeeding and the checkpoint save that normally follows it
	// immediately (consumers.go's RunOnce).
	ctx := context.Background()
	if err := consumer.Apply(ctx, ev); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if err := consumer.Apply(ctx, ev); err != nil {
		t.Fatalf("second Apply (simulated redelivery): %v", err)
	}

	if !h.taskExists("echo-t-redelivery") {
		t.Fatal("expected echo-t-redelivery to exist after redelivery")
	}

	letters, err := stores.Local.DeadLetters(context.Background(), false)
	if err != nil {
		t.Fatalf("DeadLetters: %v", err)
	}
	if len(letters) != 0 {
		t.Errorf("expected 0 dead letters, got %d: %+v", len(letters), letters)
	}

	// The Idempotency-Key store's own record is the direct proof the
	// second POST was a replay, not a second decide: if the gateway had
	// re-applied CreateTask against an aggregate id that already exists,
	// the target deployment's own uniqueness rule would have rejected it
	// with a 400 (aggregates/task_test.go's "CreateTask twice" case) rather
	// than the Apply call succeeding silently — and this test's second
	// Apply call above would have failed loudly via c.cfg.Gateway.Dispatch
	// returning a non-2xx *gatewayclient.ErrStatus, which Apply turns into
	// a dead letter. Zero dead letters plus exactly one task existing is
	// only possible if the second dispatch was a genuine Idempotency-Key
	// replay of the first.
}

func thirdPartyHost(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}
