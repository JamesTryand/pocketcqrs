//go:build smoke

package smoke

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestANodeWhosePortIsTakenIsRestartedOnAnother covers serveOnFreeAddr's
// retry: the first address it is handed is already held, as happens when
// something takes a freeAddr port before the node binds it.
func TestANodeWhosePortIsTakenIsRestartedOnAnother(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	taken := held.Addr().String()

	calls := 0
	pickAddr = func(t *testing.T) string {
		calls++
		if calls == 1 {
			return taken
		}
		return freeAddr(t)
	}
	defer func() { pickAddr = freeAddr }()

	h := startBackend(t, nil)

	if calls != 2 || h.BackendURL == "http://"+taken {
		t.Fatalf("expected one retry on a new port; %d picks, serving at %s (taken: %s)", calls, h.BackendURL, taken)
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(h.DataDir), "backend.log"))
	if len(matches) != 1 {
		t.Fatalf("no first-attempt log next to %s", h.DataDir)
	}
	raw, _ := os.ReadFile(matches[0])
	if !portTaken(string(raw)) {
		t.Fatalf("the first attempt did not fail on its port:\n%s", strings.TrimSpace(string(raw)))
	}
}
