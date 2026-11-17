//go:build smoke

package extcallersmoke

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/internal/localstore"
	"github.com/jamestryand/pocketcqrs/outbox"
)

// TestOutboxSurvivesRealProcessKills is the outbox's part of the
// fault-injection floor (decision 0003 in the venture-overview hub): an
// external caller, a real process, is killed while it works through a
// backlog of events, each needing a third-party call and then a follow-up
// command, and is restarted on the same files.
//
// What must hold afterwards: every source task has exactly one follow-up
// (none lost, none doubled), nothing is dead-lettered, and the outbox is
// empty. A third-party call may be made twice (a kill after the call and
// before its entry is updated repeats it: delivery is at-least-once), so
// calls are counted and reported, not required to be one.
//
// Each round checks its kill landed part way through (some follow-ups made,
// not all), so a badly timed round fails instead of passing vacuously.
func TestOutboxSurvivesRealProcessKills(t *testing.T) {
	const iterations = 3
	const backlog = 300
	rng := rand.New(rand.NewSource(11))
	caller := buildKillCaller(t)

	for iter := 0; iter < iterations; iter++ {
		iter := iter
		t.Run(fmt.Sprintf("iter-%d", iter), func(t *testing.T) {
			var mu sync.Mutex
			calls := map[string]int{}
			third := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(3 * time.Millisecond) // a call takes time, so the kill can land inside one
				mu.Lock()
				calls[r.URL.Query().Get("id")]++
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
			}))
			defer third.Close()

			h := startBackend(t)
			for i := 0; i < backlog; i++ {
				h.command("task", fmt.Sprintf("k%d", i), "CreateTask", map[string]string{"title": fmt.Sprintf("k%d", i)})
			}

			dir := t.TempDir()
			localPath := filepath.Join(dir, "caller.db")
			outboxPath := filepath.Join(dir, "caller-outbox.db")
			run := func(label string) func() {
				return serveProcessEnv(t, []string{"EXTCALLER_GATEWAY_TOKEN=" + h.Token}, caller, dir,
					fmt.Sprintf("caller-%s-%d", label, iter),
					"-sourceEventsDB", h.EventsDB, "-localStore", localPath, "-outbox", outboxPath,
					"-gatewayURL", h.BackendURL, "-thirdURL", third.URL)
			}

			// 1. the caller starts on the backlog; kill it part way through
			stop := run("kill")
			var before int
			deadline := time.Now().Add(30 * time.Second)
			for before = h.echoCount(); before == 0; before = h.echoCount() {
				if time.Now().After(deadline) {
					t.Fatal("the caller never made a follow-up")
				}
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(time.Duration(rng.Intn(300)) * time.Millisecond)
			stop()
			// the backend is still up: the count now is the count at the kill
			atKill := h.echoCount()
			if atKill >= backlog {
				t.Fatalf("the kill landed after the caller had finished (%d of %d): the test needs a bigger backlog",
					atKill, backlog)
			}
			t.Logf("killed with %d of %d follow-ups made", atKill, backlog)

			// 2. restart on the same files; it must finish the work
			stop = run("restart")
			last, since := -1, time.Now()
			for {
				got := h.echoCount()
				if got >= backlog {
					break
				}
				if got != last {
					last, since = got, time.Now()
				} else if time.Since(since) > 10*time.Second {
					t.Fatalf("after the restart the follow-ups stopped at %d of %d: %d lost to the kill",
						got, backlog, backlog-got)
				}
				time.Sleep(200 * time.Millisecond)
			}
			// every call answered, echoes included, before stopping
			eventually(t, "the outbox to empty", func() bool {
				mu.Lock()
				defer mu.Unlock()
				return len(calls) >= 2*backlog
			})
			time.Sleep(500 * time.Millisecond)
			stop()

			// 3. exactly one follow-up per source task
			streams := h.taskStreams()
			for i := 0; i < backlog; i++ {
				id := fmt.Sprintf("echo-k%d", i)
				if streams[id] != 1 {
					t.Fatalf("%s has %d events, want exactly 1", id, streams[id])
				}
			}
			for id, n := range streams {
				if strings.HasPrefix(id, "echo-echo-") {
					t.Fatalf("an echo of an echo exists (%s, %d events): the rule's chain did not stop", id, n)
				}
			}

			// 4. nothing dead-lettered, nothing left in the outbox
			stores, err := localstore.Open(h.EventsDB, localPath)
			if err != nil {
				t.Fatal(err)
			}
			letters, err := stores.Local.DeadLetters(context.Background(), false)
			stores.Close()
			if err != nil {
				t.Fatal(err)
			}
			if len(letters) != 0 {
				t.Fatalf("%d dead letters, want none: %+v", len(letters), letters)
			}
			box, err := outbox.Open(outboxPath)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := box.Pending(context.Background())
			box.Close()
			if err != nil {
				t.Fatal(err)
			}
			if len(pending) != 0 {
				t.Fatalf("%d entries left in the outbox, want none; first: %+v", len(pending), pending[0])
			}

			mu.Lock()
			repeated := 0
			for _, n := range calls {
				if n > 1 {
					repeated++
				}
			}
			mu.Unlock()
			t.Logf("third-party calls repeated for %d events (allowed: delivery is at-least-once)", repeated)
		})
	}
}

// buildKillCaller builds the test-only caller program (it carries the smoke
// build tag, so it is never part of an ordinary build).
func buildKillCaller(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "killcaller")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-tags", "smoke", "-o", out,
		"github.com/jamestryand/pocketcqrs/smoke/extcaller/killcaller")
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building killcaller: %v\n%s", err, raw)
	}
	return out
}

// echoCount is how many follow-up tasks exist.
func (h *harness) echoCount() int {
	var out struct {
		TotalItems int `json:"totalItems"`
	}
	if h.getJSON("/api/collections/tasks/records?perPage=1&filter=taskId~%22echo-%22", &out) != nil {
		return 0
	}
	return out.TotalItems
}

// taskStreams is each task stream's event count.
func (h *harness) taskStreams() map[string]int64 {
	h.t.Helper()
	var out struct {
		Streams []struct {
			AggregateID string `json:"aggregateId"`
			Events      int64  `json:"events"`
		} `json:"streams"`
	}
	if err := h.getJSON("/api/cqrs/streams?aggregate=task", &out); err != nil {
		h.t.Fatal(err)
	}
	m := map[string]int64{}
	for _, s := range out.Streams {
		m[s.AggregateID] = s.Events
	}
	return m
}

func (h *harness) getJSON(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, h.BackendURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", h.Token)
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
