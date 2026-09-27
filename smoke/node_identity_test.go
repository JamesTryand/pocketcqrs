//go:build smoke

package smoke

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The node-identity contract (platform/cqrs-runtime-contract,
// contracts/node-identity.md) at the process level: resolution itself is
// unit-tested in package nodeidentity against every input combination;
// these check the binary wires it in -- resolved at serve, stored in the
// data dir, kept across a restart, and an invalid assignment failing boot.

var identityLine = regexp.MustCompile(`node identity: node_id=(\S+) identity=(\S+) instance="[^"]*" host=\S* role=(\S+) started_at=(\S+)`)

// bootOnce serves over dataDir until healthy, stops, and returns the
// node-identity log line's node_id, identity, role and started_at.
func bootOnce(t *testing.T, bin, dir, dataDir, label string, extra ...string) (nodeID, kind, role, startedAt string) {
	t.Helper()
	addr := freeAddr(t)
	args := append([]string{"serve", "--http", addr, "--dir", dataDir,
		"--functionsDir", filepath.Join(dir, "pb_functions")}, extra...)
	stop := serve(t, bin, dir, label, args...)
	waitFor(t, "http://"+addr+"/api/health")
	stop()
	raw, err := os.ReadFile(filepath.Join(dir, label+".log"))
	if err != nil {
		t.Fatal(err)
	}
	m := identityLine.FindAllStringSubmatch(string(raw), -1)
	if len(m) != 1 {
		t.Fatalf("%s: want exactly one node-identity log line, got %d\n%s", label, len(m), raw)
	}
	return m[0][1], m[0][2], m[0][3], m[0][4]
}

func TestNodeIdentitySurvivesRestart(t *testing.T) {
	t.Setenv("CQRS_NODE_ID", "")
	dir := t.TempDir()
	bin := build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(dir, "pocketcqrs"))
	dataDir := filepath.Join(dir, "pb_data")

	id1, kind1, role1, started1 := bootOnce(t, bin, dir, dataDir, "boot1")
	if kind1 != "persistent" || role1 != "writer" {
		t.Fatalf("first boot: identity=%s role=%s, want persistent/writer", kind1, role1)
	}
	stored, err := os.ReadFile(filepath.Join(dataDir, "node-id"))
	if err != nil || strings.TrimSpace(string(stored)) != id1 {
		t.Fatalf("pb_data/node-id = %q (%v), want %s", stored, err, id1)
	}
	time.Sleep(5 * time.Millisecond) // started_at has millisecond resolution

	id2, kind2, _, started2 := bootOnce(t, bin, dir, dataDir, "boot2")
	if id2 != id1 || kind2 != "persistent" {
		t.Errorf("restart: node_id=%s identity=%s, want %s/persistent", id2, kind2, id1)
	}
	if started2 == started1 {
		t.Errorf("started_at did not change across the restart (%s)", started1)
	}
}

func TestAssignedNodeIDWinsAndWritesNothing(t *testing.T) {
	t.Setenv("CQRS_NODE_ID", "orders-replica-2")
	dir := t.TempDir()
	bin := build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(dir, "pocketcqrs"))
	dataDir := filepath.Join(dir, "pb_data")

	id, kind, _, _ := bootOnce(t, bin, dir, dataDir, "assigned")
	if id != "orders-replica-2" || kind != "assigned" {
		t.Errorf("node_id=%s identity=%s, want orders-replica-2/assigned", id, kind)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "node-id")); !os.IsNotExist(err) {
		t.Errorf("an assigned node wrote pb_data/node-id (stat err = %v)", err)
	}
}

func TestInvalidNodeIDFailsBoot(t *testing.T) {
	t.Setenv("CQRS_NODE_ID", "orders.replica 2")
	dir := t.TempDir()
	bin := build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(dir, "pocketcqrs"))
	dataDir := filepath.Join(dir, "pb_data")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "serve", "--http", freeAddr(t), "--dir", dataDir,
		"--functionsDir", filepath.Join(dir, "pb_functions"))
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the node kept running with an invalid CQRS_NODE_ID; it must refuse to start\n%s", out)
	}
	if err == nil {
		t.Fatalf("expected a non-zero exit, got success\n%s", out)
	}
	if !strings.Contains(string(out), "CQRS_NODE_ID") {
		t.Fatalf("the refusal must name CQRS_NODE_ID; output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "node-id")); !os.IsNotExist(err) {
		t.Errorf("a refused boot wrote pb_data/node-id (stat err = %v)", err)
	}
}
