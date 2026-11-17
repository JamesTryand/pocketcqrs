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

// TestJSDeciderWritesSurviveRealProcessKills is the write-path kill test for a JS decider (the
// shape of a generated domain), with an Idempotency-Key on every command, as a trusted caller
// such as the landing-pages intake sends: every acknowledged command must have its event after a
// restart, and no stream more than one.
func TestJSDeciderWritesSurviveRealProcessKills(t *testing.T) {
	jsWritesSurviveKills(t, "jskeyed", true)
}

func jsWritesSurviveKills(t *testing.T, label string, keyed bool, serveFlags ...string) {
	const iterations = 8
	const commands = 200
	rng := rand.New(rand.NewSource(2))
	bin := build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(t.TempDir(), "pocketcqrs"))
	midBurst := 0
	for iter := 0; iter < iterations; iter++ {
		t.Run(fmt.Sprintf("iter-%d", iter), func(t *testing.T) {
			dir := t.TempDir()
			fnDir := filepath.Join(dir, "pb_functions")
			dataDir := filepath.Join(dir, "pb_data")
			if err := os.MkdirAll(fnDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(fnDir, "note.js"), []byte(killNoteJS), 0o644); err != nil {
				t.Fatal(err)
			}
			seed := exec.Command(bin, "superuser", "upsert", superuserEmail, superuserPassword, "--dir", dataDir)
			if out, err := seed.CombinedOutput(); err != nil {
				t.Fatalf("seeding superuser failed: %v\n%s", err, out)
			}
			boot := func(name string) (*harness, func()) {
				addr := freeAddr(t)
				h := &harness{t: t, BackendURL: "http://" + addr, FunctionsDir: fnDir, DataDir: dataDir, Bin: bin, client: newClient(t)}
				stop := serve(t, bin, dir, fmt.Sprintf("%s-%s-%d", label, name, iter),
					append([]string{"serve", "--http", addr, "--dir", dataDir, "--functionsDir", fnDir}, serveFlags...)...)
				waitFor(t, h.BackendURL+"/api/health")
				h.Token = h.authenticate()
				return h, stop
			}
			h, stop := boot("kill")

			var wg sync.WaitGroup
			var mu sync.Mutex
			acked := map[string]bool{}
			firstAck := make(chan struct{})
			var once sync.Once
			for i := 0; i < commands; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					id := fmt.Sprintf("j%d", i)
					body, _ := json.Marshal(map[string]string{"title": id})
					req, err := http.NewRequest(http.MethodPost, h.BackendURL+"/api/cqrs/note/"+id+"/CreateNote", bytes.NewReader(body))
					if err != nil {
						return
					}
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", h.Token)
					if keyed {
						req.Header.Set("Idempotency-Key", "key-"+id)
					}
					resp, err := h.client.Do(req)
					if err != nil {
						return
					}
					defer resp.Body.Close()
					if resp.StatusCode == http.StatusOK {
						mu.Lock()
						acked[id] = true
						mu.Unlock()
						once.Do(func() { close(firstAck) })
					}
				}(i)
			}
			select {
			case <-firstAck:
			case <-time.After(10 * time.Second):
				t.Fatal("no command was acknowledged within 10s")
			}
			time.Sleep(time.Duration(rng.Intn(200)) * time.Millisecond)
			stop()
			wg.Wait()
			if len(acked) > 0 && len(acked) < commands {
				midBurst++
			}
			t.Logf("%d of %d acknowledged before the kill", len(acked), commands)

			h, _ = boot("restart")
			eventually(t, "the event count to settle", func() bool {
				a := catalogEventCount(t, h)
				time.Sleep(300 * time.Millisecond)
				return a == catalogEventCount(t, h)
			})
			var out struct {
				Streams []struct {
					AggregateID string `json:"aggregateId"`
					Events      int64  `json:"events"`
				} `json:"streams"`
			}
			h.apiOK(http.MethodGet, "/api/cqrs/streams?aggregate=note", nil, &out)
			byID := map[string]int64{}
			for _, s := range out.Streams {
				byID[s.AggregateID] = s.Events
				if s.Events != 1 {
					t.Fatalf("stream %s has %d events, want exactly 1", s.AggregateID, s.Events)
				}
			}
			lost := 0
			for id := range acked {
				if byID[id] != 1 {
					lost++
				}
			}
			if lost > 0 {
				t.Fatalf("%d of %d acknowledged commands have no event after the restart", lost, len(acked))
			}
		})
	}
	if !t.Failed() && midBurst == 0 {
		t.Fatal("no kill landed part way through a burst")
	}
}
