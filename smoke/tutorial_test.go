//go:build smoke

package smoke

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEmptyDefaultBoot is the proof that the platform ships empty: the
// example domains are this repo's teaching material, not part of what
// pocketcqrs is, and installing the binary must not create collections or
// aggregates nobody asked for.
func TestEmptyDefaultBoot(t *testing.T) {
	h := startBackendFlags(t, nil) // no --tutorial

	status, _ := h.api(http.MethodPost, "/api/cqrs/task/t1/CreateTask",
		jsonBody(map[string]string{"title": "should not land"}), nil)
	if status != http.StatusNotFound {
		t.Fatalf("the example task aggregate must not be registered by default, got %d", status)
	}

	// the collection is not merely empty — its migration never ran
	if status, _ := h.api(http.MethodGet, "/api/collections/tasks/records", nil, nil); status != http.StatusNotFound {
		t.Fatalf("the tasks collection must not exist by default, got %d", status)
	}

	var catalog struct {
		Aggregates []struct {
			Name   string `json:"name"`
			Origin string `json:"origin"`
		} `json:"aggregates"`
	}
	h.apiOK(http.MethodGet, "/api/cqrs/catalog", nil, &catalog)
	for _, a := range catalog.Aggregates {
		if a.Origin == "go" {
			t.Fatalf("no Go aggregate should be registered by default, found %q", a.Name)
		}
	}

	// ...and guarding nothing must not mean guarding everything. This is the
	// boot-path twin of TestWriteGuardGuardsNothingWhenThereIsNothingToGuard:
	// with no projections at all, the guarded list is empty here too.
	h.apiOK(http.MethodPost, "/api/collections", jsonBody(map[string]any{
		"name":   "smoke_mine",
		"type":   "base",
		"fields": []map[string]any{{"name": "title", "type": "text"}},
	}), nil)
	status, raw := h.api(http.MethodPost, "/api/collections/smoke_mine/records",
		jsonBody(map[string]string{"title": "mine"}), nil)
	if status != http.StatusOK {
		t.Fatalf("a user's own collection must be writable on an empty platform, got %d: %s",
			status, truncate(raw, 300))
	}
}

// TestEmptyDefaultFreesBuiltinNames pins the other side of the change. A JS
// decider is refused when its aggregate collides with a registered one, so
// making the examples opt-in hands `task` and `order` back to the user.
func TestEmptyDefaultFreesBuiltinNames(t *testing.T) {
	h := startBackendFlags(t, map[string]string{
		"task.js": `//@trigger decider task
//@handles TaskOpened
//@commands OpenTask
//@produces OpenTask TaskOpened

function initialState() { return { exists: false }; }
function decide(command, state) {
  if (command.name === 'OpenTask') {
    if (state.exists) throw new Error('task already exists');
    return [{ type: 'TaskOpened', data: { title: command.payload.title } }];
  }
  throw new Error('unknown command: ' + command.name);
}
function evolve(state, event) {
  if (event.type === 'TaskOpened') { state.exists = true; }
  return state;
}
`,
	})

	var out struct {
		Events []struct {
			Type string `json:"type"`
		} `json:"events"`
	}
	h.apiOK(http.MethodPost, "/api/cqrs/task/t1/OpenTask",
		jsonBody(map[string]string{"title": "mine now"}), &out)
	if len(out.Events) != 1 || out.Events[0].Type != "TaskOpened" {
		t.Fatalf("a user's own `task` decider should own the name: %+v", out.Events)
	}

	var catalog struct {
		Aggregates []struct {
			Name   string `json:"name"`
			Origin string `json:"origin"`
		} `json:"aggregates"`
	}
	h.apiOK(http.MethodGet, "/api/cqrs/catalog", nil, &catalog)
	for _, a := range catalog.Aggregates {
		if a.Name == "task" && a.Origin != "js" {
			t.Fatalf("expected `task` to be JS-owned, got origin %q", a.Origin)
		}
	}
}

// TestOrphanedExampleCollectionsStayGuarded covers the upgrade path, which
// is the one with teeth: an instance that ran with --tutorial has real
// collections, and turning the flag off must not quietly hand them to the
// REST API. Losing a protection silently is worse than never having it.
//
// It boots twice over ONE data dir, so it does its own wiring rather than
// going through startBackendFlags.
func TestOrphanedExampleCollectionsStayGuarded(t *testing.T) {
	dir := t.TempDir()
	fnDir := filepath.Join(dir, "pb_functions")
	if err := os.MkdirAll(fnDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(dir, "pocketcqrs"))
	dataDir := filepath.Join(dir, "pb_data")

	seed := exec.Command(bin, "superuser", "upsert", superuserEmail, superuserPassword, "--dir", dataDir)
	if out, err := seed.CombinedOutput(); err != nil {
		t.Fatalf("seeding the superuser failed: %v\n%s", err, out)
	}

	boot := func(label string, extra ...string) (*harness, func()) {
		addr := freeAddr(t)
		h := &harness{t: t, BackendURL: "http://" + addr, FunctionsDir: fnDir, client: newClient(t)}
		args := append([]string{
			"serve", "--http", addr, "--dir", dataDir, "--functionsDir", fnDir,
		}, extra...)
		stop := serve(t, bin, dir, label, args...)
		waitFor(t, h.BackendURL+"/api/health")
		h.Token = h.authenticate()
		return h, stop
	}

	// with the examples on, produce a real row
	on, stopOn := boot("backend-tutorial", "--tutorial")
	on.command("task", "t1", "CreateTask", map[string]string{"title": "before"})
	var before struct {
		TotalItems int `json:"totalItems"`
	}
	eventually(t, "the example projection to write its row", func() bool {
		status, _ := on.api(http.MethodGet, "/api/collections/tasks/records", nil, &before)
		return status == http.StatusOK && before.TotalItems == 1
	})
	stopOn()

	// same data dir, examples off
	off, _ := boot("backend-empty")

	if status, _ := off.api(http.MethodPost, "/api/cqrs/task/t2/CreateTask",
		jsonBody(map[string]string{"title": "after"}), nil); status != http.StatusNotFound {
		t.Fatalf("the aggregate should be gone, got %d", status)
	}

	status, raw := off.api(http.MethodPost, "/api/collections/tasks/records",
		jsonBody(map[string]string{"taskId": "hack", "title": "direct"}), nil)
	if status != http.StatusForbidden {
		t.Fatalf("an orphaned example collection must STAY write-guarded, got %d: %s",
			status, truncate(raw, 300))
	}

	var after struct {
		TotalItems int `json:"totalItems"`
	}
	off.apiOK(http.MethodGet, "/api/collections/tasks/records", nil, &after)
	if after.TotalItems != 1 {
		t.Fatalf("the rows written under --tutorial should survive, got %d", after.TotalItems)
	}

	// and the operator is told, rather than left to discover a frozen
	// collection months later
	raw2, err := os.ReadFile(filepath.Join(dir, "backend-empty.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw2), "example collections exist but nothing is projecting into them") {
		t.Fatalf("boot must warn about orphaned example collections; log was:\n%s", truncate(string(raw2), 1500))
	}
}
