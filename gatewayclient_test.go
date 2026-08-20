package gatewayclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDispatch_IdempotencyKeyIsDeterministic(t *testing.T) {
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"events":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok", 2*time.Second)
	cmd := Command{
		Aggregate: "task", ID: "t1", Name: "CreateTask",
		Payload:             json.RawMessage(`{}`),
		IdempotencyKeyParts: []string{"extcall:x", "ev-1", "0"},
	}

	if _, err := c.Dispatch(context.Background(), cmd); err != nil {
		t.Fatalf("first Dispatch: %v", err)
	}
	if _, err := c.Dispatch(context.Background(), cmd); err != nil {
		t.Fatalf("second Dispatch: %v", err)
	}

	if len(keys) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(keys))
	}
	if keys[0] == "" {
		t.Fatal("expected a non-empty Idempotency-Key")
	}
	if keys[0] != keys[1] {
		t.Errorf("expected the same IdempotencyKeyParts to produce the same key, got %q and %q", keys[0], keys[1])
	}
}

func TestDispatch_DifferentPartsProduceDifferentKeys(t *testing.T) {
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"events":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok", 2*time.Second)
	base := Command{Aggregate: "task", ID: "t1", Name: "CreateTask", Payload: json.RawMessage(`{}`)}

	a := base
	a.IdempotencyKeyParts = []string{"extcall:x", "ev-1", "0"}
	b := base
	b.IdempotencyKeyParts = []string{"extcall:x", "ev-2", "0"}

	if _, err := c.Dispatch(context.Background(), a); err != nil {
		t.Fatalf("Dispatch a: %v", err)
	}
	if _, err := c.Dispatch(context.Background(), b); err != nil {
		t.Fatalf("Dispatch b: %v", err)
	}
	if keys[0] == keys[1] {
		t.Errorf("expected different IdempotencyKeyParts to produce different keys, both were %q", keys[0])
	}
}

func TestDispatch_NonOKStatusReturnsErrStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"concurrency conflict"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok", 2*time.Second)
	_, err := c.Dispatch(context.Background(), Command{
		Aggregate: "task", ID: "t1", Name: "CreateTask", Payload: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("expected an error for a 409 response")
	}
	var statusErr *ErrStatus
	if !isErrStatus(err, &statusErr) {
		t.Fatalf("expected *ErrStatus, got %T: %v", err, err)
	}
	if statusErr.Status != http.StatusConflict {
		t.Errorf("expected status 409, got %d", statusErr.Status)
	}
}

func isErrStatus(err error, target **ErrStatus) bool {
	if e, ok := err.(*ErrStatus); ok {
		*target = e
		return true
	}
	return false
}
