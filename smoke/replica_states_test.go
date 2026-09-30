//go:build smoke

package smoke

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The health/telemetry contract's replication freshness (STATE-MACHINES.md,
// machine 4) through the two real replica transports, not the shared-file
// shortcut TestSecondaryReportsReplicationFromTheMastersHeartbeat uses. Each
// state is reached the way it happens in production:
//
//   - Fresh: replication flowing, the master's heartbeat within the threshold.
//   - StaleWriterUp: replication stops while the master keeps running (the
//     reader's own link is the problem): not_ready, replication_stale.
//   - StaleWriterDown: the master stops (a shared cause): degraded,
//     replication_stale, the reader stays in the pool.
//   - Unknown: the reader's copy can no longer be read at all:
//     not_ready, replication_unknown.
//
// Linux-only in practice: LiteFS is FUSE-based and the Litestream follower
// is exercised on the same hosts (see litefs_replica_test.go).

// replicaStaleThreshold is short so a stall shows within the test's budget;
// the reader measures every replicaHeartbeatInterval.
const (
	replicaStaleThreshold    = "2s"
	replicaHeartbeatInterval = "250ms"
)

// readiness is one /readyz answer from a node's ops port.
type readiness struct {
	code    int
	status  string
	role    string
	reasons []string
	deps    map[string]any
}

func (r readiness) String() string {
	return r.status + " (" + strings.Join(r.reasons, ",") + ")"
}

func (r readiness) has(reason string) bool {
	for _, got := range r.reasons {
		if got == reason {
			return true
		}
	}
	return false
}

func readyzAt(opsPort string) readiness {
	resp, err := http.Get("http://127.0.0.1:" + opsPort + "/readyz")
	if err != nil {
		return readiness{}
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	r := readiness{code: resp.StatusCode}
	r.status, _ = body["status"].(string)
	r.role, _ = body["role"].(string)
	list, _ := body["reasons"].([]any)
	for _, v := range list {
		s, _ := v.(string)
		r.reasons = append(r.reasons, s)
	}
	if checks, _ := body["checks"].(map[string]any); checks != nil {
		r.deps, _ = checks["dependencies"].(map[string]any)
	}
	return r
}

// eventuallyReady waits until the reader's /readyz satisfies cond, and on a
// timeout names the last answer it saw.
func eventuallyReady(t *testing.T, opsPort, what string, cond func(readiness) bool) readiness {
	t.Helper()
	var last readiness
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		last = readyzAt(opsPort)
		if cond(last) {
			return last
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; last /readyz: %d %s", what, last.code, last)
	return last
}

func isFresh(r readiness) bool {
	return r.code == http.StatusOK && r.status == "ready" && r.role == "reader" &&
		!r.has("replication_stale") && !r.has("replication_unknown")
}

func isStaleWriterUp(r readiness) bool {
	return r.code == http.StatusServiceUnavailable && r.status == "not_ready" && r.has("replication_stale")
}

func isStaleWriterDown(r readiness) bool {
	return r.code == http.StatusOK && r.status == "degraded" && r.has("replication_stale")
}

func isReplicationUnknown(r readiness) bool {
	return r.code == http.StatusServiceUnavailable && r.status == "not_ready" && r.has("replication_unknown")
}

// opsPortArgs gives a secondary its own ops port and the short timings.
func opsPortArgs(t *testing.T) (string, []string) {
	t.Helper()
	_, port, err := net.SplitHostPort(freeAddr(t))
	if err != nil {
		t.Fatal(err)
	}
	return port, []string{
		"--cqrsOpsPort", port,
		"--cqrsStaleThreshold", replicaStaleThreshold,
		"--cqrsHeartbeatInterval", replicaHeartbeatInterval,
	}
}

// relay is a TCP relay the test can cut and restore, standing in for the
// network between two LiteFS nodes. Cutting it closes every open connection
// and stops accepting; restoring listens on the same address again.
type relay struct {
	t      *testing.T
	addr   string
	target string

	mu    sync.Mutex
	ln    net.Listener
	conns map[net.Conn]struct{}
}

func startRelay(t *testing.T, target string) *relay {
	t.Helper()
	r := &relay{t: t, addr: freeAddr(t), target: target, conns: map[net.Conn]struct{}{}}
	r.restore()
	t.Cleanup(r.cut)
	return r
}

func (r *relay) restore() {
	r.t.Helper()
	var ln net.Listener
	var err error
	// the port was just released by cut() or freeAddr; retry briefly in case
	// the kernel has not let it go yet
	for i := 0; i < 50; i++ {
		if ln, err = net.Listen("tcp", r.addr); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		r.t.Fatalf("relay: listening on %s: %v", r.addr, err)
	}
	r.mu.Lock()
	r.ln = ln
	r.mu.Unlock()
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", r.target)
			if err != nil {
				in.Close()
				continue
			}
			r.mu.Lock()
			r.conns[in], r.conns[out] = struct{}{}, struct{}{}
			r.mu.Unlock()
			go func() { _, _ = io.Copy(out, in); out.Close() }()
			go func() { _, _ = io.Copy(in, out); in.Close() }()
		}
	}()
}

func (r *relay) cut() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ln != nil {
		r.ln.Close()
		r.ln = nil
	}
	for c := range r.conns {
		c.Close()
	}
	r.conns = map[net.Conn]struct{}{}
}

// startStoppable runs a long-lived helper process (litefs, litestream) with
// its output in dir/label.log, and returns a stop that kills it. Cleanup
// stops it too, and prints the log if the test failed.
func startStoppable(t *testing.T, dir, label string, onStop func(), name string, args ...string) (stop func()) {
	t.Helper()
	logPath := filepath.Join(dir, label+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", label, err)
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			if onStop != nil {
				onStop()
			}
		})
	}
	t.Cleanup(func() {
		stop()
		// again: a stop mid-test can fail to unmount while a node still has
		// the mount open, and by now (cleanups run in reverse) that node has
		// exited
		if onStop != nil {
			onStop()
		}
		logFile.Close()
		if t.Failed() {
			if raw, err := os.ReadFile(logPath); err == nil && len(raw) > 0 {
				t.Logf("---- %s log ----\n%s", label, raw)
			}
		}
	})
	return stop
}

// TestReplicationStatesViaLiteFS drives every replication state through a
// real LiteFS pair. The secondary's LiteFS reaches the primary's through a
// relay, so the link can be cut while the master keeps running.
func TestReplicationStatesViaLiteFS(t *testing.T) {
	bin := litefsBin(t)
	pocketcqrsBin := build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(t.TempDir(), "pocketcqrs"))
	workDir := t.TempDir()

	// ---- primary LiteFS and the master on it
	primaryFuseDir := filepath.Join(workDir, "primary-mnt")
	if err := os.MkdirAll(primaryFuseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	primaryHTTP := freeAddr(t)
	_, primaryPort, _ := net.SplitHostPort(primaryHTTP)
	primaryCfg := litefsConfig(t, workDir, primaryFuseDir, filepath.Join(workDir, "primary-litefs-data"),
		":"+primaryPort, "http://"+primaryHTTP, true)
	startStoppable(t, workDir, "litefs-primary", func() { _ = exec.Command("fusermount3", "-u", primaryFuseDir).Run() },
		bin, "mount", "-config", primaryCfg)
	waitForLogContains(t, filepath.Join(workDir, "litefs-primary.log"), "primary lease acquired")

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
		"--cqrsEventsPath", filepath.Join(primaryFuseDir, "events.db"))
	waitFor(t, master.BackendURL+"/api/health")

	// ---- secondary LiteFS, reaching the primary through the relay
	link := startRelay(t, primaryHTTP)
	secondaryDir := filepath.Join(workDir, "secondary")
	secondaryFuseDir := filepath.Join(secondaryDir, "mnt")
	if err := os.MkdirAll(secondaryFuseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secondaryCfg := litefsConfig(t, secondaryDir, secondaryFuseDir, filepath.Join(secondaryDir, "litefs-data"),
		freeAddr(t), "http://"+link.addr, false)
	stopSecondaryLiteFS := startStoppable(t, secondaryDir, "litefs-secondary",
		func() { _ = exec.Command("fusermount3", "-u", secondaryFuseDir).Run() },
		bin, "mount", "-config", secondaryCfg)
	secondaryEvents := filepath.Join(secondaryFuseDir, "events.db")
	waitForFile(t, secondaryEvents)

	opsPort, args := opsPortArgs(t)
	startSecondaryAt(t, master, secondaryEvents, args...)

	// ---- Fresh
	r := eventuallyReady(t, opsPort, "Fresh: a ready reader on a replicated heartbeat", isFresh)
	if r.deps["writer"] != "up" {
		t.Errorf("Fresh: writer dependency %v, want up", r.deps["writer"])
	}

	// ---- StaleWriterUp: the reader's own link is cut, the master runs on
	link.cut()
	r = eventuallyReady(t, opsPort, "StaleWriterUp: not_ready, replication_stale, with the master up", isStaleWriterUp)
	if r.deps["writer"] != "up" {
		t.Errorf("StaleWriterUp: writer dependency %v, want up (the master is still running)", r.deps["writer"])
	}

	// ---- back to Fresh once the link returns
	link.restore()
	eventuallyReady(t, opsPort, "Fresh again after the link is restored", isFresh)

	// ---- StaleWriterDown: the master stops, a shared cause
	master.stop()
	eventuallyReady(t, opsPort, "StaleWriterDown: degraded, replication_stale, with the master down", isStaleWriterDown)

	// ---- Unknown: the reader's copy can no longer be read at all
	stopSecondaryLiteFS()
	eventuallyReady(t, opsPort, "Unknown: not_ready, replication_unknown, once the replica cannot be read", isReplicationUnknown)
}

// TestReplicationStatesViaLitestream drives the stale states through a real
// `litestream replicate` / `restore -f` pair. Stopping the follower stops
// the reader's copy advancing while the master runs on.
//
// Unknown is not driven here: a follower's copy stays a readable file after
// the follower stops, so this transport has no natural way to make the
// heartbeat unreadable. TestReplicationStatesViaLiteFS drives it.
func TestReplicationStatesViaLitestream(t *testing.T) {
	bin := litestreamBin(t)

	master := startBackend(t, nil)
	workDir := t.TempDir()
	masterEvents := filepath.Join(master.DataDir, "events.db")
	secondaryEvents := filepath.Join(workDir, "secondary-events.db")
	cfgPath := litestreamConfig(t, workDir, masterEvents, filepath.Join(workDir, "replica"))

	startStoppable(t, workDir, "litestream-replicate", nil, bin, "replicate", "-config", cfgPath)
	time.Sleep(2 * time.Second) // let replicate ship its first snapshot (see TestSecondaryReplicatesViaLitestream)
	stopFollower := startStoppable(t, workDir, "litestream-restore", nil, bin,
		"restore", "-f", "-follow-interval", "250ms", "-config", cfgPath, "-o", secondaryEvents, masterEvents)
	waitForFile(t, secondaryEvents)

	opsPort, args := opsPortArgs(t)
	startSecondaryAt(t, master, secondaryEvents, args...)

	// ---- Fresh
	r := eventuallyReady(t, opsPort, "Fresh: a ready reader on a followed heartbeat", isFresh)
	if r.deps["writer"] != "up" {
		t.Errorf("Fresh: writer dependency %v, want up", r.deps["writer"])
	}

	// ---- StaleWriterUp: the follower stops, the master runs on
	stopFollower()
	r = eventuallyReady(t, opsPort, "StaleWriterUp: not_ready, replication_stale, with the master up", isStaleWriterUp)
	if r.deps["writer"] != "up" {
		t.Errorf("StaleWriterUp: writer dependency %v, want up (the master is still running)", r.deps["writer"])
	}

	// ---- StaleWriterDown: the master stops too
	master.stop()
	eventuallyReady(t, opsPort, "StaleWriterDown: degraded, replication_stale, with the master down", isStaleWriterDown)
}
