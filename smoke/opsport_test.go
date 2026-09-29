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
)

// The health/telemetry contract's ops port, sections 2 and 3: `serve` binds
// it before anything else, /healthz there reports the node's identity, and
// /healthz is never on the traffic port.
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
}
