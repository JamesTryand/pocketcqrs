//go:build smoke

package smoke

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// startSecondary boots a second, independent pocketcqrs process against its
// own data dir, pointed at master's events.db via --cqrsEventsPath — the
// single-writer/multi-reader shape (item 2). It reuses master's already-built
// binary rather than building a second copy.
func startSecondary(t *testing.T, master *harness) *harness {
	t.Helper()

	dir := t.TempDir()
	fnDir := filepath.Join(dir, "pb_functions")
	if err := os.MkdirAll(fnDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(dir, "pb_data")

	// each node needs its OWN local superuser: data.db is never shared or
	// replicated, only events.db is (F-12) -- the secondary's _superusers
	// table is a completely independent, unrelated table to the master's.
	seed := exec.Command(master.Bin, "superuser", "upsert", superuserEmail, superuserPassword, "--dir", dataDir)
	if out, err := seed.CombinedOutput(); err != nil {
		t.Fatalf("seeding the secondary's superuser failed: %v\n%s", err, out)
	}

	addr := freeAddr(t)
	h := &harness{t: t, BackendURL: "http://" + addr, FunctionsDir: fnDir, DataDir: dataDir, Bin: master.Bin, client: newClient(t)}
	masterEventsPath := filepath.Join(master.DataDir, "events.db")
	serve(t, master.Bin, dir, "secondary",
		"serve", "--http", addr, "--dir", dataDir, "--functionsDir", fnDir, "--tutorial",
		"--cqrsRole", "secondary", "--cqrsEventsPath", masterEventsPath)
	waitFor(t, h.BackendURL+"/api/health")

	h.Token = h.authenticate()
	return h
}

// TestSecondaryPollsMasterEventsReadOnly is the end-to-end proof for item 2:
// two real, separate OS processes, sharing nothing but master's events.db
// file on disk (no Litestream VFS -- this is the plain-file degenerate case
// --cqrsVFS supports, which is exactly what a same-machine or NFS-mounted
// setup looks like without it). It is the one thing none of the unit tests
// could show: real cross-process concurrent SQLite access, not two Stores in
// the same Go runtime.
func TestSecondaryPollsMasterEventsReadOnly(t *testing.T) {
	master := startBackend(t, nil) // --tutorial
	master.command("task", "t1", "CreateTask", map[string]string{"title": "replicated"})

	secondary := startSecondary(t, master)

	// the secondary polls the shared events.db independently -- there is no
	// push from master, so this is genuinely exercising Poll+the 1s ticker
	// fallback (events/events.go's in-process nudge only fires for a
	// process's own Append) across two processes, not one.
	eventually(t, "the secondary's event feed to show t1's event", func() bool {
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

	// the secondary's OWN local consumers.Engine (NewEngineWithCheckpoints,
	// item 1/F-11) folded that event into its OWN local tasks collection --
	// proving the checkpoint split actually works across processes, not just
	// in the isolated unit test.
	eventually(t, "the secondary's local tasks collection to have t1", func() bool {
		var records struct {
			Items []struct {
				TaskID string `json:"taskId"`
				Title  string `json:"title"`
			} `json:"items"`
		}
		status, _ := secondary.api(http.MethodGet, "/api/collections/tasks/records?filter=taskId='t1'", nil, &records)
		return status == http.StatusOK && len(records.Items) == 1 && records.Items[0].Title == "replicated"
	})

	// commands are refused outright -- nothing forwards them to the master
	// yet (item 3, unbuilt)
	status, body := secondary.api(http.MethodPost, "/api/cqrs/task/t2/CreateTask",
		jsonBody(map[string]string{"title": "should not apply"}), nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 from the secondary, got %d: %s", status, body)
	}
	if err := contains(body, "read-only replica"); err != nil {
		t.Fatalf("secondary refusal message: %v (body: %s)", err, body)
	}

	// and master is completely unaffected -- it never saw t2
	var masterStreams struct {
		Streams []struct{ AggregateID string } `json:"streams"`
	}
	master.apiOK(http.MethodGet, "/api/cqrs/streams?aggregate=task", nil, &masterStreams)
	for _, s := range masterStreams.Streams {
		if s.AggregateID == "t2" {
			t.Fatal("t2 must not exist on the master: the secondary's refused command must not have leaked through")
		}
	}
}
