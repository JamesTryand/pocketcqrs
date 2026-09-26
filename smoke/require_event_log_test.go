//go:build smoke

package smoke

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMasterRefusesMissingEventLogWhenRequired: with
// --cqrsRequireExistingEventLog a master whose events.db is missing (e.g.
// started before its LiteFS mount is up) refuses to start, rather than
// creating an empty log and serving as if history were empty. Without the
// flag the default is unchanged -- every other smoke test boots a fresh
// data dir and relies on the log being created.
func TestMasterRefusesMissingEventLogWhenRequired(t *testing.T) {
	dir := t.TempDir()
	bin := build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(dir, "pocketcqrs"))
	dataDir := filepath.Join(dir, "pb_data")
	eventsPath := filepath.Join(dir, "mnt", "events.db") // the "mount" is empty

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "serve", "--http", freeAddr(t), "--dir", dataDir,
		"--functionsDir", filepath.Join(dir, "pb_functions"),
		"--cqrsEventsPath", eventsPath, "--cqrsRequireExistingEventLog")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the master kept running with no event log; it must refuse to start\n%s", out)
	}
	if err == nil {
		t.Fatalf("expected a non-zero exit, got success\n%s", out)
	}
	if !strings.Contains(string(out), "event log does not exist") {
		t.Fatalf("refusal must name the cause; output:\n%s", out)
	}
	if _, err := os.Stat(eventsPath); !os.IsNotExist(err) {
		t.Fatalf("the refused start must not have created %s (stat err = %v)", eventsPath, err)
	}
}
