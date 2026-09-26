//go:build smoke

package smoke

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestSecondaryRunsNoSideEffects: a secondary folds the log into its own
// read models, but effect functions, reactors and cron are the MASTER's job.
// Before this was fixed every secondary registered them too, so an effect
// fired once per node, its dead-letter write failed against the read-only
// store, and each reactor dispatch bounced off events.ErrReadOnly.
//
// Both nodes are given the SAME function files, as a real fleet deployed
// from one repo would be; the effect calls a counting HTTP server, so a
// second delivery is observable, not inferred from a log line.
func TestSecondaryRunsNoSideEffects(t *testing.T) {
	var hits atomic.Int64
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer sink.Close()

	functions := map[string]string{
		// effect: one outbound call per TaskCreated
		"hook.js": "//@trigger event TaskCreated\n$http.get(\"" + sink.URL + "/hit\");\n",
		// effect that always fails, so the master dead-letters it
		"poison.js": poisonFn,
		// reactor: every top-level task gets one follow-up
		"followup.js": `//@trigger reactor TaskCreated
//@dispatches task/CreateTask
function reactTo(event) {
  if (event.aggregateId.indexOf("followup-") === 0) return [];
  return [{ aggregate: "task", id: "followup-" + event.aggregateId, command: "CreateTask",
            payload: { title: "follow up on " + event.aggregateId } }];
}
`,
		"tick.js": "//@trigger cron */5 * * * *\nconsole.log('tick');\n",
	}
	outboundFlags := []string{
		"--cqrsAllowOutboundHTTP", "--cqrsOutboundHost", "127.0.0.1", "--cqrsAllowPrivateOutbound",
	}

	master := startBackendFlags(t, functions, append([]string{"--tutorial"}, outboundFlags...)...)
	secondary := startSecondaryWith(t, master, filepath.Join(master.DataDir, "events.db"), functions, outboundFlags...)

	master.command("task", "t1", "CreateTask", map[string]string{"title": "root"})

	// projections DO run on the secondary: it sees both the root task and
	// the master's reactor output in its own local read model
	eventually(t, "the secondary's local tasks collection to have t1 and its follow-up", func() bool {
		var records struct {
			Items []struct {
				TaskID string `json:"taskId"`
			} `json:"items"`
		}
		status, _ := secondary.api(http.MethodGet, "/api/collections/tasks/records?perPage=50", nil, &records)
		if status != http.StatusOK {
			return false
		}
		seen := map[string]bool{}
		for _, r := range records.Items {
			seen[r.TaskID] = true
		}
		return seen["t1"] && seen["followup-t1"]
	})

	// two TaskCreated events (t1, followup-t1) => exactly two effect calls,
	// all from the master. Give a duplicate delivery time to show up.
	eventually(t, "the master's effect calls", func() bool { return hits.Load() >= 2 })
	time.Sleep(3 * time.Second)
	if got := hits.Load(); got != 2 {
		t.Fatalf("effect function fired %d times for 2 events: a secondary must not re-run effects", got)
	}

	// the secondary's engine carries no effect or reactor consumers
	var cat struct {
		Consumers []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"consumers"`
	}
	secondary.apiOK(http.MethodGet, "/api/cqrs/catalog", nil, &cat)
	for _, c := range cat.Consumers {
		if c.Kind == "effect-function" || c.Kind == "js-reactor" || c.Kind == "reactor" {
			t.Errorf("secondary registered side-effecting consumer %s (%s)", c.Name, c.Kind)
		}
	}

	// and a hot reload on the secondary does not register them either
	report := secondary.reload()
	for _, key := range []string{"effectsReloaded", "reactorsReloaded", "cronReloaded"} {
		if list, _ := report[key].([]any); len(list) != 0 {
			t.Errorf("secondary reload registered %s: %v", key, list)
		}
	}

	// the master is unaffected: it still runs all three
	masterReport := master.reload()
	for _, key := range []string{"effectsReloaded", "reactorsReloaded", "cronReloaded"} {
		if list, _ := masterReport[key].([]any); len(list) == 0 {
			t.Errorf("master reload should still register %s: %v", key, masterReport)
		}
	}
}
