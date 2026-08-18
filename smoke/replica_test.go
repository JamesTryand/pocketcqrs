//go:build smoke

package smoke

import (
	"database/sql"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// startSecondary boots a second, independent pocketcqrs process against its
// own data dir, pointed at master's events.db via --cqrsEventsPath — the
// single-writer/multi-reader shape (item 2). It reuses master's already-built
// binary rather than building a second copy. extra is appended to the serve
// args, e.g. "--cqrsMasterAddr", master.BackendURL to also wire forwarding
// (item 3).
func startSecondary(t *testing.T, master *harness, extra ...string) *harness {
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
	args := append([]string{
		"serve", "--http", addr, "--dir", dataDir, "--functionsDir", fnDir, "--tutorial",
		"--cqrsRole", "secondary", "--cqrsEventsPath", masterEventsPath,
	}, extra...)
	h.stop = serve(t, master.Bin, dir, "secondary", args...)
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

	// commands are refused outright -- this secondary has no --cqrsMasterAddr
	// configured, a deliberate choice (a pure reporting replica should never
	// accept write traffic even via forwarding -- see TestSecondaryForwardsCommandsToMaster
	// for the case where forwarding IS configured, item 3)
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

// TestSecondaryForwardsCommandsToMaster is the end-to-end proof for item 3:
// a command sent to the SECONDARY actually gets applied, via forwarding, on
// the master -- a real two-process HTTP reverse proxy hop, not a mock.
func TestSecondaryForwardsCommandsToMaster(t *testing.T) {
	master := startBackend(t, nil) // --tutorial
	secondary := startSecondary(t, master, "--cqrsMasterAddr", master.BackendURL)

	// the request carries the MASTER's token, not the secondary's: the two
	// nodes' auth records are unrelated (F-12), and Forward skips local auth
	// entirely on the secondary -- the destination is the one that
	// authenticates, exactly as gateway.Config.Forward's doc comment says.
	resp := secondary.do(http.MethodPost, secondary.BackendURL+"/api/cqrs/task/t3/CreateTask",
		jsonBody(map[string]string{"title": "via secondary"}), map[string]string{"Authorization": master.Token})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 from the forwarded command, got %d: %s", resp.StatusCode, b)
	}

	// it landed on the MASTER, not applied locally on the secondary
	var masterStreams struct {
		Streams []struct{ AggregateID string } `json:"streams"`
	}
	master.apiOK(http.MethodGet, "/api/cqrs/streams?aggregate=task", nil, &masterStreams)
	found := false
	for _, s := range masterStreams.Streams {
		if s.AggregateID == "t3" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected t3 to exist on the master after a command forwarded through the secondary")
	}

	// and it replicates back to the secondary via the ordinary read path,
	// exactly like any other master-originated event -- forwarding a write
	// does not create a second, special channel
	eventually(t, "the secondary to see t3 via replication", func() bool {
		var feed struct {
			Events []struct{ AggregateID string } `json:"events"`
		}
		status, _ := secondary.api(http.MethodGet, "/api/cqrs/events?aggregate=task", nil, &feed)
		if status != http.StatusOK {
			return false
		}
		for _, e := range feed.Events {
			if e.AggregateID == "t3" {
				return true
			}
		}
		return false
	})
}

// TestSecondaryForwardingUnaffectedByMasterBatching confirms item 3's
// forwarding path is completely unaware of whether the master it forwards
// to has command batching (item 4) turned on -- the secondary just proxies
// the raw HTTP request either way, and gets back whatever the master's
// gateway produces, synchronous batching path included.
func TestSecondaryForwardingUnaffectedByMasterBatching(t *testing.T) {
	master := startBackendFlags(t, nil, "--tutorial", "--cqrsCommandBatching")
	secondary := startSecondary(t, master, "--cqrsMasterAddr", master.BackendURL)

	resp := secondary.do(http.MethodPost, secondary.BackendURL+"/api/cqrs/task/t4/CreateTask",
		jsonBody(map[string]string{"title": "via secondary, batched master"}),
		map[string]string{"Authorization": master.Token})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 from the forwarded command against a batching master, got %d: %s", resp.StatusCode, b)
	}

	var masterStreams struct {
		Streams []struct{ AggregateID string } `json:"streams"`
	}
	master.apiOK(http.MethodGet, "/api/cqrs/streams?aggregate=task", nil, &masterStreams)
	found := false
	for _, s := range masterStreams.Streams {
		if s.AggregateID == "t4" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected t4 to exist on the batching master after a command forwarded through the secondary")
	}
}

// TestSecondaryForwardsAuthCollectionWritesToMaster is the end-to-end proof
// for item 5's remainder (F-12): PocketBase's own native auth traffic --
// not just CQRS commands -- gets routed to the master, since a secondary's
// _superusers table is a genuinely different table, not a stale replica of
// the master's.
func TestSecondaryForwardsAuthCollectionWritesToMaster(t *testing.T) {
	master := startBackend(t, nil) // --tutorial
	// --cqrsForwardAuth is required and deliberately separate from
	// --cqrsMasterAddr (see main.go's flag help): enabling it means every
	// token this secondary's own login flow hands out is master-signed and
	// unverifiable by the secondary's own local routes (the JWT signing
	// secret lives in data.db, never synced between nodes) -- so this test
	// verifies "not applied locally" via the secondary's data.db directly,
	// not an authenticated local HTTP read, which is exactly the
	// limitation this flag's help text warns about.
	secondary := startSecondary(t, master, "--cqrsMasterAddr", master.BackendURL, "--cqrsForwardAuth")

	const newEmail = "forwarded-su@example.com"
	const newPassword = "forwarded-pass-1234"

	resp := secondary.do(http.MethodPost, secondary.BackendURL+"/api/collections/_superusers/records",
		jsonBody(map[string]string{
			"email": newEmail, "password": newPassword, "passwordConfirm": newPassword,
		}), map[string]string{"Authorization": master.Token})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 creating a superuser via the secondary (forwarded to master), got %d: %s",
			resp.StatusCode, b)
	}

	filterQuery := "?filter=" + url.QueryEscape("email='"+newEmail+"'")

	// exists on the master
	var masterList struct {
		Items []struct {
			Email string `json:"email"`
		} `json:"items"`
	}
	master.apiOK(http.MethodGet, "/api/collections/_superusers/records"+filterQuery, nil, &masterList)
	if len(masterList.Items) != 1 {
		t.Fatalf("expected the forwarded superuser to exist on the master, got %d matches", len(masterList.Items))
	}

	// and NOT locally on the secondary -- checked directly against its
	// data.db, since an authenticated HTTP read isn't available here (see
	// the comment above)
	if localSuperuserCount(t, secondary.DataDir, newEmail) != 0 {
		t.Fatal("expected the forwarded write to NOT also have applied locally on the secondary")
	}
}

// localSuperuserCount queries a node's own data.db directly for _superusers
// rows matching email -- used where an authenticated HTTP read against that
// node isn't available (see TestSecondaryForwardsAuthCollectionWritesToMaster).
func localSuperuserCount(t *testing.T, dataDir, email string) int {
	t.Helper()
	dsn := "file:" + filepath.Join(dataDir, "data.db") + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM _superusers WHERE email = ?`, email).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
