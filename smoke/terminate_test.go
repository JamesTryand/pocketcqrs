//go:build smoke

package smoke

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestInterruptStopsBackgroundLoopsCleanly: on SIGINT the termination hook
// stops the consumer engine, batch writer and pruners, waits for them
// (bounded) and closes the stores, and the process exits promptly without
// reporting stragglers. Windows has no way to deliver os.Interrupt to a
// child process, so this only runs elsewhere; the stop/wait mechanics are
// unit-tested in lifecycle_test.go, consumers and batching.
func TestInterruptStopsBackgroundLoopsCleanly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Interrupt cannot be sent to a child process on Windows")
	}
	dir := t.TempDir()
	bin := build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(dir, "pocketcqrs"))
	dataDir := filepath.Join(dir, "pb_data")
	seed := exec.Command(bin, "superuser", "upsert", superuserEmail, superuserPassword, "--dir", dataDir)
	if out, err := seed.CombinedOutput(); err != nil {
		t.Fatalf("seeding the superuser failed: %v\n%s", err, out)
	}

	addr := freeAddr(t)
	logPath := filepath.Join(dir, "backend.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	cmd := exec.Command(bin, "serve", "--http", addr, "--dir", dataDir,
		"--functionsDir", filepath.Join(dir, "pb_functions"), "--tutorial")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	waitFor(t, "http://"+addr+"/api/health")

	h := &harness{t: t, BackendURL: "http://" + addr, client: newClient(t)}
	h.Token = h.authenticate()
	h.command("task", "t1", "CreateTask", map[string]string{"title": "before shutdown"})

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			raw, _ := os.ReadFile(logPath)
			t.Fatalf("expected a clean exit on interrupt, got %v\n%s", err, raw)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the process did not exit within 15s of SIGINT")
	}
	raw, _ := os.ReadFile(logPath)
	if strings.Contains(string(raw), "still running") || strings.Contains(string(raw), "shutdown: closing") {
		t.Fatalf("shutdown reported a problem:\n%s", raw)
	}
}
