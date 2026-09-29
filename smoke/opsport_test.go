//go:build smoke

package smoke

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The health/telemetry contract's ops port, sections 2-4: `serve` binds it
// before anything else, /healthz there reports the node's identity, /healthz
// is never on the traffic port, and /readyz opens once the node has booted
// and caught up.
func TestOpsPortServesHealthzWithTheNodesIdentity(t *testing.T) {
	t.Setenv("CQRS_NODE_ID", "")
	dir := t.TempDir()
	bin := build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(dir, "pocketcqrs"))
	dataDir := filepath.Join(dir, "pb_data")
	seed := exec.Command(bin, "superuser", "upsert", superuserEmail, superuserPassword, "--dir", dataDir)
	if out, err := seed.CombinedOutput(); err != nil {
		t.Fatalf("seeding the superuser failed: %v\n%s", err, out)
	}

	addr := freeAddr(t)
	_, opsPort, err := net.SplitHostPort(freeAddr(t))
	if err != nil {
		t.Fatal(err)
	}
	serve(t, bin, dir, "ops", "serve", "--http", addr, "--dir", dataDir,
		"--functionsDir", filepath.Join(dir, "pb_functions"), "--cqrsOpsPort", opsPort)
	waitFor(t, "http://"+addr+"/api/health")

	resp, err := http.Get("http://127.0.0.1:" + opsPort + "/healthz")
	if err != nil {
		t.Fatalf("ops port: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz on the ops port: status %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(dataDir, "node-id"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"status": "alive", "contract_version": "1.0", "node_id": strings.TrimSpace(string(stored)),
		"identity": "persistent", "stack": "pocketcqrs", "role": "writer",
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("/healthz %s = %v, want %v (body %v)", k, body[k], v, body)
		}
	}
	for _, k := range []string{"instance", "host", "started_at"} {
		if s, _ := body[k].(string); s == "" {
			t.Errorf("/healthz %s is empty (body %v)", k, body)
		}
	}

	traffic, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	traffic.Body.Close()
	if traffic.StatusCode == http.StatusOK {
		t.Errorf("/healthz answered on the traffic port; it belongs on the ops port only")
	}

	// Section 4: the traffic port answers, so boot has completed; with
	// nothing to catch up on, /readyz opens promptly (it re-checks every
	// 250ms), with no reasons.
	var ready map[string]any
	code := 0
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		resp, err := http.Get("http://127.0.0.1:" + opsPort + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		code, ready = resp.StatusCode, nil
		_ = json.NewDecoder(resp.Body).Decode(&ready)
		resp.Body.Close()
		if code == http.StatusOK {
			break
		}
	}
	if code != http.StatusOK || ready["status"] != "ready" || ready["node_id"] != want["node_id"] {
		t.Fatalf("/readyz = %d %v, want 200 ready", code, ready)
	}
	if reasons, _ := ready["reasons"].([]any); reasons == nil || len(reasons) != 0 {
		t.Errorf("/readyz reasons = %v, want []", ready["reasons"])
	}
}
