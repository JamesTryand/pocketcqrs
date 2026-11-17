//go:build smoke

package smoke

import (
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The compiled-in half of the consumer kill tests (consumer_kill_test.go has
// the JS half and the shared harness). A Go projection or reactor applies an
// event and then saves its checkpoint, in two steps (consumers.Engine), so a
// kill between them replays the event at the next start: the consumer must
// be idempotent for that, and these tests check it is. The consumers run in
// smoke/gokill, a program of its own, which registers one consumer only when
// GOKILL_CONSUMER names it, so the backlog is written with it absent.

// TestGoProjectionCatchUpSurvivesRealProcessKills: the tutorial's Go tasks
// projection, killed part way through catching up on 1,500 created tasks and
// 500 completions. After a restart every task has exactly one row, with its
// title, completed exactly when it was completed, and the projection's
// checkpoint is at the head of the log.
func TestGoProjectionCatchUpSurvivesRealProcessKills(t *testing.T) {
	const backlog = 1500
	completed := func(i int) bool { return i%3 == 0 }
	consumerSurvivesKill(t, consumerKill{
		label:       "goprojection",
		backlog:     backlog,
		bin:         buildGoKill(t),
		consumerEnv: []string{"GOKILL_CONSUMER=projection"},
		write: func(t *testing.T, h *harness) {
			writeEach(t, h, backlog, func(i int) (string, any) {
				id := fmt.Sprintf("t%d", i)
				return "task/" + id + "/CreateTask", map[string]any{"title": "title " + id}
			})
			var done []int
			for i := 0; i < backlog; i++ {
				if completed(i) {
					done = append(done, i)
				}
			}
			writeEach(t, h, len(done), func(j int) (string, any) {
				return fmt.Sprintf("task/t%d/CompleteTask", done[j]), map[string]any{}
			})
		},
		progress: func(h *harness) int {
			var page struct {
				TotalItems int `json:"totalItems"`
			}
			status, _ := h.api(http.MethodGet, "/api/collections/tasks/records?perPage=1", nil, &page)
			if status != http.StatusOK {
				return 0
			}
			return page.TotalItems
		},
		check: func(t *testing.T, h *harness) {
			// the completions follow the creates in the log: wait for the
			// checkpoint to reach the head once, then read the rows
			checkpointAtHead(t, h, "tasks")
			type row struct {
				Title     string
				Completed bool
			}
			rows := map[string]row{}
			for p := 1; ; p++ {
				var page struct {
					TotalPages int `json:"totalPages"`
					Items      []struct {
						TaskID    string `json:"taskId"`
						Title     string `json:"title"`
						Completed bool   `json:"completed"`
					} `json:"items"`
				}
				h.apiOK(http.MethodGet, fmt.Sprintf("/api/collections/tasks/records?perPage=500&page=%d", p), nil, &page)
				for _, it := range page.Items {
					if _, dup := rows[it.TaskID]; dup {
						t.Fatalf("task %s has more than one row", it.TaskID)
					}
					rows[it.TaskID] = row{it.Title, it.Completed}
				}
				if p >= page.TotalPages {
					break
				}
			}
			if len(rows) != backlog {
				t.Fatalf("the projection holds %d tasks, want %d", len(rows), backlog)
			}
			for i := 0; i < backlog; i++ {
				id := fmt.Sprintf("t%d", i)
				want := row{"title " + id, completed(i)}
				if rows[id] != want {
					t.Fatalf("task %s reads %+v, want %+v", id, rows[id], want)
				}
			}
		},
	})
}

// TestGoReactorCatchUpSurvivesRealProcessKills: a Go reactor dispatches one
// command per created task, to "echo-<id>", killed part way through 600.
// After a restart every task has exactly one echo: none lost, none doubled,
// and no echo of an echo.
func TestGoReactorCatchUpSurvivesRealProcessKills(t *testing.T) {
	const backlog = 600
	streams := func(h *harness) map[string]int64 {
		var out struct {
			Streams []struct {
				AggregateID string `json:"aggregateId"`
				Events      int64  `json:"events"`
			} `json:"streams"`
		}
		status, _ := h.api(http.MethodGet, "/api/cqrs/streams?aggregate=task", nil, &out)
		if status != http.StatusOK {
			return nil
		}
		m := map[string]int64{}
		for _, s := range out.Streams {
			m[s.AggregateID] = s.Events
		}
		return m
	}
	echoes := func(h *harness) int {
		n := 0
		for id := range streams(h) {
			if strings.HasPrefix(id, "echo-") {
				n++
			}
		}
		return n
	}
	consumerSurvivesKill(t, consumerKill{
		label:       "goreactor",
		backlog:     backlog,
		bin:         buildGoKill(t),
		consumerEnv: []string{"GOKILL_CONSUMER=reactor"},
		write: func(t *testing.T, h *harness) {
			writeEach(t, h, backlog, func(i int) (string, any) {
				id := fmt.Sprintf("t%d", i)
				return "task/" + id + "/CreateTask", map[string]any{"title": "title " + id}
			})
		},
		progress: echoes,
		check: func(t *testing.T, h *harness) {
			checkpointAtHead(t, h, "echo")
			got := streams(h)
			for id, n := range got {
				if strings.HasPrefix(id, "echo-echo-") {
					t.Fatalf("an echo of an echo: %s", id)
				}
				if n != 1 {
					t.Fatalf("%s has %d events, want exactly 1", id, n)
				}
			}
			for i := 0; i < backlog; i++ {
				if _, ok := got[fmt.Sprintf("echo-t%d", i)]; !ok {
					t.Fatalf("task t%d has no echo", i)
				}
			}
			if len(got) != 2*backlog {
				t.Fatalf("%d task streams, want %d (each task and its echo)", len(got), 2*backlog)
			}
		},
	})
}

// buildGoKill builds the test-only program (it carries the smoke build tag,
// so it is never part of an ordinary build).
func buildGoKill(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "gokill")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-tags", "smoke", "-o", out, "github.com/jamestryand/pocketcqrs/smoke/gokill")
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building gokill: %v\n%s", err, raw)
	}
	return out
}
