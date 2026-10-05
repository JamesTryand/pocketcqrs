package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/gateway"
	"github.com/jamestryand/pocketcqrs/kms"
)

// probeProtector fails Protect with the key-service outcome named by the
// event type, so each mapping is driven through the real shell.
type probeProtector struct{}

func (probeProtector) Reveal(_ context.Context, s struct{}, cmd decider.Command) (struct{}, decider.Command, error) {
	return s, cmd, nil
}

func (probeProtector) Protect(_ context.Context, _ string, evs []events.NewEvent) ([]events.NewEvent, error) {
	switch evs[0].Type {
	case "Erased":
		return nil, &kms.SubjectErasedError{SubjectID: "s1"}
	case "Down":
		return nil, &kms.UnavailableError{Op: "PUT v1/subjects/s1/key", Err: errors.New("connection refused")}
	case "Unwired":
		return nil, &kms.KeyNotFoundError{SubjectID: "s1"}
	}
	return evs, nil
}

func newPIIGateway(t *testing.T) *httptest.Server {
	t.Helper()
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)

	registry := decider.NewRegistry(store)
	decider.RegisterProtected(registry, "probe", &decider.Decider[struct{}]{
		InitialState: func() struct{} { return struct{}{} },
		Decide: func(cmd decider.Command, _ struct{}) ([]events.NewEvent, error) {
			return []events.NewEvent{{Type: cmd.Name, Data: json.RawMessage(`{}`)}}, nil
		},
		Evolve: func(s struct{}, _ events.Event) (struct{}, error) { return s, nil },
	}, probeProtector{})

	pbRouter, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	gateway.RegisterRoutes(&core.ServeEvent{App: app, Router: pbRouter}, registry, gateway.Config{AllowAnonymous: true})
	mux, err := pbRouter.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func postProbe(t *testing.T, srv *httptest.Server, id, command, body string) (int, http.Header, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/api/cqrs/probe/"+id+"/"+command, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

func TestGatewayAcceptsPlaintextPersonalData(t *testing.T) {
	srv := newPIIGateway(t)
	if status, _, body := postProbe(t, srv, "p1", "Ok", `{"email":"alice@example.com"}`); status != 200 {
		t.Fatalf("plaintext is how personal data arrives: %d %s", status, body)
	}
}

func TestGatewayRefusesAnEnvelopeAnywhere(t *testing.T) {
	srv := newPIIGateway(t)
	for _, body := range []string{
		`{"email":{"$pii":{"s":"s1","c":"vault:v1:x"}}}`,
		`{"people":[{"email":{"$pii":{"s":"s1","c":"x"}}}]}`,
		`{"email":{"\u0024pii":{"s":"s1","c":"x"}}}`, // escaped key: \u0024 is $
		`{"$pii":{"s":"s1","c":"x"},"other":1}`,
	} {
		status, _, resp := postProbe(t, srv, "p1", "Ok", body)
		if status != 400 {
			t.Errorf("%s: want 400, got %d %s", body, status, resp)
		}
	}
}

func TestGatewayAllowsTheMarkerAsAValueOrANearMissKey(t *testing.T) {
	srv := newPIIGateway(t)
	for i, body := range []string{`{"note":"$pii"}`, `{"$piix":1}`, `{"x":["$pii"]}`} {
		if status, _, resp := postProbe(t, srv, "n"+string(rune('a'+i)), "Ok", body); status != 200 {
			t.Errorf("%s: want 200, got %d %s", body, status, resp)
		}
	}
}

func TestGatewayMapsKeyServiceOutcomes(t *testing.T) {
	srv := newPIIGateway(t)
	cases := []struct {
		command string
		want    int
	}{
		{"Erased", http.StatusGone},                 // permanent: a returning person is a new subject
		{"Down", http.StatusServiceUnavailable},     // nothing applied; retry
		{"Unwired", http.StatusInternalServerError}, // a wiring fault, not the caller's
	}
	for _, c := range cases {
		status, hdr, body := postProbe(t, srv, "k-"+c.command, c.command, `{}`)
		if status != c.want {
			t.Errorf("%s: want %d, got %d %s", c.command, c.want, status, body)
		}
		if c.want == http.StatusServiceUnavailable && hdr.Get("Retry-After") == "" {
			t.Errorf("%s: a 503 must carry Retry-After", c.command)
		}
	}
}
