//go:build smoke

package smoke

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
)

// The health/telemetry contract's replication freshness (section 5, machine
// 4) across two real processes: a secondary reads the master's heartbeat from
// the shared events.db, reports ready while it is fresh, and once the master
// stops (so the heartbeat goes stale and the master's /healthz stops
// answering) stays in the pool as degraded, replication_stale, rather than
// leaving it.
func TestSecondaryReportsReplicationFromTheMastersHeartbeat(t *testing.T) {
	master := startBackend(t, nil)
	_, opsPort, err := net.SplitHostPort(freeAddr(t))
	if err != nil {
		t.Fatal(err)
	}
	startSecondary(t, master, "--cqrsOpsPort", opsPort, "--cqrsStaleThreshold", "2s")

	readyz := func() (int, map[string]any) {
		resp, err := http.Get("http://127.0.0.1:" + opsPort + "/readyz")
		if err != nil {
			return 0, nil
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}
	reasons := func(body map[string]any) string {
		var out []string
		list, _ := body["reasons"].([]any)
		for _, r := range list {
			s, _ := r.(string)
			out = append(out, s)
		}
		return strings.Join(out, ",")
	}

	var last map[string]any
	eventually(t, "the secondary to be ready on a fresh heartbeat", func() bool {
		code, body := readyz()
		last = body
		return code == http.StatusOK && body["status"] == "ready" && body["role"] == "reader"
	})
	// the master is a required dependency of every secondary, found through
	// its heartbeat's ops URL (the loopback address it binds, here)
	if deps, _ := last["checks"].(map[string]any)["dependencies"].(map[string]any); deps["writer"] != "up" || deps["event_store"] != "up" {
		t.Errorf("dependencies: %v", last["checks"])
	}
	if checks, _ := last["checks"].(map[string]any); checks == nil || checks["write_lag_seconds"].(float64) > 2 {
		t.Errorf("fresh write_lag_seconds: %v", last["checks"])
	}

	master.stop()

	eventually(t, "the secondary to report a stale heartbeat with the master down as degraded", func() bool {
		code, body := readyz()
		last = body
		// replication_stale first; dependency_unavailable joins it once the
		// master has failed enough consecutive checks
		return code == http.StatusOK && body["status"] == "degraded" && strings.HasPrefix(reasons(body), "replication_stale")
	})
}
