//go:build smoke

// Package smoke drives a real pocketcqrs backend and a real extcaller
// Consumer together over HTTP and a real events.db, exactly as they would
// run in production. It exists for PLAN.md Phase 2's explicit test: a burst
// of events while the target third party is down, recovering within the
// retry budget with no lost or duplicated dispatch — behavior that lives in
// the wiring between three real components (the gateway, the checkpointed
// engine, and the idempotency store) and cannot be exercised by a unit test
// against any one of them alone.
//
// Run it explicitly (it builds a binary and opens ports, so it is not part
// of the default suite):
//
//	go test -tags=smoke ./smoke/ -v -timeout 5m
package smoke

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs-extensions/internal/gatewayclient"
	"github.com/jamestryand/pocketcqrs-extensions/internal/localstore"
)

const (
	superuserEmail    = "smoketest@example.com"
	superuserPassword = "smoke-pass-1234"
)

// harness is a running pocketcqrs backend with everything needed to drive
// it and to point an extcaller Consumer at it.
type harness struct {
	t *testing.T

	BackendURL string
	EventsDB   string
	Token      string

	client *http.Client
}

// startBackend builds pocketcqrs, seeds a superuser and serves with
// --tutorial, so the task aggregate (CmdCreateTask/TaskCreated, projected
// into the "tasks" collection) is available as a real target for follow-up
// commands.
func startBackend(t *testing.T) *harness {
	t.Helper()

	dir := t.TempDir()
	bin := buildBin(t, "github.com/jamestryand/pocketcqrs", filepath.Join(dir, "pocketcqrs"))
	dataDir := filepath.Join(dir, "pb_data")

	seed := exec.Command(bin, "superuser", "upsert", superuserEmail, superuserPassword, "--dir", dataDir)
	if out, err := seed.CombinedOutput(); err != nil {
		t.Fatalf("seeding the superuser failed: %v\n%s", err, out)
	}

	addr := freeAddr(t)
	h := &harness{
		t: t, BackendURL: "http://" + addr,
		EventsDB: filepath.Join(dataDir, "events.db"),
		client:   &http.Client{Timeout: 20 * time.Second},
	}
	serveProcess(t, bin, dir, "backend",
		"serve", "--http", addr, "--dir", dataDir, "--tutorial")
	waitFor(t, h.BackendURL+"/api/health")

	h.Token = h.authenticate()
	return h
}

func buildBin(t *testing.T, pkg, out string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, pkg)
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s failed: %v\n%s", pkg, err, outBytes)
	}
	return out
}

func serveProcess(t *testing.T, bin, dir, label string, args ...string) {
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
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			logFile.Close()
		})
	}
	t.Cleanup(func() {
		stop()
		if t.Failed() {
			if raw, err := os.ReadFile(logPath); err == nil && len(raw) > 0 {
				t.Logf("---- %s log ----\n%s", label, raw)
			}
		}
	})
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func waitFor(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("%s never became ready", url)
}

func (h *harness) authenticate() string {
	h.t.Helper()
	raw, _ := json.Marshal(map[string]string{"identity": superuserEmail, "password": superuserPassword})
	resp, err := h.client.Post(h.BackendURL+"/api/collections/_superusers/auth-with-password",
		"application/json", bytes.NewReader(raw))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("superuser auth failed: %s: %s", resp.Status, body)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		h.t.Fatal(err)
	}
	return out.Token
}

// command issues a domain command through the gateway (as an ordinary
// authenticated client would — not through gatewayclient, since this
// simulates the domain-side traffic that produces the events extcaller
// reacts to, separate from the follow-up dispatch under test).
func (h *harness) command(aggregate, id, name string, payload any) {
	h.t.Helper()
	raw, _ := json.Marshal(payload)
	url := fmt.Sprintf("%s/api/cqrs/%s/%s/%s", h.BackendURL, aggregate, id, name)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", h.Token)
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		h.t.Fatalf("%s: expected 200, got %d: %s", url, resp.StatusCode, body)
	}
}

// taskExists reports whether a "tasks" record with this taskId exists (see
// pocketcqrs's projections.tasksProjection).
func (h *harness) taskExists(taskID string) bool {
	h.t.Helper()
	url := fmt.Sprintf("%s/api/collections/tasks/records?filter=%s",
		h.BackendURL, "taskId%3D%22"+taskID+"%22")
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", h.Token)
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		TotalItems int `json:"totalItems"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		h.t.Fatal(err)
	}
	return out.TotalItems == 1
}

// eventually retries until cond holds — the consumer engine is
// asynchronous, so a bare assertion after a command is a race.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// openStores opens an extcaller localstore.Stores pair against h's real
// events.db and a fresh local store under t.TempDir().
func (h *harness) openStores(t *testing.T) *localstore.Stores {
	t.Helper()
	stores, err := localstore.Open(h.EventsDB, filepath.Join(t.TempDir(), "extcaller.db"))
	if err != nil {
		t.Fatalf("localstore.Open: %v", err)
	}
	t.Cleanup(func() { stores.Close() })
	return stores
}

func (h *harness) gatewayClient() *gatewayclient.Client {
	return gatewayclient.New(h.BackendURL, h.Token, 10*time.Second)
}
