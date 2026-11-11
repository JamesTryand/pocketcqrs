//go:build smoke

package smoke

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A program of one's own, in its own module outside this repository: `init go`
// starts it from a document, the generated wiring registers everything, and it
// depends on pocketcqrs only as a module (here through a replace onto this
// checkout, the test's stand-in for a released version). It serves the
// document's commands and read routes and declares its read models'
// collections with their types.
func TestGoConsumerOutsideTheRepository(t *testing.T) {
	repo, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	cli := builtOnce(t, "github.com/jamestryand/pocketcqrs")
	app := filepath.Join(t.TempDir(), "tickets")
	run := func(dir string, env []string, name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
		}
	}
	run(repo, nil, cli, "init", "go", "example.com/tickets", "--dir", app, "--pocketcqrs", repo,
		"--model", filepath.Join(repo, "smoke", "testdata", "tickets.eventmodel.json"))

	// nothing of pocketcqrs's source is in the new module: only its own files
	// and the generated code
	if _, err := os.Stat(filepath.Join(app, "domain", "register.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(app, "host")); err == nil {
		t.Fatal("init copied pocketcqrs's host package into the program")
	}

	offline := []string{"GOFLAGS=-mod=mod", "GOPROXY=off"}
	run(app, offline, "go", "mod", "tidy")
	exe := "tickets"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	built := filepath.Join(app, exe)
	run(app, offline, "go", "build", "-o", built, ".")
	// regenerating through the tool dependency (go generate) leaves it building
	run(app, offline, "go", "generate", "./...")
	run(app, offline, "go", "build", "-o", built, ".")

	h := startBackendBin(t, func(dir string) string {
		out := filepath.Join(dir, exe)
		if err := copyExecutable(built, out); err != nil {
			t.Fatal(err)
		}
		return out
	}, nil)

	// the read models' collections, declared by the generated wiring, typed
	var col struct {
		Fields   []map[string]any `json:"fields"`
		ListRule *string          `json:"listRule"`
	}
	h.apiOK(http.MethodGet, "/api/collections/tickets", nil, &col)
	types := map[string]string{}
	for _, f := range col.Fields {
		types[f["name"].(string)], _ = f["type"].(string)
	}
	if types["ticketId"] != "text" || types["estimatedEffort"] != "number" {
		t.Fatalf("tickets collection fields: %v", types)
	}
	if col.ListRule != nil {
		t.Fatalf("a compiled-in read model's record API must be superusers only, got rule %q", *col.ListRule)
	}

	// the document's command permissions: OpenTicket needs the agent role
	var users struct {
		Fields []map[string]any `json:"fields"`
	}
	h.apiOK(http.MethodGet, "/api/collections/users", nil, &users)
	h.apiOK(http.MethodPatch, "/api/collections/users",
		jsonBody(map[string]any{"fields": append(users.Fields, map[string]any{"name": "role", "type": "text"})}), nil)
	h.apiOK(http.MethodPost, "/api/collections/users/records", jsonBody(map[string]any{
		"email": "nobody@example.com", "password": "consumer-pass-1234", "passwordConfirm": "consumer-pass-1234"}), nil)
	var signedIn struct {
		Token string `json:"token"`
	}
	h.apiOK(http.MethodPost, "/api/collections/users/auth-with-password",
		jsonBody(map[string]string{"identity": "nobody@example.com", "password": "consumer-pass-1234"}), &signedIn)
	open := jsonBody(map[string]any{"teamId": "red", "estimatedEffort": 3})
	resp := h.do(http.MethodPost, h.BackendURL+"/api/cqrs/ticket/t-0/OpenTicket", open,
		map[string]string{"Authorization": signedIn.Token})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("OpenTicket without the agent role: want 403, got %d", resp.StatusCode)
	}

	h.apiOK(http.MethodPost, "/api/cqrs/ticket/t-1/OpenTicket", jsonBody(map[string]any{"teamId": "red", "estimatedEffort": 3}), nil)
	eventually(t, "the ticket in its read route", func() bool {
		resp := h.do(http.MethodGet, h.BackendURL+"/api/cqrs/rm/tickets", nil, map[string]string{"Authorization": h.Token})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		var out struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return false
		}
		return len(out.Items) == 1 && out.Items[0]["teamId"] == "red"
	})
}
