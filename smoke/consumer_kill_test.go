//go:build smoke

package smoke

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/outbox"
)

// The consumer half of the fault-injection floor (decision 0003 in the
// venture-overview hub): a real server is killed while a consumer is part
// way through catching up on the log, restarted on the same data, and the
// consumer's output is checked for loss, corruption and doubles.
//
// The backlog is written first with the consumer absent, so the kill lands
// in catch-up, not in an idle loop. Each test checks that it did land part
// way (some of the backlog consumed, not all of it), so a run that killed
// too early or too late fails instead of passing vacuously.

const killNoteJS = `//@trigger decider note
//@handles NoteCreated
//@commands CreateNote

function initialState() { return { exists: false }; }
function decide(command, state) {
  if (command.name !== "CreateNote") throw new Error("unknown command: " + command.name);
  if (state.exists) throw new Error("note already exists");
  return [{ type: "NoteCreated", data: { title: command.payload.title } }];
}
function evolve(state, event) {
  if (event.type === "NoteCreated") { state.exists = true; }
  return state;
}
`

// TestProjectionCatchUpSurvivesRealProcessKills: a JS projection commits its
// rows and its position in one transaction (consumers.Committer). Killed
// mid-catch-up, it must resume where it was: every note has exactly one row
// with the right title, and its position reaches the head of the log.
// (Its row ops are upserts by key, so a replayed event would rewrite the
// same row; what this can catch is a lost event, a wrong value, or a
// position that runs ahead of or behind the rows.)
func TestProjectionCatchUpSurvivesRealProcessKills(t *testing.T) {
	const notesJS = `//@trigger projection notes on NoteCreated
//@schema notes noteId:text title:text
//@key noteId
function project(event) {
  return { upsert: { key: event.aggregateId, fields: { noteId: event.aggregateId, title: event.data.title } } };
}
`
	const backlog = 1500
	consumerSurvivesKill(t, consumerKill{
		label:     "projection",
		backlog:   backlog,
		functions: map[string]string{"notes.js": notesJS},
		progress: func(h *harness) int {
			var page struct {
				TotalItems int `json:"totalItems"`
			}
			status, _ := h.api(http.MethodGet, "/api/collections/notes/records?perPage=1", nil, &page)
			if status != http.StatusOK {
				return 0
			}
			return page.TotalItems
		},
		check: func(t *testing.T, h *harness) {
			titles := map[string]string{}
			for p := 1; ; p++ {
				var page struct {
					TotalPages int `json:"totalPages"`
					Items      []struct {
						NoteID string `json:"noteId"`
						Title  string `json:"title"`
					} `json:"items"`
				}
				h.apiOK(http.MethodGet, fmt.Sprintf("/api/collections/notes/records?perPage=500&page=%d", p), nil, &page)
				for _, it := range page.Items {
					if _, dup := titles[it.NoteID]; dup {
						t.Fatalf("note %s has more than one row", it.NoteID)
					}
					titles[it.NoteID] = it.Title
				}
				if p >= page.TotalPages {
					break
				}
			}
			if len(titles) != backlog {
				t.Fatalf("the projection holds %d notes, want %d", len(titles), backlog)
			}
			for i := 0; i < backlog; i++ {
				id := fmt.Sprintf("n%d", i)
				if titles[id] != "title "+id {
					t.Fatalf("note %s reads %q, want %q", id, titles[id], "title "+id)
				}
			}
			checkpointAtHead(t, h, "notes")
		},
	})
}

// TestReactorCatchUpSurvivesRealProcessKills: a reactor dispatches one
// command per event, to an id derived from the event, and only then
// advances its checkpoint, so a kill between the two re-runs it at startup
// (docs/js-guide.md, "Redelivery"). Killed mid-catch-up, every note must end
// with exactly one stamp: none lost, none doubled.
func TestReactorCatchUpSurvivesRealProcessKills(t *testing.T) {
	const stampJS = `//@trigger decider stamp
//@handles Stamped
//@commands Stamp

function initialState() { return { exists: false }; }
function decide(command, state) {
  if (command.name !== "Stamp") throw new Error("unknown command: " + command.name);
  if (state.exists) throw new Error("stamp already exists");
  return [{ type: "Stamped", data: { note: command.payload.note } }];
}
function evolve(state, event) {
  if (event.type === "Stamped") { state.exists = true; }
  return state;
}
`
	const stamperJS = `//@trigger reactor NoteCreated
//@dispatches stamp/Stamp

function reactTo(event) {
  return [{ aggregate: "stamp", id: "stamp-" + event.aggregateId, command: "Stamp", payload: { note: event.aggregateId } }];
}
`
	const backlog = 600
	stamps := func(h *harness) map[string]int64 {
		var out struct {
			Streams []struct {
				AggregateID string `json:"aggregateId"`
				Events      int64  `json:"events"`
			} `json:"streams"`
		}
		status, _ := h.api(http.MethodGet, "/api/cqrs/streams?aggregate=stamp", nil, &out)
		if status != http.StatusOK {
			return nil
		}
		m := map[string]int64{}
		for _, s := range out.Streams {
			m[s.AggregateID] = s.Events
		}
		return m
	}
	consumerSurvivesKill(t, consumerKill{
		label:   "reactor",
		backlog: backlog,
		// the stamp decider is there from the start: only the reactor is new
		baseFunctions: map[string]string{"stamp.js": stampJS},
		functions:     map[string]string{"stamper.js": stamperJS},
		progress:      func(h *harness) int { return len(stamps(h)) },
		check: func(t *testing.T, h *harness) {
			got := stamps(h)
			if len(got) != backlog {
				t.Fatalf("%d notes are stamped, want %d", len(got), backlog)
			}
			for i := 0; i < backlog; i++ {
				id := fmt.Sprintf("stamp-n%d", i)
				if got[id] != 1 {
					t.Fatalf("%s has %d events, want exactly 1", id, got[id])
				}
			}
			var dl struct {
				DeadLetters []any `json:"deadLetters"`
			}
			h.apiOK(http.MethodGet, "/api/cqrs/deadletters", nil, &dl)
			if len(dl.DeadLetters) != 0 {
				t.Fatalf("%d dead letters after the restart, want none: %+v", len(dl.DeadLetters), dl.DeadLetters)
			}
			checkpointAtHead(t, h, "stamper")
		},
	})
}

// TestEffectOutboxSurvivesRealProcessKills: an effect function's work is
// recorded in the server's outbox on the engine's pass and run off it by the
// outbox worker (functions.Apply, outbox.Worker). Killed part way through a
// backlog of calls, the server must make every call after the restart:
// none lost, nothing dead-lettered, the outbox empty. A call may be made
// twice (a kill after the call and before its entry is taken off repeats
// it: effects are at-least-once), so repeats are reported, not refused.
func TestEffectOutboxSurvivesRealProcessKills(t *testing.T) {
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
	seen := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(calls)
	}

	notifyJS := `//@trigger event NoteCreated
const res = $http.get("` + third.URL + `/hit?id=" + encodeURIComponent(event.aggregateId));
if (res.status >= 400) { throw new Error("notify failed: " + res.status); }
`
	const backlog = 600
	consumerSurvivesKill(t, consumerKill{
		label:      "effect",
		backlog:    backlog,
		functions:  map[string]string{"notify.js": notifyJS},
		serveFlags: []string{"--cqrsAllowOutboundHTTP", "--cqrsOutboundHost", "127.0.0.1", "--cqrsAllowPrivateOutbound"},
		progress:   func(*harness) int { return seen() },
		reset: func() {
			mu.Lock()
			calls = map[string]int{}
			mu.Unlock()
		},
		check: func(t *testing.T, h *harness) {
			mu.Lock()
			repeated := 0
			for i := 0; i < backlog; i++ {
				id := fmt.Sprintf("n%d", i)
				if calls[id] == 0 {
					mu.Unlock()
					t.Fatalf("note %s was never called for", id)
				}
				if calls[id] > 1 {
					repeated++
				}
			}
			mu.Unlock()
			var dl struct {
				DeadLetters []any `json:"deadLetters"`
			}
			h.apiOK(http.MethodGet, "/api/cqrs/deadletters", nil, &dl)
			if len(dl.DeadLetters) != 0 {
				t.Fatalf("%d dead letters after the restart, want none: %+v", len(dl.DeadLetters), dl.DeadLetters)
			}
			eventually(t, "the server's outbox to empty", func() bool {
				box, err := outbox.Open(filepath.Join(h.DataDir, "outbox.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer box.Close()
				pending, err := box.Pending(context.Background())
				return err == nil && len(pending) == 0
			})
			t.Logf("calls repeated for %d notes (allowed: effects are at-least-once)", repeated)
		},
	})
}

type consumerKill struct {
	label         string
	backlog       int
	serveFlags    []string                       // extra serve flags, every boot
	reset         func()                         // optional: clears test-side state at the start of each round
	baseFunctions map[string]string              // present from the first boot
	functions     map[string]string              // the consumer under test, added after the backlog
	bin           string                         // optional: the program to run (default: pocketcqrs itself)
	consumerEnv   []string                       // optional: environment that adds the consumer (a compiled-in one)
	write         func(t *testing.T, h *harness) // optional: writes the backlog (default: writeNotes)
	progress      func(h *harness) int
	check         func(t *testing.T, h *harness)
}

func consumerSurvivesKill(t *testing.T, k consumerKill) {
	const iterations = 3
	rng := rand.New(rand.NewSource(7))
	bin := k.bin
	if bin == "" {
		bin = build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(t.TempDir(), "pocketcqrs"))
	}

	for iter := 0; iter < iterations; iter++ {
		iter := iter
		t.Run(fmt.Sprintf("iter-%d", iter), func(t *testing.T) {
			if k.reset != nil {
				k.reset()
			}
			dir := t.TempDir()
			fnDir := filepath.Join(dir, "pb_functions")
			dataDir := filepath.Join(dir, "pb_data")
			if err := os.MkdirAll(fnDir, 0o755); err != nil {
				t.Fatal(err)
			}
			write := func(files map[string]string) {
				for name, src := range files {
					if err := os.WriteFile(filepath.Join(fnDir, name), []byte(src), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			write(map[string]string{"note.js": killNoteJS})
			write(k.baseFunctions)
			seed := exec.Command(bin, "superuser", "upsert", superuserEmail, superuserPassword, "--dir", dataDir)
			if out, err := seed.CombinedOutput(); err != nil {
				t.Fatalf("seeding superuser failed: %v\n%s", err, out)
			}
			boot := func(name string, env []string) (*harness, func()) {
				addr := freeAddr(t)
				h := &harness{t: t, BackendURL: "http://" + addr, FunctionsDir: fnDir, DataDir: dataDir, Bin: bin, client: newClient(t)}
				stop, _ := serveProcessEnv(t, env, bin, dir, fmt.Sprintf("%s-%s-%d", k.label, name, iter),
					append([]string{"serve", "--http", addr, "--dir", dataDir, "--functionsDir", fnDir}, k.serveFlags...)...)
				waitFor(t, h.BackendURL+"/api/health")
				h.Token = h.authenticate()
				return h, stop
			}

			// 1. the backlog, with the consumer absent
			h, stop := boot("backlog", nil)
			if k.write != nil {
				k.write(t, h)
			} else {
				writeNotes(t, h, k.backlog)
			}
			stop()

			// 2. the consumer arrives; kill the server part way through its catch-up
			write(k.functions)
			h, stop = boot("kill", k.consumerEnv)
			var before int
			deadline := time.Now().Add(30 * time.Second)
			for {
				before = k.progress(h)
				if before > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the consumer never started catching up")
				}
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(time.Duration(rng.Intn(150)) * time.Millisecond)
			stop()
			if before >= k.backlog {
				t.Fatalf("the kill landed after catch-up had finished (%d of %d): the test needs a bigger backlog",
					before, k.backlog)
			}
			t.Logf("killed with at least %d of %d consumed", before, k.backlog)

			// 3. restart on the same data: catch-up resumes and finishes
			h, _ = boot("restart", k.consumerEnv)
			// catch-up finishing, or the count settling short of the backlog: lost events
			last, since := -1, time.Now()
			for {
				got := k.progress(h)
				if got >= k.backlog {
					break
				}
				if got != last {
					last, since = got, time.Now()
				} else if time.Since(since) > 10*time.Second {
					t.Fatalf("after the restart the consumer stopped at %d of %d: %d events lost to the kill",
						got, k.backlog, k.backlog-got)
				}
				time.Sleep(200 * time.Millisecond)
			}
			// let anything still in flight settle before asserting
			eventually(t, "the event count to stop changing", func() bool {
				a := catalogEventCount(t, h)
				time.Sleep(300 * time.Millisecond)
				return a == catalogEventCount(t, h)
			})
			k.check(t, h)
		})
	}
}

// writeNotes creates n notes (n0..n{n-1}) concurrently and requires every
// one to be acknowledged. Goroutines report errors rather than calling
// t.Fatal, which is only safe on the test's own goroutine.
func writeNotes(t *testing.T, h *harness, n int) {
	t.Helper()
	writeEach(t, h, n, func(i int) (string, any) {
		id := fmt.Sprintf("n%d", i)
		return "note/" + id + "/CreateNote", map[string]any{"title": "title " + id}
	})
}

// writeEach sends n commands concurrently, the i-th to /api/cqrs/<path> with
// body as cmd(i) gives them, and requires every one to be acknowledged.
func writeEach(t *testing.T, h *harness, n int, cmd func(i int) (path string, body any)) {
	t.Helper()
	const workers = 16
	ids := make(chan int)
	var mu sync.Mutex
	var failures []string
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ids {
				path, payload := cmd(i)
				id := path
				body, _ := json.Marshal(payload)
				req, err := http.NewRequest(http.MethodPost, h.BackendURL+"/api/cqrs/"+path, bytes.NewReader(body))
				if err == nil {
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", h.Token)
					var resp *http.Response
					resp, err = h.client.Do(req)
					if err == nil {
						resp.Body.Close()
						if resp.StatusCode != http.StatusOK {
							err = fmt.Errorf("status %d", resp.StatusCode)
						}
					}
				}
				if err != nil {
					mu.Lock()
					failures = append(failures, id+": "+err.Error())
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
	if len(failures) > 0 {
		t.Fatalf("%d of %d commands were not acknowledged, first: %s", len(failures), n, failures[0])
	}
}

// checkpointAtHead requires the named consumer's checkpoint (the catalog's
// name for it ends with name) to have reached the head of the log.
func checkpointAtHead(t *testing.T, h *harness, name string) {
	t.Helper()
	var catalog struct {
		Totals struct {
			MaxPosition int64 `json:"maxPosition"`
		} `json:"totals"`
		Consumers []struct {
			Name       string `json:"name"`
			Checkpoint int64  `json:"checkpoint"`
		} `json:"consumers"`
	}
	var names []string
	ok := func() bool {
		h.apiOK(http.MethodGet, "/api/cqrs/catalog", nil, &catalog)
		names = names[:0]
		for _, c := range catalog.Consumers {
			names = append(names, fmt.Sprintf("%s@%d", c.Name, c.Checkpoint))
			if (c.Name == name || strings.HasSuffix(c.Name, ":"+name) || strings.HasSuffix(c.Name, ":"+name+".js")) &&
				c.Checkpoint == catalog.Totals.MaxPosition {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("consumer %q is not at the head of the log (%d); consumers: %s",
				name, catalog.Totals.MaxPosition, strings.Join(names, ", "))
		}
		time.Sleep(200 * time.Millisecond)
	}
}
