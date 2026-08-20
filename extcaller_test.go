package extcaller

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/outbound"

	"github.com/jamestryand/pocketcqrs-extensions/internal/gatewayclient"
)

func newTestOutbound(t *testing.T, host string) *outbound.Client {
	t.Helper()
	c, err := outbound.New(outbound.Config{
		AllowedHosts: []string{host},
		AllowPrivate: true,
		Timeout:      2 * time.Second,
		MaxInFlight:  4,
		MaxBodyBytes: 1 << 16,
	})
	if err != nil {
		t.Fatalf("outbound.New: %v", err)
	}
	return c
}

// hostOnly strips the port from a "host:port" address, since
// outbound.Config.AllowedHosts wants a bare hostname.
func hostOnly(t *testing.T, addr string) string {
	t.Helper()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("net.SplitHostPort(%q): %v", addr, err)
	}
	return host
}

func newTestDeadLetters(t *testing.T) *events.Store {
	t.Helper()
	store, err := events.Open(t.TempDir() + "/extcaller-test.db")
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func testEvent(evType string) events.Event {
	return events.Event{
		Position:    1,
		ID:          "ev-1",
		Aggregate:   "order",
		AggregateID: "order-1",
		Type:        evType,
		Data:        json.RawMessage(`{}`),
	}
}

func TestApply_UnmatchedEventType_NoOp(t *testing.T) {
	c, err := New(Config{
		Name:        "t",
		Outbound:    newTestOutbound(t, "example.com"),
		Gateway:     gatewayclient.New("http://unused.invalid", "tok", time.Second),
		DeadLetters: newTestDeadLetters(t),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Apply(context.Background(), testEvent("some.Other")); err != nil {
		t.Fatalf("Apply returned error for unmatched event type: %v", err)
	}
}

func TestApply_SuccessDispatchesFollowUp(t *testing.T) {
	third := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer third.Close()
	thirdHost := third.Listener.Addr().String()

	var dispatched []string
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dispatched = append(dispatched, r.URL.Path)
		if got := r.Header.Get("Idempotency-Key"); got == "" {
			t.Errorf("expected Idempotency-Key header on dispatch")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("expected Authorization header, got %q", got)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"events":[]}`))
	}))
	defer gw.Close()

	rule := Rule{
		EventType: "order.Confirmed",
		BuildRequest: func(ev events.Event) (outbound.Request, error) {
			return outbound.Request{Method: "GET", URL: "http://" + thirdHost + "/lookup"}, nil
		},
		HandleResponse: func(ev events.Event, resp *outbound.Response) ([]FollowUp, error) {
			return []FollowUp{{
				Aggregate: "fulfillment", ID: "fulfill-" + ev.ID, Name: "CreateTask",
				Payload: json.RawMessage(`{}`),
			}}, nil
		},
	}

	c, err := New(Config{
		Name:        "t",
		Rules:       []Rule{rule},
		Outbound:    newTestOutbound(t, hostOnly(t, thirdHost)),
		Gateway:     gatewayclient.New(gw.URL, "tok", time.Second),
		DeadLetters: newTestDeadLetters(t),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := c.Apply(context.Background(), testEvent("order.Confirmed")); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 gateway dispatch, got %d", len(dispatched))
	}
}

func TestApply_OutboundFailureExhaustsRetriesAndDeadLetters(t *testing.T) {
	var calls int
	third := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer third.Close()
	thirdHost := third.Listener.Addr().String()

	rule := Rule{
		EventType: "order.Confirmed",
		BuildRequest: func(ev events.Event) (outbound.Request, error) {
			return outbound.Request{Method: "GET", URL: "http://" + thirdHost + "/lookup"}, nil
		},
		HandleResponse: func(ev events.Event, resp *outbound.Response) ([]FollowUp, error) {
			if resp.Status != http.StatusOK {
				return nil, errors.New("non-200 from third party")
			}
			return nil, nil
		},
	}

	deadLetters := newTestDeadLetters(t)
	c, err := New(Config{
		Name:        "t",
		Rules:       []Rule{rule},
		Outbound:    newTestOutbound(t, hostOnly(t, thirdHost)),
		Gateway:     gatewayclient.New("http://unused.invalid", "tok", time.Second),
		DeadLetters: deadLetters,
		Retry:       RetryPolicy{MaxAttempts: 1, Backoff: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// A non-2xx from the outbound call is a Response, not an error (per
	// outbound.Client.Do's contract) — HandleResponse is what rejects it
	// here, exercising the "handling response" dead-letter path, which
	// never retries (retry only bounds the outbound call itself).
	if err := c.Apply(context.Background(), testEvent("order.Confirmed")); err != nil {
		t.Fatalf("Apply returned error (should always be nil): %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 outbound call, got %d", calls)
	}

	letters, err := deadLetters.DeadLetters(context.Background(), false)
	if err != nil {
		t.Fatalf("DeadLetters: %v", err)
	}
	if len(letters) != 1 {
		t.Fatalf("expected 1 dead letter, got %d", len(letters))
	}
	if letters[0].Consumer != "extcall:t" {
		t.Errorf("expected consumer %q, got %q", "extcall:t", letters[0].Consumer)
	}
}

func TestApply_HostNotAllowed_DeadLetters(t *testing.T) {
	rule := Rule{
		EventType: "order.Confirmed",
		BuildRequest: func(ev events.Event) (outbound.Request, error) {
			return outbound.Request{Method: "GET", URL: "http://not-allowed.example/lookup"}, nil
		},
		HandleResponse: func(ev events.Event, resp *outbound.Response) ([]FollowUp, error) {
			t.Fatal("HandleResponse must not be called when the outbound call is refused")
			return nil, nil
		},
	}

	deadLetters := newTestDeadLetters(t)
	c, err := New(Config{
		Name:        "t",
		Rules:       []Rule{rule},
		Outbound:    newTestOutbound(t, "allowed.example"),
		Gateway:     gatewayclient.New("http://unused.invalid", "tok", time.Second),
		DeadLetters: deadLetters,
		Retry:       RetryPolicy{MaxAttempts: 1, Backoff: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := c.Apply(context.Background(), testEvent("order.Confirmed")); err != nil {
		t.Fatalf("Apply returned error (should always be nil): %v", err)
	}

	letters, err := deadLetters.DeadLetters(context.Background(), false)
	if err != nil {
		t.Fatalf("DeadLetters: %v", err)
	}
	if len(letters) != 1 {
		t.Fatalf("expected 1 dead letter, got %d", len(letters))
	}
	if !strings.Contains(letters[0].Error, outbound.ErrHostNotAllowed.Error()) {
		t.Errorf("expected dead letter error to mention %q, got %q",
			outbound.ErrHostNotAllowed.Error(), letters[0].Error)
	}
}
