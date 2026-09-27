package gateway_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/gateway"
)

// Only the decider's own verdict is a 400. A failure anywhere else on the
// command path -- loading or upcasting the stream, the append itself -- is
// the platform's problem, not the caller's, and must not look like a
// domain rejection a client would give up on.

func TestDomainRejectionStays400(t *testing.T) {
	srv := newTestGateway(t, nil)
	if status, body := postCreate(t, srv, ""); status != http.StatusOK {
		t.Fatalf("first create: %d %s", status, body)
	}
	status, body := postCreate(t, srv, "")
	if status != http.StatusBadRequest || !strings.Contains(strings.ToLower(body), "task already exists") {
		t.Fatalf("a domain rejection must stay 400 with the decider's message, got %d: %s", status, body)
	}
}

func TestInfrastructureFailureIs500NotRejection(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	srv := newTestGatewayWithConfig(t, store, gateway.Config{AllowAnonymous: true})

	if status, body := postCreate(t, srv, ""); status != http.StatusOK {
		t.Fatalf("first create: %d %s", status, body)
	}
	// the stream now fails to load: an upcaster fault, not a verdict
	store.SetUpcaster(func(ev events.Event) (events.Event, error) {
		return ev, errors.New("upcaster exploded")
	})
	status, body := postCreate(t, srv, "")
	if status != http.StatusInternalServerError {
		t.Fatalf("a stream-load failure must be 500, got %d: %s", status, body)
	}
}

func TestBusyEventStoreIs503(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	store, err := events.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	srv := newTestGatewayWithConfig(t, store, gateway.Config{AllowAnonymous: true})

	// another writer holds the write lock for longer than events.db's
	// busy_timeout: the dependency is unavailable, and a retry may succeed
	other, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	conn, err := other.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.ExecContext(context.Background(), "ROLLBACK") })

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/cqrs/task/busy1/Create", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a busy event store must be 503, got %d: %s", resp.StatusCode, body)
	}
}
