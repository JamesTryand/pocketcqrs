package gateway_test

// The conformance probe for the command gateway's HTTP contract v0.1
// (platform/cqrs-runtime-contract GATEWAY-HTTP-CONTRACT.md, frozen
// 2026-11-21). One case per G3 row this stack can reach, each checking the
// G4 problem body; the G5 reasons on the two generated refusals; G6's
// accepted body with and without events; G7's replay rules; G8's
// Retry-After on every 503; and G3's "a dependency failure before the
// decision is 503, not 500".
//
// Not reached here: 502 (no proxy of its own; forwarding is left out of
// v0.1), and the batching-only 503 (queue full) and 504 (batch timeout),
// whose status (and the 503's Retry-After) batching_test.go checks; their
// bodies come from the same problem writer as every case here.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/cmdauth"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/gateway"
	"github.com/jamestryand/pocketcqrs/idempotency"
	"github.com/jamestryand/pocketcqrs/kms"
)

// entState is the probe's aggregate, named like a generated one so its two
// generated refusals read exactly as the generators write them.
type entState struct {
	Exists bool
	Count  int64
}

type probe struct {
	t        *testing.T
	srv      *httptest.Server
	app      *tests.TestApp
	store    *events.Store
	path     string
	registry *decider.Registry
	idem     *idempotency.Store

	mode    string // events.ModeMaintenance turns maintenance on
	modeErr error  // the mode read fails
	allow   bool   // the Authorize hook's verdict
	authErr error  // the Authorize hook fails (or refuses with a *cmdauth.Refusal)
	inject  error  // what the Inject command's decide returns; nil: one event
	raced   bool   // the Race command has already raced once
}

func newProbe(t *testing.T, anonymous bool) *probe {
	t.Helper()
	p := &probe{t: t, allow: true}
	p.path = filepath.Join(t.TempDir(), "events.db")
	store, err := events.Open(p.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	p.store = store
	idem, err := idempotency.Open(filepath.Join(t.TempDir(), "idempotency.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idem.Close() })
	p.idem = idem

	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	p.app = app

	p.registry = decider.NewRegistry(store)
	decider.Register(p.registry, "entitlement", p.decider())

	cfg := gateway.Config{
		AllowAnonymous: anonymous,
		Idempotency:    idem,
		Mode: func(context.Context) (string, error) {
			return p.mode, p.modeErr
		},
		Authorize: func(*core.RequestEvent, string, string, string, []byte) (bool, error) {
			if p.authErr != nil {
				return false, p.authErr
			}
			return p.allow, nil
		},
	}
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	gateway.RegisterRoutes(&core.ServeEvent{App: app, Router: router}, p.registry, cfg)
	mux, err := router.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *probe) decider() *decider.Decider[entState] {
	ev := func(typ string) []events.NewEvent {
		return []events.NewEvent{{Type: typ, Data: json.RawMessage(`{"n":1}`)}}
	}
	return &decider.Decider[entState]{
		InitialState: func() entState { return entState{} },
		Decide: func(cmd decider.Command, s entState) ([]events.NewEvent, error) {
			switch cmd.Name {
			case "Grant": // a create, exactly as generated
				if s.Exists {
					return nil, fmt.Errorf("entitlement already exists")
				}
				return ev("EntitlementGranted"), nil
			case "Renew": // needs the stream, exactly as generated
				if !s.Exists {
					return nil, fmt.Errorf("entitlement does not exist")
				}
				return ev("EntitlementRenewed"), nil
			case "Touch": // decided, nothing to record
				return nil, nil
			case "Refuse": // a hand-written refusal, lower case, no full stop
				return nil, fmt.Errorf("amount must be positive")
			case "Owner":
				return nil, decider.Forbidden("only the owner may renew this entitlement")
			case "Race": // loses the race for the stream once
				if !p.raced {
					p.raced = true
					if _, err := p.store.Append(context.Background(), "entitlement", "race", s.Count,
						ev("Interloped")); err != nil {
						return nil, decider.Fault(err)
					}
				}
				return ev("EntitlementRenewed"), nil
			case "Inject":
				if p.inject != nil {
					return nil, p.inject
				}
				return ev("EntitlementRenewed"), nil
			}
			return nil, fmt.Errorf("unknown command: %s", cmd.Name)
		},
		Evolve: func(s entState, e events.Event) (entState, error) {
			s.Count++
			if e.Type == "EntitlementGranted" {
				s.Exists = true
			}
			return s, nil
		},
	}
}

type answer struct {
	status int
	header http.Header
	body   []byte
}

func (p *probe) post(path, body string, headers ...string) answer {
	p.t.Helper()
	req, err := http.NewRequest(http.MethodPost, p.srv.URL+"/api/cqrs/"+path, bytes.NewReader([]byte(body)))
	if err != nil {
		p.t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		p.t.Fatal(err)
	}
	return answer{status: resp.StatusCode, header: resp.Header, body: b}
}

// problem checks G4 on a non-2xx answer: the status, an RFC 9457 body with
// at least status, title and detail. It returns the body's members.
func problem(t *testing.T, a answer, status int) map[string]any {
	t.Helper()
	if a.status != status {
		t.Fatalf("want %d, got %d: %s", status, a.status, a.body)
	}
	if ct := a.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("G4: Content-Type must be application/problem+json, got %q (%s)", ct, a.body)
	}
	var m map[string]any
	if err := json.Unmarshal(a.body, &m); err != nil {
		t.Fatalf("G4: the body is not a JSON object: %v: %s", err, a.body)
	}
	if s, ok := m["status"].(float64); !ok || int(s) != status {
		t.Errorf("G4: status member must be %d, got %v (%s)", status, m["status"], a.body)
	}
	for _, k := range []string{"title", "detail"} {
		if v, ok := m[k].(string); !ok || v == "" {
			t.Errorf("G4: %s member must be a non-empty string, got %v (%s)", k, m[k], a.body)
		}
	}
	return m
}

// unavailable checks a 503: G4's body and G8's Retry-After in seconds.
func unavailable(t *testing.T, a answer) map[string]any {
	t.Helper()
	m := problem(t, a, http.StatusServiceUnavailable)
	if ra := a.header.Get("Retry-After"); ra == "" || strings.Trim(ra, "0123456789") != "" {
		t.Errorf("G8: a 503 must carry Retry-After in seconds, got %q", ra)
	}
	return m
}

// accepted checks G6 and returns the events.
func accepted(t *testing.T, a answer) []map[string]any {
	t.Helper()
	if a.status != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", a.status, a.body)
	}
	var body struct {
		Events *[]map[string]any `json:"events"`
	}
	if err := json.Unmarshal(a.body, &body); err != nil {
		t.Fatalf("G6: %v: %s", err, a.body)
	}
	if body.Events == nil {
		t.Fatalf("G6: events must be an array, never null or missing: %s", a.body)
	}
	for _, e := range *body.Events {
		for _, k := range []string{"type", "aggregate", "aggregateId"} {
			if v, ok := e[k].(string); !ok || v == "" {
				t.Errorf("G6: an event's %s must be a non-empty string: %s", k, a.body)
			}
		}
		if _, ok := e["data"].(map[string]any); !ok {
			t.Errorf("G6: an event's data must be a JSON object: %s", a.body)
		}
		if md, present := e["metadata"]; present {
			if _, ok := md.(map[string]any); !ok {
				t.Errorf("G6: an event's metadata must be a JSON object: %s", a.body)
			}
		}
	}
	return *body.Events
}

func streamLen(t *testing.T, p *probe, id string) int {
	t.Helper()
	s, err := p.store.LoadStream(context.Background(), "entitlement", id)
	if err != nil {
		t.Fatal(err)
	}
	return len(s)
}

// --- G3 rows, G4 bodies, G5 reasons, G6 ---

func TestContract200WithEvents(t *testing.T) {
	p := newProbe(t, true)
	got := accepted(t, p.post("entitlement/e1/Grant", `{}`))
	if len(got) != 1 || got[0]["type"] != "EntitlementGranted" || got[0]["aggregateId"] != "e1" {
		t.Fatalf("G6: want the one appended event, got %v", got)
	}
}

func TestContract200WithNoEvents(t *testing.T) {
	p := newProbe(t, true)
	a := p.post("entitlement/e1/Touch", ``)
	if got := accepted(t, a); len(got) != 0 {
		t.Fatalf("G6: want no events, got %v", got)
	}
	if !bytes.Contains(a.body, []byte(`"events":[]`)) {
		t.Fatalf(`G6: no events is "events":[], got %s`, a.body)
	}
}

func TestContract400DoesNotExist(t *testing.T) {
	p := newProbe(t, true)
	m := problem(t, p.post("entitlement/e1/Renew", `{}`), http.StatusBadRequest)
	if m["detail"] != "entitlement does not exist" {
		t.Errorf("G4: detail must be the decider's message verbatim, got %q", m["detail"])
	}
	if m["reason"] != "does-not-exist" {
		t.Errorf("G5: reason must be does-not-exist, got %v", m["reason"])
	}
}

func TestContract400AlreadyExists(t *testing.T) {
	p := newProbe(t, true)
	accepted(t, p.post("entitlement/e1/Grant", `{}`))
	m := problem(t, p.post("entitlement/e1/Grant", `{}`), http.StatusBadRequest)
	if m["detail"] != "entitlement already exists" {
		t.Errorf("G4: detail must be the decider's message verbatim, got %q", m["detail"])
	}
	if m["reason"] != "already-exists" {
		t.Errorf("G5: reason must be already-exists, got %v", m["reason"])
	}
}

func TestContract400OtherRefusalVerbatim(t *testing.T) {
	p := newProbe(t, true)
	m := problem(t, p.post("entitlement/e1/Refuse", `{}`), http.StatusBadRequest)
	if m["detail"] != "amount must be positive" {
		t.Errorf("G4: detail must be the decider's message verbatim (not re-cased or punctuated), got %q", m["detail"])
	}
	if r, ok := m["reason"]; ok && (r == "does-not-exist" || r == "already-exists") {
		t.Errorf("G5: a hand-written refusal must not claim a generated reason, got %v", r)
	}
}

func TestContract400Malformed(t *testing.T) {
	p := newProbe(t, true)
	problem(t, p.post("entitlement/e1/Grant", `{"email":{"$pii":"x"}}`), http.StatusBadRequest)
	problem(t, p.post("entitlement/e1/Grant", `{}`, "Idempotency-Key", strings.Repeat("k", 201)), http.StatusBadRequest)
	accepted(t, p.post("entitlement/e1/Grant", `{}`, "Idempotency-Key", strings.Repeat("k", 200)))
}

func TestContract401(t *testing.T) {
	p := newProbe(t, false)
	problem(t, p.post("entitlement/e1/Grant", `{}`), http.StatusUnauthorized)
	problem(t, p.post("entitlement/e1/Grant", `{}`, "Authorization", "not-a-token"), http.StatusUnauthorized)
	problem(t, p.post("entitlement/e1/Grant", `{}`, "Authorization", "Bearer not-a-token"), http.StatusUnauthorized)
	_, token := newAuthRecord(t, p.app, "users", "caller")
	accepted(t, p.post("entitlement/e1/Grant", `{}`, "Authorization", token)) // the bare token, as always
}

func TestContract403(t *testing.T) {
	p := newProbe(t, true)
	p.allow = false
	problem(t, p.post("entitlement/e1/Grant", `{}`), http.StatusForbidden)
	p.allow, p.authErr = true, &cmdauth.Refusal{Message: "only the seller may grant"}
	if m := problem(t, p.post("entitlement/e1/Grant", `{}`), http.StatusForbidden); m["detail"] != "only the seller may grant" {
		t.Errorf("G4: detail must be the declaration's message, got %q", m["detail"])
	}
	p.authErr = nil
	if m := problem(t, p.post("entitlement/e1/Owner", `{}`), http.StatusForbidden); m["detail"] != "only the owner may renew this entitlement" {
		t.Errorf("G4: detail must be the decider's message verbatim, got %q", m["detail"])
	}
}

func TestContract404(t *testing.T) {
	p := newProbe(t, true)
	problem(t, p.post("nosuch/e1/Grant", `{}`), http.StatusNotFound)
}

func TestContract409(t *testing.T) {
	p := newProbe(t, true)
	problem(t, p.post("entitlement/race/Race", `{}`), http.StatusConflict)
}

func TestContract410(t *testing.T) {
	p := newProbe(t, true)
	p.inject = &kms.SubjectErasedError{SubjectID: "s1"}
	problem(t, p.post("entitlement/e1/Inject", `{}`), http.StatusGone)
}

func TestContract422(t *testing.T) {
	p := newProbe(t, true)
	accepted(t, p.post("entitlement/e1/Grant", `{}`, "Idempotency-Key", "k1"))
	problem(t, p.post("entitlement/e1/Grant", `{"other":true}`, "Idempotency-Key", "k1"), http.StatusUnprocessableEntity)
}

func TestContract500(t *testing.T) {
	p := newProbe(t, true)
	p.inject = decider.Fault(errors.New("the decider never reached a verdict"))
	problem(t, p.post("entitlement/e1/Inject", `{}`), http.StatusInternalServerError)
}

// --- 503, each with Retry-After (G8); before the decision is 503 (G3) ---

func TestContract503Maintenance(t *testing.T) {
	p := newProbe(t, true)
	p.mode = events.ModeMaintenance
	unavailable(t, p.post("entitlement/e1/Grant", `{}`))
}

func TestContract503ModeReadFails(t *testing.T) {
	p := newProbe(t, true)
	p.modeErr = errors.New("system mode unreadable")
	unavailable(t, p.post("entitlement/e1/Grant", `{}`))
}

func TestContract503AuthorizeHookFails(t *testing.T) {
	p := newProbe(t, true)
	p.authErr = errors.New("the role lookup failed")
	unavailable(t, p.post("entitlement/e1/Grant", `{}`))
}

func TestContract503IdempotencyLookupFails(t *testing.T) {
	p := newProbe(t, true)
	p.idem.Close()
	unavailable(t, p.post("entitlement/e1/Grant", `{}`, "Idempotency-Key", "k1"))
}

func TestContract503StreamLoadFails(t *testing.T) {
	p := newProbe(t, true)
	accepted(t, p.post("entitlement/e1/Grant", `{}`))
	p.store.SetUpcaster(func(ev events.Event) (events.Event, error) {
		return ev, errors.New("the stream could not be loaded")
	})
	unavailable(t, p.post("entitlement/e1/Renew", `{}`))
}

func TestContract503KeyServiceDown(t *testing.T) {
	p := newProbe(t, true)
	p.inject = &kms.UnavailableError{Op: "seal", Err: errors.New("connection refused")}
	unavailable(t, p.post("entitlement/e1/Inject", `{}`))
}

func TestContract503ReadOnly(t *testing.T) {
	p := newProbe(t, true)
	p.inject = events.ErrReadOnly
	unavailable(t, p.post("entitlement/e1/Inject", `{}`))
}

func TestContract503StoreBusy(t *testing.T) {
	p := newProbe(t, true)
	other, err := sql.Open("sqlite", "file:"+p.path+"?_pragma=busy_timeout(0)")
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
	unavailable(t, p.post("entitlement/busy/Grant", `{}`))
}

// --- G7 ---

func TestContractReplays200Verbatim(t *testing.T) {
	p := newProbe(t, true)
	first := p.post("entitlement/e1/Grant", `{}`, "Idempotency-Key", "k1")
	accepted(t, first)
	again := p.post("entitlement/e1/Grant", `{}`, "Idempotency-Key", "k1")
	accepted(t, again)
	if !bytes.Equal(first.body, again.body) {
		t.Fatalf("G7: a replay must be the first 200 verbatim:\n first %s\n again %s", first.body, again.body)
	}
	if n := streamLen(t, p, "e1"); n != 1 {
		t.Fatalf("G7: a replay must not decide again, the stream has %d events", n)
	}
}

func TestContract409IsDecidedAgain(t *testing.T) {
	p := newProbe(t, true)
	problem(t, p.post("entitlement/race/Race", `{}`, "Idempotency-Key", "k1"), http.StatusConflict)
	accepted(t, p.post("entitlement/race/Race", `{}`, "Idempotency-Key", "k1"))
}

// A 409 recorded before this rule (the old gateway saved one) is not
// replayed either, and the 200 that follows is what gets kept.
func TestContractStale409RecordIsDecidedAgain(t *testing.T) {
	p := newProbe(t, true)
	hash := idempotency.Hash("entitlement", "e1", "Grant", `{}`)
	if err := p.idem.Save(context.Background(), "k1", hash, http.StatusConflict,
		json.RawMessage(`{"error":"events: concurrency conflict"}`)); err != nil {
		t.Fatal(err)
	}
	first := p.post("entitlement/e1/Grant", `{}`, "Idempotency-Key", "k1")
	accepted(t, first)
	again := p.post("entitlement/e1/Grant", `{}`, "Idempotency-Key", "k1")
	accepted(t, again)
	if !bytes.Equal(first.body, again.body) || streamLen(t, p, "e1") != 1 {
		t.Fatalf("G7: the 200 after a stale 409 must be the one replayed: %s / %s", first.body, again.body)
	}
}

func TestContractNeverRecorded(t *testing.T) {
	t.Run("400", func(t *testing.T) {
		p := newProbe(t, true)
		problem(t, p.post("entitlement/e1/Renew", `{}`, "Idempotency-Key", "k1"), http.StatusBadRequest)
		accepted(t, p.post("entitlement/e1/Grant", `{}`))
		accepted(t, p.post("entitlement/e1/Renew", `{}`, "Idempotency-Key", "k1"))
	})
	t.Run("401", func(t *testing.T) {
		p := newProbe(t, false)
		problem(t, p.post("entitlement/e1/Grant", `{}`, "Idempotency-Key", "k1"), http.StatusUnauthorized)
		_, token := newAuthRecord(t, p.app, "users", "caller")
		accepted(t, p.post("entitlement/e1/Grant", `{}`, "Idempotency-Key", "k1", "Authorization", "Bearer "+token))
	})
	t.Run("403", func(t *testing.T) {
		p := newProbe(t, true)
		p.allow = false
		problem(t, p.post("entitlement/e1/Grant", `{}`, "Idempotency-Key", "k1"), http.StatusForbidden)
		p.allow = true
		accepted(t, p.post("entitlement/e1/Grant", `{}`, "Idempotency-Key", "k1"))
	})
	t.Run("404", func(t *testing.T) {
		p := newProbe(t, true)
		problem(t, p.post("later/e1/Grant", `{}`, "Idempotency-Key", "k1"), http.StatusNotFound)
		decider.Register(p.registry, "later", p.decider())
		accepted(t, p.post("later/e1/Grant", `{}`, "Idempotency-Key", "k1"))
	})
	t.Run("410", func(t *testing.T) {
		p := newProbe(t, true)
		p.inject = &kms.SubjectErasedError{SubjectID: "s1"}
		problem(t, p.post("entitlement/e1/Inject", `{}`, "Idempotency-Key", "k1"), http.StatusGone)
		p.inject = nil
		accepted(t, p.post("entitlement/e1/Inject", `{}`, "Idempotency-Key", "k1"))
	})
	t.Run("503", func(t *testing.T) {
		p := newProbe(t, true)
		p.mode = events.ModeMaintenance
		unavailable(t, p.post("entitlement/e1/Grant", `{}`, "Idempotency-Key", "k1"))
		p.mode = ""
		p.inject = &kms.UnavailableError{Op: "seal", Err: errors.New("connection refused")}
		unavailable(t, p.post("entitlement/e1/Inject", `{}`, "Idempotency-Key", "k2"))
		p.inject = nil
		accepted(t, p.post("entitlement/e1/Grant", `{}`, "Idempotency-Key", "k1"))
		accepted(t, p.post("entitlement/e1/Inject", `{}`, "Idempotency-Key", "k2"))
	})
}

// A recorded answer is kept at least 7 days (G7). The host's
// --cqrsIdempotencyRetention defaults to this value; idempotency's own
// TestPruneKeepsSixDayOldRecordAtDefaultRetention prunes a backdated record.
func TestContractRecordsKeptSevenDays(t *testing.T) {
	if idempotency.DefaultRetention < 7*24*time.Hour {
		t.Fatalf("G7: records must be kept at least 7 days, the default is %s", idempotency.DefaultRetention)
	}
}
