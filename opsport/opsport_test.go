package opsport

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/nodeidentity"
)

var started = time.Date(2026, 9, 29, 8, 30, 12, 345_600_000, time.FixedZone("BST", 3600))

func startOn(t *testing.T, h *Health) *Server {
	t.Helper()
	s, err := Start(h, "127.0.0.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

// healthz fetches /healthz, checks the status code and returns the body both
// as a map (to see nulls and key order) and raw.
func healthz(t *testing.T, s *Server) (map[string]any, string) {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://%s/healthz", s.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	return body, string(raw)
}

func TestWhileBootingHealthzAnswersAliveWithTheUnresolvedFieldsNull(t *testing.T) {
	body, _ := healthz(t, startOn(t, New("node-3", started)))

	if body["status"] != "alive" || body["contract_version"] != "1.0" {
		t.Errorf("status/contract_version: %v", body)
	}
	for _, field := range []string{"node_id", "identity", "instance", "role"} {
		v, present := body[field]
		if !present || v != nil {
			t.Errorf("%s = %v (present %v), want null while booting", field, v, present)
		}
	}
	// known from process start, so present even while booting
	if body["host"] != "node-3" || body["stack"] != "pocketcqrs" || body["started_at"] != "2026-09-29T07:30:12.345Z" {
		t.Errorf("host/stack/started_at: %v", body)
	}
}

func TestOnceIdentityIsResolvedEveryFieldIsSetInTheContractsOrder(t *testing.T) {
	h := New("node-3", started)
	s := startOn(t, h)
	h.SetIdentity(nodeidentity.Identity{
		NodeID: "0192b5c4-7e1a-7c3e-9f00-5b2d8a1c4e77", Kind: nodeidentity.Persistent,
		Instance: "timesheets", Host: "node-3", Stack: nodeidentity.Stack, Role: "writer", StartedAt: started,
	})

	body, raw := healthz(t, s)

	order := regexp.MustCompile(`"(\w+)":`).FindAllStringSubmatch(raw, -1)
	var keys []string
	for _, m := range order {
		keys = append(keys, m[1])
	}
	want := []string{"status", "contract_version", "node_id", "identity", "instance", "host", "stack", "role", "started_at"}
	if fmt.Sprint(keys) != fmt.Sprint(want) {
		t.Errorf("keys %v, want %v", keys, want)
	}
	for k, v := range body {
		if v == nil {
			t.Errorf("%s is null after identity was resolved", k)
		}
	}
	if body["node_id"] != "0192b5c4-7e1a-7c3e-9f00-5b2d8a1c4e77" || body["identity"] != "persistent" ||
		body["instance"] != "timesheets" || body["role"] != "writer" {
		t.Errorf("identity fields: %v", body)
	}
}

func TestOnlyGETHealthzIsServed(t *testing.T) {
	s := startOn(t, New("node-3", started))
	resp, err := http.Post(fmt.Sprintf("http://%s/healthz", s.Addr()), "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz: status %d, want 405", resp.StatusCode)
	}
}

func TestAPortThatIsAlreadyTakenFailsTheBoot(t *testing.T) {
	first := startOn(t, New("a", started))
	if _, err := Start(New("b", started), "127.0.0.1", first.Addr().(*net.TCPAddr).Port); err == nil {
		t.Fatal("a second ops server bound the same port")
	}
}

func TestPortIsTheSettingElseTheDefault(t *testing.T) {
	for in, want := range map[string]int{"": DefaultPort, "8080": 8080, "0": 0, "65535": 65535} {
		if got, err := ParsePort(in); err != nil || got != want {
			t.Errorf("ParsePort(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"abc", "-1", "65536", " 80", "080"} {
		if _, err := ParsePort(bad); err == nil {
			t.Errorf("ParsePort(%q): want an error", bad)
		}
	}
}
