package gateway_test

import (
	"path/filepath"
	"testing"

	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/gateway"
)

// TestAuthorizeHookRejectsWithForbiddenAndNoSideEffect proves a rejecting
// Config.Authorize both returns 403 AND leaves no event appended — checked
// by pointing a SECOND gateway (permissive this time) at the exact same
// store afterwards: if the rejected attempt had appended TaskCreated
// anyway, this second Create for the same "t1" would fail with "task
// already exists" instead of succeeding. Same proof shape
// CqrsGatewayAuthorizeHookTests uses on the dotnetcqrs side this was
// ported from.
func TestAuthorizeHookRejectsWithForbiddenAndNoSideEffect(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	rejecting := newTestGatewayWithConfig(t, store, gateway.Config{
		AllowAnonymous: true,
		Authorize: func(_ *core.RequestEvent, _, _, _ string, _ []byte) (bool, error) {
			return false, nil
		},
	})
	status, body := postCreate(t, rejecting, "")
	if status != 403 {
		t.Fatalf("expected 403 from a rejecting Authorize hook, got %d: %s", status, body)
	}

	permissive := newTestGatewayWithConfig(t, store, gateway.Config{AllowAnonymous: true})
	status, body = postCreate(t, permissive, "")
	if status != 200 {
		t.Fatalf("the rejected attempt must have left no event appended -- a fresh Create for the "+
			"same task should still succeed, got %d: %s", status, body)
	}
}

// TestAuthorizeHookAllowsWhenTrue proves a permissive Authorize hook lets
// dispatch proceed exactly as if none were configured.
func TestAuthorizeHookAllowsWhenTrue(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	srv := newTestGatewayWithConfig(t, store, gateway.Config{
		AllowAnonymous: true,
		Authorize: func(_ *core.RequestEvent, _, _, _ string, _ []byte) (bool, error) {
			return true, nil
		},
	})
	status, body := postCreate(t, srv, "")
	if status != 200 {
		t.Fatalf("expected 200 from an allowing Authorize hook, got %d: %s", status, body)
	}
}

// TestAuthorizeHookErrorRejectsWith500 proves an Authorize hook's own error
// (a failed read-model lookup, say) surfaces as 500, distinct from an
// ordinary authorization refusal (403).
func TestAuthorizeHookErrorRejectsWith500(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	srv := newTestGatewayWithConfig(t, store, gateway.Config{
		AllowAnonymous: true,
		Authorize: func(_ *core.RequestEvent, _, _, _ string, _ []byte) (bool, error) {
			return false, errBoom
		},
	})
	status, body := postCreate(t, srv, "")
	if status != 500 {
		t.Fatalf("expected 500 when the Authorize hook itself errors, got %d: %s", status, body)
	}
}

// TestAuthorizeHookReceivesAggregateCommandIdAndPayload proves the hook
// sees exactly what the request named — the whole point of the hook is to
// let a policy table keyed on (aggregate, command) decide, and a scope/
// ownership policy needs the real id and payload too.
func TestAuthorizeHookReceivesAggregateCommandIdAndPayload(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	var gotAggregate, gotCommand, gotID string
	var gotPayload []byte
	srv := newTestGatewayWithConfig(t, store, gateway.Config{
		AllowAnonymous: true,
		Authorize: func(_ *core.RequestEvent, aggregate, command, id string, payload []byte) (bool, error) {
			gotAggregate, gotCommand, gotID, gotPayload = aggregate, command, id, payload
			return true, nil
		},
	})
	if status, body := postCreate(t, srv, ""); status != 200 {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	if gotAggregate != "task" || gotCommand != "Create" || gotID != "t1" || string(gotPayload) != "{}" {
		t.Fatalf("Authorize should see (task, Create, t1, {}), got (%s, %s, %s, %s)",
			gotAggregate, gotCommand, gotID, gotPayload)
	}
}

var errBoom = errAuthorizeBoom{}

type errAuthorizeBoom struct{}

func (errAuthorizeBoom) Error() string { return "boom: read-model lookup failed" }
