//go:build smoke

package smoke

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/events"
)

// The fault-injection floor (decision 0003 in the venture-overview hub) for the two real replication
// transports, Litestream and LiteFS. Each round sends a burst of commands to the master while a secondary reads
// the log through the transport, kills one part part way through the burst (a real process kill), and restarts
// it. Afterwards every acknowledged command has exactly one event on the master, no stream is doubled, and the
// secondary holds exactly what the master holds: the same head of the log and one right row per task.
//
// They need the real binaries (LITESTREAM_BIN / LITEFS_BIN, or on PATH; LiteFS also needs Linux and FUSE) and
// skip without them, like the other tests in litestream_replica_test.go and litefs_replica_test.go.
// smoke/linux/Containerfile is an environment with both.

const liteBurst = 600

// sidecar is a long-lived helper process (litestream, litefs) that a test kills and starts again.
type sidecar struct {
	t     *testing.T
	bin   string
	args  []string
	dir   string
	label string
	// fuseDir, for litefs: the mount to clear after a kill
	fuseDir string
	n       int
	cmd     *exec.Cmd
	log     *os.File
}

func startSidecar(t *testing.T, bin, dir, label string, args ...string) *sidecar {
	t.Helper()
	s := &sidecar{t: t, bin: bin, args: args, dir: dir, label: label}
	s.start()
	t.Cleanup(s.kill)
	return s
}

func (s *sidecar) start() {
	s.t.Helper()
	s.n++
	logPath := filepath.Join(s.dir, fmt.Sprintf("%s-%d.log", s.label, s.n))
	f, err := os.Create(logPath)
	if err != nil {
		s.t.Fatal(err)
	}
	cmd := exec.Command(s.bin, s.args...)
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		s.t.Fatalf("starting %s: %v", s.label, err)
	}
	s.cmd, s.log = cmd, f
}

// kill is a real process kill (SIGKILL).
func (s *sidecar) kill() {
	if s.cmd == nil {
		return
	}
	_ = s.cmd.Process.Kill()
	_, _ = s.cmd.Process.Wait()
	s.log.Close()
	s.cmd = nil
	if s.fuseDir != "" {
		// a killed litefs leaves its FUSE mount behind, still held open by the node reading through it: a lazy
		// unmount (-z) clears it at once, as a supervisor restarting litefs would
		_ = exec.Command("fusermount3", "-uz", s.fuseDir).Run()
	}
}

// logs is every log this sidecar wrote, for a failure message.
func (s *sidecar) logs() string {
	var b bytes.Buffer
	for i := 1; i <= s.n; i++ {
		raw, _ := os.ReadFile(filepath.Join(s.dir, fmt.Sprintf("%s-%d.log", s.label, i)))
		fmt.Fprintf(&b, "---- %s-%d ----\n%s\n", s.label, i, tail(string(raw), 3000))
	}
	return b.String()
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// liteBurstTasks sends n CreateTask commands to url (16 at a time, each once, never retried) and returns the ids
// acknowledged. firstThird is closed once a third of them are acknowledged.
func liteBurstTasks(client *http.Client, url, token, prefix string, n int, firstThird chan<- struct{}) map[string]bool {
	var mu sync.Mutex
	acked := map[string]bool{}
	var once sync.Once
	ids := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ids {
				id := fmt.Sprintf("%s%d", prefix, i)
				body, _ := json.Marshal(map[string]string{"title": "title " + id})
				req, err := http.NewRequest(http.MethodPost, url+"/api/cqrs/task/"+id+"/CreateTask", bytes.NewReader(body))
				if err != nil {
					continue
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", token)
				resp, err := client.Do(req)
				if err != nil {
					continue // lost to the kill: not acknowledged
				}
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					mu.Lock()
					acked[id] = true
					if len(acked) >= n/3 {
						once.Do(func() { close(firstThird) })
					}
					mu.Unlock()
				}
			}
		}()
	}
	for i := 0; i < n; i++ {
		ids <- i
	}
	close(ids)
	wg.Wait()
	once.Do(func() { close(firstThird) })
	return acked
}

// liteTargets is the rounds to run: LITE_KILL_TARGETS (comma-separated; "none" is a round with no kill, a
// baseline) or the default.
func liteTargets(def []string) []string {
	if v := os.Getenv("LITE_KILL_TARGETS"); v != "" {
		return strings.Split(v, ",")
	}
	return def
}

// eventuallyWithin is eventually with its own time limit.
func eventuallyWithin(t *testing.T, d time.Duration, what string, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

// checkReplicated is the end state every round must reach: the master holds exactly one event per stream, every
// acknowledged command among them; the secondary reaches the master's head and projects one right row per task.
func checkReplicated(t *testing.T, master, sec *harness, acked map[string]bool, logs func() string) {
	t.Helper()
	if !eventuallyWithin(t, 30*time.Second, "the master's log to settle", func() bool {
		a := catalogEventCount(t, master)
		time.Sleep(300 * time.Millisecond)
		return a == catalogEventCount(t, master)
	}) {
		t.Fatal("the master's log never settled")
	}
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
			t.Fatalf("%s was acknowledged, but the master holds %d events for it (want exactly 1)", id, byID[id])
		}
	}
	head := maxPosition(t, master)
	var secHead int64
	if !eventuallyWithin(t, 120*time.Second, "the secondary to reach the master's head", func() bool {
		secHead = maxPosition(t, sec)
		return secHead == head
	}) {
		t.Fatalf("the secondary's log stopped at %d, the master's head is %d\n%s", secHead, head, logs())
	}
	checkpointAtHead(t, sec, "tasks")
	rows := taskRows(t, sec, "tasks")
	if len(rows) != len(byID) {
		t.Fatalf("the secondary projects %d tasks, the master holds %d", len(rows), len(byID))
	}
	for id := range byID {
		if rows[id].Title != "title "+id {
			t.Fatalf("on the secondary, task %s reads %+v", id, rows[id])
		}
	}
}

// TestLitestreamSurvivesRealProcessKills: the master's `litestream replicate`, the secondary's
// `litestream restore -f`, and the master itself, each killed part way through a burst (two rounds each).
func TestLitestreamSurvivesRealProcessKills(t *testing.T) {
	lsBin := litestreamBin(t)
	bin := build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(t.TempDir(), "pocketcqrs"))
	rng := rand.New(rand.NewSource(17))
	targets := liteTargets([]string{"replicate", "restore", "master", "replicate", "restore", "master"})
	for round, target := range targets {
		round, target := round, target
		t.Run(fmt.Sprintf("%d-%s", round, target), func(t *testing.T) {
			work := t.TempDir()
			masterDir, secDir := filepath.Join(work, "master"), filepath.Join(work, "secondary")
			masterAddr := freeAddr(t)
			master, stopMaster := bootNode(t, bin, masterDir, "master", masterAddr, nil)
			masterEvents := filepath.Join(master.DataDir, "events.db")
			cfg := litestreamConfig(t, work, masterEvents, filepath.Join(work, "replica"))
			replicate := startSidecar(t, lsBin, work, "litestream-replicate", "replicate", "-config", cfg)
			time.Sleep(2 * time.Second) // the first snapshot, before restore looks for it
			copyPath := filepath.Join(work, "secondary-events.db")
			restore := startSidecar(t, lsBin, work, "litestream-restore",
				"restore", "-f", "-follow-interval", "500ms", "-config", cfg, "-o", copyPath, masterEvents)
			waitForFile(t, copyPath)
			secArgs := []string{"--cqrsRole", "secondary", "--cqrsEventsPath", copyPath}
			sec, stopSec := bootNode(t, bin, secDir, "secondary", "", nil, secArgs...)
			logs := func() string {
				// is the copy behind, or only the running secondary's view of it? Read the file anew.
				out := replicate.logs() + restore.logs()
				fi, err := os.Stat(copyPath)
				if err != nil {
					return out + "the copy: " + err.Error()
				}
				out += fmt.Sprintf("the copy: %d bytes, modified %s\n", fi.Size(), fi.ModTime().Format(time.RFC3339Nano))
				ro, err := events.OpenReadOnly(copyPath)
				if err != nil {
					return out + "opening the copy anew: " + err.Error()
				}
				defer ro.Close()
				evs, err := ro.Poll(context.Background(), 0, 100000)
				return out + fmt.Sprintf("opened anew, the copy holds %d events (%v)\n", len(evs), err)
			}

			third := make(chan struct{})
			done := make(chan map[string]bool)
			go func() { done <- liteBurstTasks(master.client, master.BackendURL, master.Token, "s", liteBurst, third) }()
			<-third
			time.Sleep(time.Duration(rng.Intn(100)) * time.Millisecond)
			switch target {
			case "replicate":
				replicate.kill()
				time.Sleep(time.Second)
				replicate.start()
			case "restore":
				restore.kill()
				time.Sleep(time.Second)
				restore.start()
				// Litestream 0.5.17 often refuses to resume a follow killed early ("cannot resume follow mode:
				// saved TXID ... is ahead of latest snapshot ... delete ..."). The recovery it asks for, as a
				// supervisor would carry it out: delete the copy and its saved position, restore afresh, and
				// restart the secondary, which still has the deleted file open.
				time.Sleep(3 * time.Second)
				if raw, _ := os.ReadFile(restore.logPath()); bytes.Contains(raw, []byte("cannot resume follow mode")) {
					restore.kill()
					old, _ := filepath.Glob(copyPath + "*")
					for _, f := range old {
						if err := os.Remove(f); err != nil {
							t.Fatal(err)
						}
					}
					restore.start()
					waitForFile(t, copyPath)
					stopSec()
					sec, _ = bootNode(t, bin, secDir, "secondary-restart", "", nil, secArgs...)
					t.Logf("the follow could not resume; the copy was restored afresh and the secondary restarted")
				}
			case "master":
				stopMaster()
				master, _ = bootNode(t, bin, masterDir, "master-restart", masterAddr, nil)
			}
			acked := <-done
			t.Logf("%s killed after %d+ of %d acknowledged; %d acknowledged in all", target, liteBurst/3, liteBurst, len(acked))
			if target != "none" && (len(acked) == 0 || len(acked) == liteBurst && target == "master") {
				t.Fatalf("the master kill did not land part way through the burst (%d acknowledged)", len(acked))
			}
			checkReplicated(t, master, sec, acked, logs)
		})
	}
}

// logPath is the log of this sidecar's current run.
func (s *sidecar) logPath() string {
	return filepath.Join(s.dir, fmt.Sprintf("%s-%d.log", s.label, s.n))
}

// TestLiteFSSurvivesRealProcessKills: the secondary's `litefs mount`, the master, and the primary's `litefs mount`
// (taking the master's events.db away from under it), each killed part way through a burst (two rounds each).
// After a litefs kill its mount is cleared, it is started again, and the node reading through it is restarted, as a
// supervisor would.
func TestLiteFSSurvivesRealProcessKills(t *testing.T) {
	lfBin := litefsBin(t)
	bin := build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(t.TempDir(), "pocketcqrs"))
	rng := rand.New(rand.NewSource(19))
	targets := liteTargets([]string{"litefs-secondary", "master", "litefs-primary", "litefs-secondary", "master", "litefs-primary"})
	for round, target := range targets {
		round, target := round, target
		t.Run(fmt.Sprintf("%d-%s", round, target), func(t *testing.T) {
			work := t.TempDir()
			mount := func(name string) (string, string) {
				fuse, data := filepath.Join(work, name+"-mnt"), filepath.Join(work, name+"-litefs-data")
				if err := os.MkdirAll(fuse, 0o755); err != nil {
					t.Fatal(err)
				}
				return fuse, data
			}
			pFuse, pData := mount("primary")
			sFuse, sData := mount("secondary")
			pPort := strings.TrimPrefix(freeAddr(t), "127.0.0.1")
			sPort := strings.TrimPrefix(freeAddr(t), "127.0.0.1")
			primaryURL := "http://127.0.0.1" + pPort
			for _, d := range []string{"pcfg", "scfg"} {
				if err := os.MkdirAll(filepath.Join(work, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			pCfg := litefsConfig(t, filepath.Join(work, "pcfg"), pFuse, pData, pPort, primaryURL, true)
			sCfg := litefsConfig(t, filepath.Join(work, "scfg"), sFuse, sData, sPort, primaryURL, false)

			primary := &sidecar{t: t, bin: lfBin, args: []string{"mount", "-config", pCfg}, dir: work, label: "litefs-primary", fuseDir: pFuse}
			primary.start()
			t.Cleanup(primary.kill)
			waitForLogContains(t, primary.logPath(), "primary lease acquired")

			masterDir, secDir := filepath.Join(work, "master"), filepath.Join(work, "secondary")
			masterAddr := freeAddr(t)
			masterArgs := []string{"--cqrsEventsPath", filepath.Join(pFuse, "events.db")}
			master, stopMaster := bootNode(t, bin, masterDir, "master", masterAddr, nil, masterArgs...)

			secondary := &sidecar{t: t, bin: lfBin, args: []string{"mount", "-config", sCfg}, dir: work, label: "litefs-secondary", fuseDir: sFuse}
			secondary.start()
			t.Cleanup(secondary.kill)
			secEvents := filepath.Join(sFuse, "events.db")
			waitForFile(t, secEvents)
			secArgs := []string{"--cqrsRole", "secondary", "--cqrsEventsPath", secEvents}
			sec, stopSec := bootNode(t, bin, secDir, "secondary", "", nil, secArgs...)
			logs := func() string { return primary.logs() + secondary.logs() }

			third := make(chan struct{})
			done := make(chan map[string]bool)
			go func() { done <- liteBurstTasks(master.client, master.BackendURL, master.Token, "f", liteBurst, third) }()
			<-third
			time.Sleep(time.Duration(rng.Intn(100)) * time.Millisecond)
			switch target {
			case "litefs-secondary":
				secondary.kill()
				time.Sleep(time.Second)
				secondary.start()
				waitForFile(t, secEvents)
				stopSec()
				sec, _ = bootNode(t, bin, secDir, "secondary-restart", "", nil, secArgs...)
			case "master":
				stopMaster()
				master, _ = bootNode(t, bin, masterDir, "master-restart", masterAddr, nil, masterArgs...)
			case "litefs-primary":
				primary.kill()
				time.Sleep(time.Second)
				primary.start()
				waitForLogContains(t, primary.logPath(), "primary lease acquired")
				if os.Getenv("LITE_KILL_DIAGNOSE") != "" {
					time.Sleep(2 * time.Second)
					entries, err := os.ReadDir(pFuse)
					var names []string
					for _, e := range entries {
						names = append(names, e.Name())
					}
					raw, _ := os.ReadFile(primary.logPath())
					t.Logf("after the restart the mount holds %v (%v); litefs says:\n%s", names, err, tail(string(raw), 3000))
				}
				stopMaster()
				master, _ = bootNode(t, bin, masterDir, "master-restart", masterAddr, nil, masterArgs...)
			}
			acked := <-done
			t.Logf("%s killed after %d+ of %d acknowledged; %d acknowledged in all", target, liteBurst/3, liteBurst, len(acked))
			if target != "none" && len(acked) == 0 {
				t.Fatal("nothing was acknowledged")
			}
			checkReplicated(t, master, sec, acked, logs)
		})
	}
}
