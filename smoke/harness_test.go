//go:build smoke

// Package smoke runs the platform end to end: it builds both binaries,
// starts a real backend and a real dashboard on ephemeral ports, and drives
// them over HTTP exactly as an operator would.
//
// Why this exists as code rather than as a script someone runs by hand: the
// two worst defects found while building the dashboard were caught by ad-hoc
// smoke checks — an admin action that silently did nothing, and a function
// file that could be written and would then abort every later reload. Unit
// tests missed both, because both live in the wiring between processes.
// Checks that only exist in a shell history cannot be re-run by anyone else,
// and are not run by CI.
//
// Run it explicitly (it builds binaries and opens ports, so it is not part
// of the default suite):
//
//	go test -tags=smoke ./smoke/ -v
package smoke

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	superuserEmail    = "smoketest@example.com"
	superuserPassword = "smoke-pass-1234"
)

// harness is a running backend (+ optionally a dashboard) with everything
// needed to talk to them.
type harness struct {
	t *testing.T

	BackendURL   string
	DashboardURL string
	FunctionsDir string
	Token        string

	client *http.Client
}

// startBackend builds pocketcqrs, seeds a superuser and serves. Function
// files given as name→source are written before boot, so boot-time
// registration is part of what is exercised.
func startBackend(t *testing.T, functions map[string]string) *harness {
	t.Helper()

	dir := t.TempDir()
	fnDir := filepath.Join(dir, "pb_functions")
	if err := os.MkdirAll(fnDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, src := range functions {
		if err := os.WriteFile(filepath.Join(fnDir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	bin := build(t, "github.com/JamesTryand/pocketcqrs", filepath.Join(dir, "pocketcqrs"))
	dataDir := filepath.Join(dir, "pb_data")

	// the superuser has to exist before serving: every operational route is
	// superuser-scoped, so without one the harness could not authenticate
	seed := exec.Command(bin, "superuser", "upsert", superuserEmail, superuserPassword, "--dir", dataDir)
	if out, err := seed.CombinedOutput(); err != nil {
		t.Fatalf("seeding the superuser failed: %v\n%s", err, out)
	}

	addr := freeAddr(t)
	h := &harness{t: t, BackendURL: "http://" + addr, FunctionsDir: fnDir, client: newClient(t)}
	serve(t, bin, dir, "backend",
		"serve", "--http", addr, "--dir", dataDir, "--functionsDir", fnDir)
	waitFor(t, h.BackendURL+"/api/health")

	h.Token = h.authenticate()
	return h
}

// startDashboard builds and serves the dashboard against this backend.
func (h *harness) startDashboard() {
	h.t.Helper()
	dir := h.t.TempDir()
	bin := build(h.t, "github.com/JamesTryand/pocketcqrs/pocketcqrs-dashboard",
		filepath.Join(dir, "pocketcqrs-dashboard"))

	addr := freeAddr(h.t)
	h.DashboardURL = "http://" + addr
	serve(h.t, bin, dir, "dashboard", "--backend", h.BackendURL, "--listen", addr)
	waitFor(h.t, h.DashboardURL+"/login")
}

func build(t *testing.T, pkg, out string) string {
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

// serve starts a long-running process and registers its teardown. Output
// goes to a file the test names on failure — a smoke failure is usually
// explained by what the server logged, and losing that costs far more time
// than keeping it.
func serve(t *testing.T, bin, dir, label string, args ...string) {
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

// freeAddr reserves a loopback port by binding and releasing it.
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

// newClient keeps cookies (the dashboard session) and does NOT follow
// redirects, so a 303 is assertable rather than invisible.
func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Jar:           jar,
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func (h *harness) authenticate() string {
	h.t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	resp := h.do(http.MethodPost, h.BackendURL+"/api/collections/_superusers/auth-with-password",
		jsonBody(map[string]string{"identity": superuserEmail, "password": superuserPassword}), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("superuser auth failed: %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		h.t.Fatal(err)
	}
	return out.Token
}

// ---- request helpers ----

type body struct {
	reader      io.Reader
	contentType string
}

func jsonBody(v any) *body {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return &body{reader: bytes.NewReader(raw), contentType: "application/json"}
}

func formBody(values map[string]string) *body {
	form := make([]string, 0, len(values))
	for k, v := range values {
		form = append(form, k+"="+urlEncode(v))
	}
	return &body{reader: strings.NewReader(strings.Join(form, "&")),
		contentType: "application/x-www-form-urlencoded"}
}

func urlEncode(s string) string {
	var b strings.Builder
	for _, r := range []byte(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == '~':
			b.WriteByte(r)
		default:
			fmt.Fprintf(&b, "%%%02X", r)
		}
	}
	return b.String()
}

func (h *harness) do(method, url string, b *body, headers map[string]string) *http.Response {
	h.t.Helper()
	var reader io.Reader
	if b != nil {
		reader = b.reader
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	if b != nil {
		req.Header.Set("Content-Type", b.contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// api calls the backend with the superuser token and decodes JSON into out
// (which may be nil). It returns the status so tests can assert refusals.
func (h *harness) api(method, path string, b *body, out any) (int, string) {
	h.t.Helper()
	resp := h.do(method, h.BackendURL+path, b, map[string]string{"Authorization": h.Token})
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			h.t.Fatalf("%s %s: decoding %q: %v", method, path, truncate(string(raw), 200), err)
		}
	}
	return resp.StatusCode, string(raw)
}

// apiOK is api() for calls that must succeed.
func (h *harness) apiOK(method, path string, b *body, out any) {
	h.t.Helper()
	status, raw := h.api(method, path, b, out)
	if status != http.StatusOK {
		h.t.Fatalf("%s %s: expected 200, got %d: %s", method, path, status, truncate(raw, 300))
	}
}

// dash GETs a dashboard page and returns its status and body.
func (h *harness) dash(method, path string, b *body, htmx bool) (int, string) {
	h.t.Helper()
	headers := map[string]string{}
	if htmx {
		headers["HX-Request"] = "true"
	}
	resp := h.do(method, h.DashboardURL+path, b, headers)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// dashHeaders is dash() when a response HEADER is the thing under test
// (HX-Redirect, Location, Vary).
func (h *harness) dashHeaders(method, path string, b *body, htmx bool) (*http.Response, string) {
	h.t.Helper()
	headers := map[string]string{}
	if htmx {
		headers["HX-Request"] = "true"
	}
	resp := h.do(method, h.DashboardURL+path, b, headers)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

func (h *harness) login() {
	h.t.Helper()
	status, _ := h.dash(http.MethodPost, "/login",
		formBody(map[string]string{"identity": superuserEmail, "password": superuserPassword}), false)
	if status != http.StatusSeeOther {
		h.t.Fatalf("dashboard login failed: %d", status)
	}
}

// command issues a domain command through the gateway.
func (h *harness) command(aggregate, id, name string, payload any) {
	h.t.Helper()
	h.apiOK(http.MethodPost, fmt.Sprintf("/api/cqrs/%s/%s/%s", aggregate, id, name), jsonBody(payload), nil)
}

// setMode moves the maintenance barrier.
func (h *harness) setMode(mode string) {
	h.t.Helper()
	h.apiOK(http.MethodPost, "/api/cqrs/admin/mode", jsonBody(map[string]string{"mode": mode}), nil)
}

// reload hot-reloads the functions directory and returns the report.
func (h *harness) reload() map[string]any {
	h.t.Helper()
	var report map[string]any
	h.apiOK(http.MethodPost, "/api/cqrs/admin/reload", nil, &report)
	return report
}

// eventually retries until cond holds — consumers are asynchronous, so a
// bare assertion after a command is a race, and a fixed sleep is either slow
// or flaky.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func contains(haystack string, needles ...string) error {
	var missing []string
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return errors.New("missing " + strings.Join(missing, ", "))
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
