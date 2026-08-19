//go:build smoke

package smoke

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/events"
)

// litestreamBin locates a real litestream binary for this test to drive as a
// subprocess -- LITESTREAM_BIN first (an explicit path, for local/CI setups
// that build or vendor one), then PATH. Skips rather than failing when
// neither is available: litestream is deliberately not a pocketcqrs
// dependency (litestream-vfs-scope.md), so most environments won't have it,
// and this test's value is proving the real wiring works where it IS
// available, not gating the rest of the suite on installing it everywhere.
func litestreamBin(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("LITESTREAM_BIN"); bin != "" {
		return bin
	}
	if bin, err := exec.LookPath("litestream"); err == nil {
		return bin
	}
	t.Skip("no litestream binary available (set LITESTREAM_BIN or put litestream on PATH) -- see litestream-replication-plan.md")
	return ""
}

// startLitestreamProcess runs a litestream subcommand as a long-lived
// subprocess, logging to dir/label.log the same way serve() does for
// pocketcqrs processes, and registers cleanup to kill it.
func startLitestreamProcess(t *testing.T, bin, dir, label string, args ...string) {
	t.Helper()
	logPath := filepath.Join(dir, label+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", label, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		logFile.Close()
		if t.Failed() {
			if raw, err := os.ReadFile(logPath); err == nil && len(raw) > 0 {
				t.Logf("---- %s log ----\n%s", label, raw)
			}
		}
	})
}

// waitForFile polls until path exists or the test times out -- the
// sequencing dependency litestream-replication-plan.md's Step 3 flags:
// pocketcqrs's own secondary process must not start reading a path
// Litestream's restore -f sidecar hasn't produced yet.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

// litestreamConfig writes a minimal litestream.yml replicating masterEvents
// to a plain local/network directory target (litestream-replication-plan.md
// Step 2's recommended starting target -- no cloud dependency). Both the
// master's `replicate` and the secondary's `restore -f` share this same
// file: `restore`'s config-based lookup matches its positional argument
// against this file's `dbs[].path` by exact string equality (confirmed
// directly against litestream/cmd/litestream/main.go's DBConfig), so the key
// must be byte-identical to what's passed as that argument.
func litestreamConfig(t *testing.T, dir, masterEventsPath, replicaDir string) string {
	t.Helper()
	cfgPath := filepath.Join(dir, "litestream.yml")
	cfg := fmt.Sprintf("dbs:\n  - path: %s\n    replica:\n      path: %s\n", masterEventsPath, replicaDir)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// TestSecondaryReplicatesViaLitestream is the end-to-end proof for
// litestream-replication-plan.md's Steps 2-3: a real `litestream replicate`
// sidecar on the master and a real `litestream restore -f` sidecar producing
// a secondary's local copy (simulating a second host locally -- separate
// directories, no path shared directly the way startSecondary's plain-file
// case uses) actually deliver a committed event to a full secondary node's
// read path (events feed + local projection), through the real chosen
// mechanism rather than the plain-file shortcut
// TestSecondaryPollsMasterEventsReadOnly exercises.
//
// Deliberately does NOT also assert a forwarded command's round-trip back
// through Litestream within this test: that surfaced a real, reproducible
// stall (NEEDS.md, "Litestream restore -f's follow loop stalls under a
// concurrent reader") which is a separate, currently-open problem, not a
// wiring bug in this test. See TestLitestreamFollowStallsUnderConcurrentReader
// for the minimal, pocketcqrs-independent reproduction.
func TestSecondaryReplicatesViaLitestream(t *testing.T) {
	bin := litestreamBin(t)

	master := startBackend(t, nil) // --tutorial
	master.command("task", "t1", "CreateTask", map[string]string{"title": "via litestream"})

	workDir := t.TempDir()
	masterEventsPath := filepath.Join(master.DataDir, "events.db")
	replicaDir := filepath.Join(workDir, "replica")
	secondaryEventsPath := filepath.Join(workDir, "secondary-events.db")

	cfgPath := litestreamConfig(t, workDir, masterEventsPath, replicaDir)

	startLitestreamProcess(t, bin, workDir, "litestream-replicate",
		"replicate", "-config", cfgPath)

	// give replicate a moment to ship the initial snapshot before restore
	// looks for one -- restore -f's own retry/backoff would eventually find
	// it either way, but this keeps the test's own timeout budget for the
	// parts that matter.
	time.Sleep(2 * time.Second)

	startLitestreamProcess(t, bin, workDir, "litestream-restore",
		"restore", "-f", "-follow-interval", "500ms", "-config", cfgPath,
		"-o", secondaryEventsPath, masterEventsPath)

	waitForFile(t, secondaryEventsPath)

	// a full secondary node reading via the Litestream-followed copy, with
	// forwarding also wired (item 3) -- Step 4's combination. Forwarding
	// itself is verified below (the command lands on master); its
	// replicate-back leg is deliberately not asserted here, see the doc
	// comment above.
	secondary := startSecondaryAt(t, master, secondaryEventsPath, "--cqrsMasterAddr", master.BackendURL)

	// read replication actually flows through the real mechanism, not the
	// plain-file shortcut.
	eventually(t, "the secondary to see t1 via a real Litestream cycle", func() bool {
		var feed struct {
			Events []struct{ AggregateID string } `json:"events"`
		}
		status, _ := secondary.api(http.MethodGet, "/api/cqrs/events?aggregate=task", nil, &feed)
		if status != http.StatusOK {
			return false
		}
		for _, e := range feed.Events {
			if e.AggregateID == "t1" {
				return true
			}
		}
		return false
	})

	// and the secondary's own local projection folded it, same as the
	// plain-file case -- proves the Litestream path doesn't change anything
	// downstream of OpenReadOnly.
	eventually(t, "the secondary's local tasks collection to have t1", func() bool {
		var records struct {
			Items []struct {
				TaskID string `json:"taskId"`
				Title  string `json:"title"`
			} `json:"items"`
		}
		status, _ := secondary.api(http.MethodGet, "/api/collections/tasks/records?filter=taskId='t1'", nil, &records)
		return status == http.StatusOK && len(records.Items) == 1 && records.Items[0].Title == "via litestream"
	})

	// forwarding itself (item 3) still works normally alongside a
	// Litestream-followed read path -- a command sent to the secondary
	// lands on master. Its replication back to the secondary is NOT
	// asserted here (see the doc comment above).
	resp := secondary.do(http.MethodPost, secondary.BackendURL+"/api/cqrs/task/t2/CreateTask",
		jsonBody(map[string]string{"title": "forwarded via litestream secondary"}),
		map[string]string{"Authorization": master.Token})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from the forwarded command, got %d", resp.StatusCode)
	}
	var masterStreams struct {
		Streams []struct{ AggregateID string } `json:"streams"`
	}
	master.apiOK(http.MethodGet, "/api/cqrs/streams?aggregate=task", nil, &masterStreams)
	found := false
	for _, s := range masterStreams.Streams {
		if s.AggregateID == "t2" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected t2 to exist on the master after a command forwarded through the Litestream-backed secondary")
	}
}

// TestLitestreamFollowStallsUnderConcurrentReader is a minimal,
// pocketcqrs-independent reproduction of a real bug found while building
// litestream-replication-plan.md's Steps 2-4, 2026-08-19: `litestream
// restore -f`'s follow loop can stall for tens of seconds -- far past its
// configured -follow-interval -- once something continuously holds/reopens
// a read connection against its output file. The master's own `replicate`
// process ships new transactions promptly throughout (confirmed via its own
// log, not assumed); the stall is entirely on the consuming side.
//
// Root-cause hypothesis, not yet confirmed on Linux (this was found and
// reproduced on Windows only): `restore -f`'s applyLTXFile needs a brief
// EXCLUSIVE lock to patch in new pages (litestream/replica.go:960-963), and
// the file it's patching is deliberately in DELETE/rollback-journal mode
// (see litestream-vfs-scope.md, "Why DELETE mode, not WAL"), whose locking
// model requires that exclusive lock to wait out every reader's SHARED
// lock. A continuously-repolling reader may never leave a large enough gap,
// starving the writer -- a classic rollback-journal-mode risk that WAL mode
// specifically exists to avoid, but this file is deliberately NOT in WAL
// mode. Unconfirmed whether this is Windows-specific (its file-locking
// fairness characteristics differ from POSIX) or a cross-platform risk.
//
// Skipped by default -- this documents a known, unresolved issue (see
// NEEDS.md, "Litestream restore -f's follow loop stalls under a concurrent
// reader") rather than a wiring bug this suite should gate on. Remove the
// Skip to reproduce or to verify a fix.
func TestLitestreamFollowStallsUnderConcurrentReader(t *testing.T) {
	t.Skip("known issue, not yet resolved -- see NEEDS.md; remove this Skip to reproduce")

	bin := litestreamBin(t)
	dir := t.TempDir()
	masterPath := filepath.Join(dir, "master-events.db")
	replicaDir := filepath.Join(dir, "replica")
	secondaryPath := filepath.Join(dir, "secondary-events.db")

	seedStore, err := events.Open(masterPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seedStore.Append(nil, "task", "seed", 0, []events.NewEvent{{Type: "Created"}}); err != nil {
		t.Fatal(err)
	}

	cfgPath := litestreamConfig(t, dir, masterPath, replicaDir)
	startLitestreamProcess(t, bin, dir, "replicate", "replicate", "-config", cfgPath)
	time.Sleep(2 * time.Second)
	startLitestreamProcess(t, bin, dir, "restore", "restore", "-f", "-follow-interval", "500ms",
		"-config", cfgPath, "-o", secondaryPath, masterPath)
	waitForFile(t, secondaryPath)

	// the concurrent reader: nothing but a bare OpenReadOnly + Poll loop,
	// no pocketcqrs server -- isolates the stall from anything
	// pocketcqrs-specific (command batching, consumers.Engine, HTTP, etc.,
	// were all ruled out as the cause during investigation).
	reader, err := events.OpenReadOnly(secondaryPath)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			reader.Poll(nil, 0, 100)
			time.Sleep(100 * time.Millisecond)
		}
	}()

	// append on master well after the reader is already polling --
	// reproduces the observed stall: this should land on the secondary
	// within ~1-2s (matching the master-side replicate log, confirmed
	// separately to ship promptly) but instead took 30-90+ seconds in every
	// investigation run.
	writer, err := events.Open(masterPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Append(nil, "task", "late", 0, []events.NewEvent{{Type: "Created"}}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		evs, _ := reader.Poll(nil, 1, 100)
		if len(evs) > 0 {
			return // fixed: replicated within a reasonable window
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the late append did not replicate within 10s under a concurrent reader -- known issue, see NEEDS.md")
}
