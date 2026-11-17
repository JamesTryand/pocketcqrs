//go:build smoke

package smoke

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The replication half of the fault-injection floor (decision 0003 in the
// venture-overview hub), in the same-host shape the rest of replica_test.go
// uses: a secondary reads the master's events.db file directly. (The
// Litestream and LiteFS transports need their own binaries, and LiteFS needs
// Linux; neither is killed here.)
//
// A secondary keeps its own progress: Go consumers' checkpoints in its own
// checkpoints.db, a JS projection's position in its own data.db. The master
// runs the same projection and is at the head of the log throughout, so a
// secondary that took the master's progress for its own would skip events,
// and these tests would see them missing.

const replicaTaskListJS = `//@trigger projection tasklist on TaskCreated
//@schema tasklist taskId:text title:text
//@key taskId
function project(event) {
  return { upsert: { key: event.aggregateId, fields: { taskId: event.aggregateId, title: event.data.title } } };
}
`

// TestSecondaryCatchUpSurvivesRealProcessKills: a secondary is killed part
// way through catching up on 1,500 created tasks and 500 completions in the
// master's log, then restarted on its own data. Its Go tasks projection and
// a JS projection must both end with one row per task, right, and their
// positions at the head of the log.
func TestSecondaryCatchUpSurvivesRealProcessKills(t *testing.T) {
	const iterations = 3
	const backlog = 1500
	completed := func(i int) bool { return i%3 == 0 }
	rng := rand.New(rand.NewSource(11))
	bin := build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(t.TempDir(), "pocketcqrs"))

	for iter := 0; iter < iterations; iter++ {
		iter := iter
		t.Run(fmt.Sprintf("iter-%d", iter), func(t *testing.T) {
			master, _ := bootNode(t, bin, t.TempDir(), fmt.Sprintf("replica-master-%d", iter), "", nil)
			writeEach(t, master, backlog, func(i int) (string, any) {
				id := fmt.Sprintf("t%d", i)
				return "task/" + id + "/CreateTask", map[string]any{"title": "title " + id}
			})
			var done []int
			for i := 0; i < backlog; i++ {
				if completed(i) {
					done = append(done, i)
				}
			}
			writeEach(t, master, len(done), func(j int) (string, any) {
				return fmt.Sprintf("task/t%d/CompleteTask", done[j]), map[string]any{}
			})
			// the master's own tasks projection is at the head
			checkpointAtHead(t, master, "tasks")

			secDir := t.TempDir()
			secondary := []string{"--cqrsRole", "secondary", "--cqrsEventsPath", filepath.Join(master.DataDir, "events.db")}
			fns := map[string]string{"tasklist.js": replicaTaskListJS}

			h, stop := bootNode(t, bin, secDir, fmt.Sprintf("replica-kill-%d", iter), "", fns, secondary...)
			var before int
			deadline := time.Now().Add(30 * time.Second)
			for {
				before = collectionCount(h, "tasks")
				if before > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the secondary never started catching up")
				}
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(time.Duration(rng.Intn(150)) * time.Millisecond)
			stop()
			if before >= backlog {
				t.Fatalf("the kill landed after catch-up had finished (%d of %d): the test needs a bigger backlog", before, backlog)
			}
			t.Logf("secondary killed with at least %d of %d tasks projected", before, backlog)

			h, _ = bootNode(t, bin, secDir, fmt.Sprintf("replica-restart-%d", iter), "", fns, secondary...)
			checkpointAtHead(t, h, "tasks")
			checkpointAtHead(t, h, "tasklist")

			rows := taskRows(t, h, "tasks")
			if len(rows) != backlog {
				t.Fatalf("the secondary's tasks projection holds %d tasks, want %d", len(rows), backlog)
			}
			list := taskRows(t, h, "tasklist")
			if len(list) != backlog {
				t.Fatalf("the secondary's JS projection holds %d tasks, want %d", len(list), backlog)
			}
			for i := 0; i < backlog; i++ {
				id := fmt.Sprintf("t%d", i)
				if want := (taskRow{"title " + id, completed(i)}); rows[id] != want {
					t.Fatalf("on the secondary, task %s reads %+v, want %+v", id, rows[id], want)
				}
				if list[id].Title != "title "+id {
					t.Fatalf("on the secondary, the JS projection's task %s reads %q", id, list[id].Title)
				}
			}
		})
	}
}

// TestForwardedCommandsSurviveMasterKills: commands are sent to a secondary,
// which forwards them to the master, and the master is killed part way
// through the burst and restarted on the same data and address. Every
// command the secondary acknowledged has exactly one event on the master,
// no stream is doubled, and the secondary's read model catches up with
// everything the master holds.
func TestForwardedCommandsSurviveMasterKills(t *testing.T) {
	const iterations = 5
	const commands = 200
	rng := rand.New(rand.NewSource(13))
	bin := build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(t.TempDir(), "pocketcqrs"))

	midBurst := 0
	defer func() {
		if !t.Failed() && midBurst == 0 {
			t.Fatalf("no master kill landed part way through a burst of %d commands; the timings need adjusting", commands)
		}
		t.Logf("%d of %d kills landed part way through a burst", midBurst, iterations)
	}()

	for iter := 0; iter < iterations; iter++ {
		iter := iter
		t.Run(fmt.Sprintf("iter-%d", iter), func(t *testing.T) {
			masterDir := t.TempDir()
			masterAddr := freeAddr(t)
			master, stopMaster := bootNode(t, bin, masterDir, fmt.Sprintf("forward-master-%d", iter), masterAddr, nil)
			sec, _ := bootNode(t, bin, t.TempDir(), fmt.Sprintf("forward-secondary-%d", iter), "", nil,
				"--cqrsRole", "secondary", "--cqrsEventsPath", filepath.Join(master.DataDir, "events.db"),
				"--cqrsMasterAddr", master.BackendURL)

			var wg sync.WaitGroup
			var mu sync.Mutex
			acked := map[string]bool{}
			firstAck := make(chan struct{})
			var firstOnce sync.Once
			for i := 0; i < commands; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					id := fmt.Sprintf("f%d", i)
					body, _ := json.Marshal(map[string]string{"title": id})
					req, err := http.NewRequest(http.MethodPost, sec.BackendURL+"/api/cqrs/task/"+id+"/CreateTask", bytes.NewReader(body))
					if err != nil {
						return
					}
					req.Header.Set("Content-Type", "application/json")
					// the master's token: the destination authenticates a forwarded command
					req.Header.Set("Authorization", master.Token)
					resp, err := sec.client.Do(req)
					if err != nil {
						return
					}
					defer resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						mu.Lock()
						acked[id] = true
						mu.Unlock()
						firstOnce.Do(func() { close(firstAck) })
					}
				}(i)
			}
			select {
			case <-firstAck:
			case <-time.After(10 * time.Second):
				t.Fatal("no forwarded command was acknowledged within 10s")
			}
			time.Sleep(time.Duration(rng.Intn(20)) * time.Millisecond)
			stopMaster()
			wg.Wait()
			t.Logf("%d of %d forwarded commands acknowledged before the master was killed", len(acked), commands)
			if len(acked) > 0 && len(acked) < commands {
				midBurst++
			}

			// the master comes back on the same data and address
			master, _ = bootNode(t, bin, masterDir, fmt.Sprintf("forward-master-restart-%d", iter), masterAddr, nil)
			eventually(t, "the restarted master's event count to stop changing", func() bool {
				a := catalogEventCount(t, master)
				time.Sleep(200 * time.Millisecond)
				return a == catalogEventCount(t, master)
			})
			// the secondary follows the restarted master's log to its head;
			// the master is read after, so anything it appended late (a
			// queued command it finished on restart) is in both
			checkpointAtHead(t, sec, "tasks")
			var streams struct {
				Streams []struct {
					AggregateID string `json:"aggregateId"`
					Events      int64  `json:"events"`
				} `json:"streams"`
			}
			master.apiOK(http.MethodGet, "/api/cqrs/streams?aggregate=task", nil, &streams)
			byID := map[string]int64{}
			for _, s := range streams.Streams {
				byID[s.AggregateID] = s.Events
				if s.Events != 1 {
					t.Fatalf("stream %s has %d events on the master, want exactly 1", s.AggregateID, s.Events)
				}
			}
			for id := range acked {
				if byID[id] != 1 {
					t.Fatalf("the secondary acknowledged %s, but the master holds %d events for it (want exactly 1)", id, byID[id])
				}
			}

			rows := taskRows(t, sec, "tasks")
			if len(rows) != len(byID) {
				var missing []string
				for id := range rows {
					if byID[id] == 0 {
						missing = append(missing, id)
					}
				}
				t.Fatalf("the secondary's read model holds %d tasks, the master %d (log heads: master %d, secondary %d); "+
					"on the secondary only: %v", len(rows), len(byID), maxPosition(t, master), maxPosition(t, sec), missing)
			}
			for id := range byID {
				if rows[id].Title != id {
					t.Fatalf("on the secondary, task %s reads %+v", id, rows[id])
				}
			}
		})
	}
}

// maxPosition is the head of the log as the node sees it.
func maxPosition(t *testing.T, h *harness) int64 {
	t.Helper()
	var catalog struct {
		Totals struct {
			MaxPosition int64 `json:"maxPosition"`
		} `json:"totals"`
	}
	h.apiOK(http.MethodGet, "/api/cqrs/catalog", nil, &catalog)
	return catalog.Totals.MaxPosition
}

// bootNode starts a node with --tutorial in dir (pb_data and pb_functions
// inside it, the superuser seeded on first use) on addr, or a free address
// when addr is empty, and returns it signed in. A second call with the same
// dir restarts the node on the same data.
func bootNode(t *testing.T, bin, dir, label, addr string, functions map[string]string, extra ...string) (*harness, func()) {
	t.Helper()
	fnDir := filepath.Join(dir, "pb_functions")
	dataDir := filepath.Join(dir, "pb_data")
	if err := os.MkdirAll(fnDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, src := range functions {
		if err := os.WriteFile(filepath.Join(fnDir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		seed := exec.Command(bin, "superuser", "upsert", superuserEmail, superuserPassword, "--dir", dataDir)
		if out, err := seed.CombinedOutput(); err != nil {
			t.Fatalf("seeding superuser failed: %v\n%s", err, out)
		}
	}
	if addr == "" {
		addr = freeAddr(t)
	}
	h := &harness{t: t, BackendURL: "http://" + addr, FunctionsDir: fnDir, DataDir: dataDir, Bin: bin, client: newClient(t)}
	stop := serve(t, bin, dir, label,
		append([]string{"serve", "--http", addr, "--dir", dataDir, "--functionsDir", fnDir, "--tutorial"}, extra...)...)
	waitFor(t, h.BackendURL+"/api/health")
	h.Token = h.authenticate()
	return h, stop
}

// collectionCount is the number of records in a collection, 0 when it cannot
// be read (yet).
func collectionCount(h *harness, collection string) int {
	var page struct {
		TotalItems int `json:"totalItems"`
	}
	status, _ := h.api(http.MethodGet, "/api/collections/"+collection+"/records?perPage=1", nil, &page)
	if status != http.StatusOK {
		return 0
	}
	return page.TotalItems
}

type taskRow struct {
	Title     string
	Completed bool
}

// taskRows reads a tasks-shaped collection (taskId, title, completed) whole,
// failing on a task with more than one row.
func taskRows(t *testing.T, h *harness, collection string) map[string]taskRow {
	t.Helper()
	rows := map[string]taskRow{}
	for p := 1; ; p++ {
		var page struct {
			TotalPages int `json:"totalPages"`
			Items      []struct {
				TaskID    string `json:"taskId"`
				Title     string `json:"title"`
				Completed bool   `json:"completed"`
			} `json:"items"`
		}
		h.apiOK(http.MethodGet, fmt.Sprintf("/api/collections/%s/records?perPage=500&page=%d", collection, p), nil, &page)
		for _, it := range page.Items {
			if _, dup := rows[it.TaskID]; dup {
				t.Fatalf("task %s has more than one row in %s", it.TaskID, collection)
			}
			rows[it.TaskID] = taskRow{it.Title, it.Completed}
		}
		if p >= page.TotalPages {
			break
		}
	}
	return rows
}
