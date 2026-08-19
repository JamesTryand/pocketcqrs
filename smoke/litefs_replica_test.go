//go:build smoke

package smoke

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// litefsBin locates a real litefs binary for this test to drive as a
// subprocess -- LITEFS_BIN first, then PATH. Skips (not fails) when
// unavailable, or on any OS other than Linux: LiteFS is FUSE-based, and FUSE
// is Linux-only (litefs-replication-plan.md, litestream-vfs-scope.md).
func litefsBin(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("litefs is FUSE-based and Linux-only -- see litefs-replication-plan.md")
	}
	if bin := os.Getenv("LITEFS_BIN"); bin != "" {
		return bin
	}
	if bin, err := exec.LookPath("litefs"); err == nil {
		return bin
	}
	t.Skip("no litefs binary available (set LITEFS_BIN or put litefs on PATH) -- see litefs-replication-plan.md")
	return ""
}

// startLitefsProcess runs `litefs mount` as a long-lived subprocess, logging
// to dir/label.log the same way serve() does for pocketcqrs processes, and
// registers cleanup to kill it and unmount its FUSE mount (killing the
// process alone does not guarantee the mount is released).
func startLitefsProcess(t *testing.T, bin, dir, label, configPath, fuseDir string) {
	t.Helper()
	logPath := filepath.Join(dir, label+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "mount", "-config", configPath)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", label, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = exec.Command("fusermount3", "-u", fuseDir).Run()
		logFile.Close()
		if t.Failed() {
			if raw, err := os.ReadFile(logPath); err == nil && len(raw) > 0 {
				t.Logf("---- %s log ----\n%s", label, raw)
			}
		}
	})
}

// litefsConfig writes a minimal litefs.yml for one node -- static lease (no
// Consul, matching this project's own fixed-master design, decided in
// litestream-vfs-scope.md/NEEDS.md). advertiseURL is always the PRIMARY's
// address, for every node including the primary itself -- confirmed shape,
// matches LiteFS's own TestMultiNode_StaticLeaser and this worktree's
// litefs-concurrent-reader-test.sh.
func litefsConfig(t *testing.T, dir, fuseDir, dataDir, httpAddr, advertiseURL string, candidate bool) string {
	t.Helper()
	cfgPath := filepath.Join(dir, "litefs.yml")
	cfg := fmt.Sprintf(
		"fuse:\n  dir: %q\ndata:\n  dir: %q\nhttp:\n  addr: %q\nlease:\n  type: \"static\"\n  hostname: \"primary\"\n  advertise-url: %q\n  candidate: %v\n",
		fuseDir, dataDir, httpAddr, advertiseURL, candidate)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// waitForLogContains polls a log file until it contains substr or the test
// times out.
func waitForLogContains(t *testing.T, path, substr string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			if contains(string(raw), substr) == nil {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("%s never contained %q", path, substr)
}

// TestSecondaryReplicatesViaLiteFS is the end-to-end proof for
// litefs-replication-plan.md's Step 3: a real master running its OWN
// events.db through a LiteFS FUSE mount (not a plain path -- LiteFS has to
// see every write to replicate it, unlike Litestream's non-intrusive
// sidecar), a real secondary reading through its own FUSE mount, and command
// forwarding combined with that replication path -- including the
// replicate-back leg Litestream's equivalent test could not reliably assert
// (litestream_replica_test.go's TestSecondaryReplicatesViaLitestream
// deliberately skips it; see that file's doc comment).
//
// Runs `litefs mount` as two separate processes (not via LiteFS's own
// `exec:` subprocess-supervision option) so each side's log and lifecycle
// stays independently inspectable through this suite's existing serve()/
// startLitefsProcess() patterns -- the `exec:`-supervised single-process
// shape is a real, and arguably better, deployment option (see the plan's
// Step 1) but is an operator-doc/systemd-unit concern, not something this
// test needs to exercise to prove the replication mechanism itself works.
func TestSecondaryReplicatesViaLiteFS(t *testing.T) {
	bin := litefsBin(t)
	pocketcqrsBin := build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(t.TempDir(), "pocketcqrs"))

	workDir := t.TempDir()

	// ---- primary: LiteFS mount first, pocketcqrs's OWN events.db lives
	// inside it from the start -- this is the real structural difference
	// from Litestream, confirmed in litefs-replication-plan.md.
	primaryFuseDir := filepath.Join(workDir, "primary-mnt")
	primaryDataDir := filepath.Join(workDir, "primary-litefs-data")
	if err := os.MkdirAll(primaryFuseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	primaryAddr := "http://127.0.0.1:23808"
	primaryCfg := litefsConfig(t, workDir, primaryFuseDir, primaryDataDir, ":23808", primaryAddr, true)
	startLitefsProcess(t, bin, workDir, "litefs-primary", primaryCfg, primaryFuseDir)
	waitForLogContains(t, filepath.Join(workDir, "litefs-primary.log"), "primary lease acquired")

	// now start pocketcqrs itself, pointed at the FUSE-mounted path
	masterEventsPath := filepath.Join(primaryFuseDir, "events.db")
	masterDataDir := filepath.Join(workDir, "master-pb_data")
	masterFnDir := filepath.Join(workDir, "master-pb_functions")
	if err := os.MkdirAll(masterFnDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := exec.Command(pocketcqrsBin, "superuser", "upsert", superuserEmail, superuserPassword, "--dir", masterDataDir)
	if out, err := seed.CombinedOutput(); err != nil {
		t.Fatalf("seeding master's superuser failed: %v\n%s", err, out)
	}
	masterAddr := freeAddr(t)
	master := &harness{t: t, BackendURL: "http://" + masterAddr, FunctionsDir: masterFnDir, DataDir: masterDataDir, Bin: pocketcqrsBin, client: newClient(t)}
	master.stop = serve(t, pocketcqrsBin, workDir, "master",
		"serve", "--http", masterAddr, "--dir", masterDataDir, "--functionsDir", masterFnDir, "--tutorial",
		"--cqrsEventsPath", masterEventsPath)
	waitFor(t, master.BackendURL+"/api/health")
	master.Token = master.authenticate()

	master.command("task", "t1", "CreateTask", map[string]string{"title": "via litefs"})

	// ---- secondary: its own FUSE mount, static lease pointed at the primary
	secondaryFuseDir := filepath.Join(workDir, "secondary-mnt")
	secondaryDataDir := filepath.Join(workDir, "secondary-litefs-data")
	if err := os.MkdirAll(secondaryFuseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secondaryCfg := litefsConfig(t, workDir, secondaryFuseDir, secondaryDataDir, ":23809", primaryAddr, false)
	startLitefsProcess(t, bin, workDir, "litefs-secondary", secondaryCfg, secondaryFuseDir)

	secondaryEventsPath := filepath.Join(secondaryFuseDir, "events.db")
	waitForFile(t, secondaryEventsPath)

	// full secondary node: reads via the FUSE-replicated copy, forwards
	// commands to master (item 3) -- Step 4's combination, this time fully
	// asserted both directions (see the doc comment above).
	secondary := startSecondaryAt(t, master, secondaryEventsPath, "--cqrsMasterAddr", master.BackendURL)

	eventually(t, "the secondary to see t1 via LiteFS replication", func() bool {
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

	eventually(t, "the secondary's local tasks collection to have t1", func() bool {
		var records struct {
			Items []struct {
				TaskID string `json:"taskId"`
				Title  string `json:"title"`
			} `json:"items"`
		}
		status, _ := secondary.api(http.MethodGet, "/api/collections/tasks/records?filter=taskId='t1'", nil, &records)
		return status == http.StatusOK && len(records.Items) == 1 && records.Items[0].Title == "via litefs"
	})

	// forwarding, AND the replicate-back leg -- this is the assertion
	// Litestream's equivalent test could not make reliably (the stall).
	resp := secondary.do(http.MethodPost, secondary.BackendURL+"/api/cqrs/task/t2/CreateTask",
		jsonBody(map[string]string{"title": "forwarded via litefs secondary"}),
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
		t.Fatal("expected t2 to exist on the master after a command forwarded through the LiteFS-backed secondary")
	}

	eventually(t, "the forwarded command to replicate back to the secondary via LiteFS", func() bool {
		var feed struct {
			Events []struct{ AggregateID string } `json:"events"`
		}
		status, _ := secondary.api(http.MethodGet, "/api/cqrs/events?aggregate=task", nil, &feed)
		if status != http.StatusOK {
			return false
		}
		for _, e := range feed.Events {
			if e.AggregateID == "t2" {
				return true
			}
		}
		return false
	})
}
