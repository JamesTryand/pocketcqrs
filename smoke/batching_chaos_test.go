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

// TestBatchingSurvivesRealProcessKills is the heavy, real-process half of
// item 4's fault-injection requirement. The fast, in-process variant
// (batching package's TestChaosPreCommitCrashReplayIsDeterministic, 300
// randomized iterations) deterministically proves the exact crash windows
// by injecting a fault at the precise commit boundary. This exercises the
// same guarantee end to end instead -- real file I/O, real SQLite WAL
// behavior, a real killed process, not a simulated one -- to catch any
// OS/SQLite-level surprise the in-process fault hook can't see.
//
// Iteration count is deliberately modest, not the 20-50 the design first
// sketched: each iteration boots a full PocketBase process twice (real
// wall-clock seconds), unlike the fast variant's sub-millisecond
// iterations. Enough to exercise a real spread of kill timings without
// making the smoke suite prohibitively slow.
func TestBatchingSurvivesRealProcessKills(t *testing.T) {
	const iterations = 8
	const commandsPerIteration = 60
	rng := rand.New(rand.NewSource(1))

	bin := build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(t.TempDir(), "pocketcqrs"))

	for iter := 0; iter < iterations; iter++ {
		iter := iter
		t.Run(fmt.Sprintf("iter-%d", iter), func(t *testing.T) {
			dir := t.TempDir()
			fnDir := filepath.Join(dir, "pb_functions")
			if err := os.MkdirAll(fnDir, 0o755); err != nil {
				t.Fatal(err)
			}
			dataDir := filepath.Join(dir, "pb_data")

			seed := exec.Command(bin, "superuser", "upsert", superuserEmail, superuserPassword, "--dir", dataDir)
			if out, err := seed.CombinedOutput(); err != nil {
				t.Fatalf("seeding superuser failed: %v\n%s", err, out)
			}

			addr := freeAddr(t)
			h := &harness{t: t, BackendURL: "http://" + addr, FunctionsDir: fnDir, DataDir: dataDir, Bin: bin, client: newClient(t)}
			stop := serve(t, bin, dir, fmt.Sprintf("batching-kill-%d", iter),
				"serve", "--http", addr, "--dir", dataDir, "--functionsDir", fnDir, "--tutorial",
				"--cqrsCommandBatching")
			waitFor(t, h.BackendURL+"/api/health")
			h.Token = h.authenticate()

			// fire a burst of concurrent, independent-stream commands.
			// Each goroutine tracks its OWN request's outcome directly --
			// it must NOT use h.api/h.do, which call t.Fatal on a
			// transport error and are unsafe to call from a non-test
			// goroutine; a request losing its connection to the kill is
			// an EXPECTED outcome here, not a test failure.
			var wg sync.WaitGroup
			var mu sync.Mutex
			succeeded := map[string]bool{}
			for i := 0; i < commandsPerIteration; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					id := fmt.Sprintf("k%d", i)
					body, _ := json.Marshal(map[string]string{"title": id})
					req, err := http.NewRequest(http.MethodPost,
						h.BackendURL+"/api/cqrs/task/"+id+"/CreateTask", bytes.NewReader(body))
					if err != nil {
						return
					}
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", h.Token)
					resp, err := h.client.Do(req)
					if err != nil {
						return // connection lost to the kill -- not a failure
					}
					defer resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						mu.Lock()
						succeeded[id] = true
						mu.Unlock()
					}
				}(i)
			}

			// kill at a randomized short delay -- somewhere in the middle
			// of the burst landing, not reliably before or after it
			time.Sleep(time.Duration(rng.Intn(30)+2) * time.Millisecond)
			stop()
			wg.Wait()

			// restart against the SAME data dir
			addr2 := freeAddr(t)
			h2 := &harness{t: t, BackendURL: "http://" + addr2, FunctionsDir: fnDir, DataDir: dataDir, Bin: bin, client: newClient(t)}
			serve(t, bin, dir, fmt.Sprintf("batching-restart-%d", iter),
				"serve", "--http", addr2, "--dir", dataDir, "--functionsDir", fnDir, "--tutorial",
				"--cqrsCommandBatching")
			waitFor(t, h2.BackendURL+"/api/health")
			h2.Token = h2.authenticate()

			// give the restarted writer a beat to settle any leftover
			// queue rows (resumed or freshly decided) before asserting
			eventually(t, "the restarted node's event count to stop changing", func() bool {
				before := catalogEventCount(t, h2)
				time.Sleep(200 * time.Millisecond)
				after := catalogEventCount(t, h2)
				return before == after
			})

			var streams struct {
				Streams []struct {
					AggregateID string `json:"aggregateId"`
					Events      int64  `json:"events"`
				} `json:"streams"`
			}
			h2.apiOK(http.MethodGet, "/api/cqrs/streams?aggregate=task", nil, &streams)
			byID := map[string]int64{}
			for _, s := range streams.Streams {
				byID[s.AggregateID] = s.Events
			}

			// every command that got a client-side 200 BEFORE the kill
			// must have exactly one matching event now -- not zero
			// (lost), not more than one (double-applied)
			for id := range succeeded {
				if byID[id] != 1 {
					t.Fatalf("command %s got a 200 before the kill but stream now has %d events (want exactly 1)",
						id, byID[id])
				}
			}

			// no stream has more than the single Create it could
			// legitimately have -- catches a double-apply on ANY stream,
			// not just ones this test happened to observe succeeding
			for _, s := range streams.Streams {
				if s.Events != 1 {
					t.Fatalf("stream %s has %d events, expected exactly 1 (double-apply or corruption)",
						s.AggregateID, s.Events)
				}
			}
		})
	}
}

func catalogEventCount(t *testing.T, h *harness) int64 {
	t.Helper()
	var catalog struct {
		Totals struct {
			Events int64 `json:"events"`
		} `json:"totals"`
	}
	h.apiOK(http.MethodGet, "/api/cqrs/catalog", nil, &catalog)
	return catalog.Totals.Events
}
