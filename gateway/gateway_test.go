package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
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
	return newTestGatewayWithConfig(t, store, gateway.Config{AllowAnonymous: true, Idempotency: idem})
}

func newTestGatewayWithConfig(t *testing.T, store *events.Store, cfg gateway.Config) *httptest.Server {
	t.Helper()
	srv, _ := newTestGatewayWithConfigAndApp(t, store, cfg)
	return srv
}

// newTestGatewayWithConfigAndApp is newTestGatewayWithConfig, also
// returning the underlying *tests.TestApp -- needed by any test that must
// create an auth record (an external-caller identity, or an ordinary user)
// to authenticate a request as, rather than relying on AllowAnonymous.
func newTestGatewayWithConfigAndApp(t *testing.T, store *events.Store, cfg gateway.Config) (*httptest.Server, *tests.TestApp) {
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
	gateway.RegisterRoutes(&core.ServeEvent{App: app, Router: pbRouter}, registry, cfg)

	mux, err := pbRouter.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, app
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

// postCreateWithHeaders is postCreate, but lets a test set arbitrary
// headers (a bearer token, Causation-Id/Correlation-Id) on a fresh
// aggregate id each call, so tests exercising actorMeta's derivation don't
// collide with each other or with postCreate's own fixed "t1".
func postCreateWithHeaders(t *testing.T, srv *httptest.Server, aggregateID string, headers map[string]string) (status int, body string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/cqrs/task/"+aggregateID+"/Create", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
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

// newAuthRecord creates collectionName (an auth collection, created fresh
// if it does not already exist) with a "name" text field, a record in it
// with that field set, and mints a real auth token for the record --
// exactly the shape a recognized external-caller identity (or an ordinary
// end user, for the negative-case tests) takes at the gateway.
func newAuthRecord(t *testing.T, app core.App, collectionName, name string) (*core.Record, string) {
	t.Helper()
	rec, token := newAuthRecordWithEmail(t, app, collectionName, name+"@example.com")
	rec.Set("name", name)
	if err := app.Save(rec); err != nil {
		t.Fatalf("saving record in %s: %v", collectionName, err)
	}
	return rec, token
}

// newAuthRecordWithEmail is newAuthRecord's shared setup, without ever
// setting "name" -- used directly by
// TestActorMetaFallsBackToRecordIdWhenNameEmpty to prove that omission
// degrades gracefully rather than erroring.
func newAuthRecordWithEmail(t *testing.T, app core.App, collectionName, email string) (*core.Record, string) {
	t.Helper()
	col, err := app.FindCollectionByNameOrId(collectionName)
	if err != nil {
		col = core.NewAuthCollection(collectionName)
		col.Fields.Add(&core.TextField{Name: "name"})
		if err := app.Save(col); err != nil {
			t.Fatalf("creating collection %s: %v", collectionName, err)
		}
	}
	rec := core.NewRecord(col)
	rec.SetEmail(email)
	rec.SetPassword("1234567890")
	if err := app.Save(rec); err != nil {
		t.Fatalf("saving record in %s: %v", collectionName, err)
	}
	token, err := rec.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	return rec, token
}

// firstEventMeta decodes {"events":[...]} and returns the first event's
// full metadata map, or fails the test -- the shape every actorMeta
// assertion below needs to check (actor, actorCollection, causationId,
// correlationId, whichever the test cares about).
func firstEventMeta(t *testing.T, body string) map[string]any {
	t.Helper()
	var decoded struct {
		Events []struct {
			Metadata json.RawMessage `json:"metadata"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decoding response body: %v\n%s", err, body)
	}
	if len(decoded.Events) == 0 {
		t.Fatalf("no events in response: %s", body)
	}
	var meta map[string]any
	if err := json.Unmarshal(decoded.Events[0].Metadata, &meta); err != nil {
		t.Fatalf("decoding event metadata: %v\n%s", err, decoded.Events[0].Metadata)
	}
	return meta
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

func TestGatewayForwardsToConfiguredTarget(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"events":[{"type":"FromMaster"}]}`))
	}))
	defer master.Close()

	masterURL, err := url.Parse(master.URL)
	if err != nil {
		t.Fatal(err)
	}

	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	srv := newTestGatewayWithConfig(t, store, gateway.Config{
		AllowAnonymous: true,
		Forward:        httputil.NewSingleHostReverseProxy(masterURL),
	})

	status, body := postCreate(t, srv, "")
	if status != http.StatusOK {
		t.Fatalf("expected the proxied 200, got %d: %s", status, body)
	}
	if !strings.Contains(body, "FromMaster") {
		t.Fatalf("expected the master's response verbatim, got: %s", body)
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("expected the master to see POST, got %s", gotMethod)
	}
	if gotPath != "/api/cqrs/task/t1/Create" {
		t.Fatalf("expected the original path forwarded verbatim, got %s", gotPath)
	}
	if gotBody != "{}" {
		t.Fatalf("expected the original body forwarded verbatim, got %q", gotBody)
	}

	// the local decider must never have run: no event exists in this
	// node's own store, since Forward short-circuits before any local
	// decide/append is attempted
	stream, err := store.LoadStream(context.Background(), "task", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(stream) != 0 {
		t.Fatalf("expected no local events (forwarding must bypass local decide), got %d", len(stream))
	}
}

func TestGatewayForwardSkipsLocalAuthCheck(t *testing.T) {
	reached := false
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"events":[]}`))
	}))
	defer master.Close()

	masterURL, err := url.Parse(master.URL)
	if err != nil {
		t.Fatal(err)
	}

	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	// AllowAnonymous is deliberately false: without Forward this would
	// require a valid token. With Forward set, no local auth check should
	// ever run -- the destination is the one that authenticates.
	srv := newTestGatewayWithConfig(t, store, gateway.Config{
		AllowAnonymous: false,
		Forward:        httputil.NewSingleHostReverseProxy(masterURL),
	})

	status, body := postCreate(t, srv, "") // no Authorization header
	if status != http.StatusOK {
		t.Fatalf("expected the request to reach the forward target despite no local auth, got %d: %s", status, body)
	}
	if !reached {
		t.Fatal("the forward target was never reached")
	}
}

// TestActorMetaStampsExtcallPrefixForRecognizedCaller and its siblings below
// cover item 7/8: a recognized external-caller identity (a record in
// Config.ExternalCallerCollection) gets its commands' actor stamped as
// "extcall:<name>" and may additionally supply Causation-Id/Correlation-Id;
// an ordinary caller in any other collection gets neither, unchanged from
// before either field existed.
func TestActorMetaStampsExtcallPrefixForRecognizedCaller(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	srv, app := newTestGatewayWithConfigAndApp(t, store, gateway.Config{
		ExternalCallerCollection: "service_accounts",
	})
	_, token := newAuthRecord(t, app, "service_accounts", "orders-sync")

	status, body := postCreateWithHeaders(t, srv, "t1", map[string]string{
		"Authorization": "Bearer " + token,
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	meta := firstEventMeta(t, body)
	if meta["actor"] != "extcall:orders-sync" {
		t.Fatalf("expected actor \"extcall:orders-sync\", got %v (full meta: %v)", meta["actor"], meta)
	}
}

func TestActorMetaLeavesOrdinaryCallerUnchanged(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	srv, app := newTestGatewayWithConfigAndApp(t, store, gateway.Config{
		ExternalCallerCollection: "service_accounts",
	})
	// an ordinary user, in a DIFFERENT collection than the recognized one
	rec, token := newAuthRecord(t, app, "users", "alice")

	status, body := postCreateWithHeaders(t, srv, "t1", map[string]string{
		"Authorization": "Bearer " + token,
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	meta := firstEventMeta(t, body)
	if meta["actor"] != rec.Id {
		t.Fatalf("expected the raw record id %q as actor (unchanged behavior), got %v", rec.Id, meta["actor"])
	}
	if meta["actorCollection"] != "users" {
		t.Fatalf("expected actorCollection \"users\", got %v", meta["actorCollection"])
	}
}

func TestActorMetaHonorsCausationAndCorrelationForRecognizedCaller(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	srv, app := newTestGatewayWithConfigAndApp(t, store, gateway.Config{
		ExternalCallerCollection: "service_accounts",
	})
	_, token := newAuthRecord(t, app, "service_accounts", "orders-sync")

	status, body := postCreateWithHeaders(t, srv, "t1", map[string]string{
		"Authorization":  "Bearer " + token,
		"Causation-Id":   "evt-123",
		"Correlation-Id": "corr-456",
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	meta := firstEventMeta(t, body)
	if meta["causationId"] != "evt-123" {
		t.Fatalf("expected causationId \"evt-123\", got %v", meta["causationId"])
	}
	if meta["correlationId"] != "corr-456" {
		t.Fatalf("expected correlationId \"corr-456\", got %v", meta["correlationId"])
	}
}

// The gating is the point of item 8's design: an arbitrary authenticated
// caller must not be able to fabricate the causation graph ReactorFlows and
// the catalog explorer display -- only a recognized external caller may.
func TestActorMetaIgnoresCausationAndCorrelationForOrdinaryCaller(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	srv, app := newTestGatewayWithConfigAndApp(t, store, gateway.Config{
		ExternalCallerCollection: "service_accounts",
	})
	_, token := newAuthRecord(t, app, "users", "alice")

	status, body := postCreateWithHeaders(t, srv, "t1", map[string]string{
		"Authorization":  "Bearer " + token,
		"Causation-Id":   "evt-123",
		"Correlation-Id": "corr-456",
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	meta := firstEventMeta(t, body)
	if _, present := meta["causationId"]; present {
		t.Fatalf("an ordinary caller's Causation-Id header was honored: %v", meta)
	}
	if _, present := meta["correlationId"]; present {
		t.Fatalf("an ordinary caller's Correlation-Id header was honored: %v", meta)
	}
}

// Config.ExternalCallerCollection empty (the default) must leave every
// existing test above unaffected -- pinned directly, not just implied by
// the other tests still passing.
func TestActorMetaUnsetLeavesBehaviorUnchanged(t *testing.T) {
	srv := newTestGateway(t, nil) // AllowAnonymous: true, no ExternalCallerCollection

	status, _ := postCreate(t, srv, "")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
}

// A recognized-collection record with no "name" field set must degrade to
// the raw record id, not fail the request -- documented in Config.
// ExternalCallerCollection's own comment as a deliberate default (this
// package has no logger to warn through), pinned here so it can't quietly
// change into an error or an empty actor instead.
func TestActorMetaFallsBackToRecordIdWhenNameEmpty(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	srv, app := newTestGatewayWithConfigAndApp(t, store, gateway.Config{
		ExternalCallerCollection: "service_accounts",
	})
	rec, token := newAuthRecordWithEmail(t, app, "service_accounts", "noname@example.com")

	status, body := postCreateWithHeaders(t, srv, "t1", map[string]string{
		"Authorization": "Bearer " + token,
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200 (degrade, don't fail), got %d: %s", status, body)
	}
	meta := firstEventMeta(t, body)
	if meta["actor"] != rec.Id {
		t.Fatalf("expected the raw record id %q as actor when \"name\" is empty, got %v", rec.Id, meta["actor"])
	}
}

// Pins the invariant actorMeta's own comment documents: an ordinary
// caller's actor (a bare PocketBase record id) never collides with the
// "reactor:"/"extcall:" prefixes ReactorFlows and catalog.go's
// consumer-Kind switch match on, which is what lets those prefixes double
// as an automation marker with no separate typed field.
func TestOrdinaryCallerActorNeverMatchesAutomationPrefixes(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	srv, app := newTestGatewayWithConfigAndApp(t, store, gateway.Config{
		ExternalCallerCollection: "service_accounts",
	})
	_, token := newAuthRecord(t, app, "users", "alice")

	status, body := postCreateWithHeaders(t, srv, "t1", map[string]string{
		"Authorization": "Bearer " + token,
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	meta := firstEventMeta(t, body)
	actor, _ := meta["actor"].(string)
	if strings.HasPrefix(actor, "reactor:") || strings.HasPrefix(actor, "extcall:") {
		t.Fatalf("an ordinary caller's actor collided with an automation prefix: %q", actor)
	}
}
