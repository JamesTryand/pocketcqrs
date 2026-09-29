package opsport

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/nodeidentity"
)

// GET /readyz (contract section 4) for the lifecycle and read-model
// dimensions: one test per row of STATE-MACHINES.md's "Readiness: lifecycle"
// and "Readiness: read_models" tables that this step covers, plus machine 1's
// catch-up transitions. The consumer status is faked; the consumers package
// tests cover how the engine produces it.

type node struct {
	t         *testing.T
	health    *Health
	clock     time.Time
	mu        sync.Mutex
	consumers []consumers.Status
	log       []string
}

func newNode(t *testing.T, role string) *node {
	n := &node{t: t, clock: started}
	n.health = New("node-3", started)
	n.health.now = func() time.Time {
		n.mu.Lock()
		defer n.mu.Unlock()
		return n.clock
	}
	n.health.SetIdentity(nodeidentity.Identity{
		NodeID: "0192b5c4-7e1a-7c3e-9f00-5b2d8a1c4e77", Kind: nodeidentity.Persistent,
		Instance: "timesheets", Host: "node-3", Stack: nodeidentity.Stack, Role: role, StartedAt: started,
	})
	// a reader's replication is fresh here; replication_test.go covers the rest
	if role == "reader" {
		n.health.SetReplication(func() ReplicationStatus { return ReplicationStatus{State: ReplicationFresh} })
	}
	return n
}

func (n *node) set(name string, state consumers.State, readModel bool, lag float64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for i, c := range n.consumers {
		if c.Name == name {
			n.consumers = append(n.consumers[:i], n.consumers[i+1:]...)
			break
		}
	}
	n.consumers = append(n.consumers, consumers.Status{Name: name, ReadModel: readModel, State: state, LagSeconds: &lag})
}

func (n *node) status() []consumers.Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]consumers.Status(nil), n.consumers...)
}

func (n *node) at(d time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.clock = started.Add(d)
}

func (n *node) beginCatchUp() {
	n.t.Helper()
	err := n.health.BeginCatchUp(n.status, time.Minute, func(format string, args ...any) {
		n.mu.Lock()
		defer n.mu.Unlock()
		n.log = append(n.log, fmt.Sprintf(format, args...))
	})
	if err != nil {
		n.t.Fatal(err)
	}
	n.t.Cleanup(func() { n.health.SetLifecycle(Serving) })
}

// readyz is the status code, status and comma-joined reasons.
func (n *node) readyz() string {
	code, body := n.health.Readyz()
	return fmt.Sprintf("%d %s [%s]", code, body.Status, strings.Join(body.Reasons, ","))
}

func (n *node) logged(substr string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	count := 0
	for _, l := range n.log {
		if strings.Contains(l, substr) {
			count++
		}
	}
	return count
}

func expect(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("/readyz = %s, want %s", got, want)
	}
}

// --- Readiness: lifecycle ---

func TestBootingIsNotReadyStarting(t *testing.T) {
	expect(t, newNode(t, "writer").readyz(), "503 not_ready [starting]")
}

func TestCatchingUpIsNotReadyCatchingUp(t *testing.T) {
	for _, role := range []string{"writer", "reader"} {
		n := newNode(t, role)
		n.set("orders", consumers.Behind, true, 0)
		n.beginCatchUp()

		if n.health.Lifecycle() != CatchingUp {
			t.Errorf("%s: lifecycle %s", role, n.health.Lifecycle())
		}
		if got := n.readyz(); !strings.HasPrefix(got, "503 not_ready [catching_up") {
			t.Errorf("%s: /readyz = %s", role, got)
		}
	}
}

func TestServingWithEveryReadModelCurrentIsReadyWithNoReasons(t *testing.T) {
	n := newNode(t, "writer")
	n.set("orders", consumers.Behind, true, 0)
	n.beginCatchUp()

	n.set("orders", consumers.Current, true, 0)

	expect(t, n.readyz(), "200 ready []")
	if n.health.Lifecycle() != Serving || n.logged("readiness opened") != 1 {
		t.Errorf("lifecycle %s, log %v", n.health.Lifecycle(), n.log)
	}
}

func TestANodeWithNoReadModelsServesAsSoonAsBootCompletes(t *testing.T) {
	n := newNode(t, "writer")
	n.beginCatchUp()

	if n.health.Lifecycle() != Serving {
		t.Errorf("lifecycle %s", n.health.Lifecycle())
	}
}

func TestDrainingIsNotReadyDraining(t *testing.T) {
	for _, role := range []string{"writer", "reader"} {
		t.Run(role, func(t *testing.T) {
			n := newNode(t, role)
			n.beginCatchUp()
			if n.health.Lifecycle() != Serving {
				t.Fatalf("lifecycle %s before draining", n.health.Lifecycle())
			}

			n.health.BeginDraining(nil)

			if n.health.Lifecycle() != Draining {
				t.Errorf("lifecycle %s, want draining", n.health.Lifecycle())
			}
			expect(t, n.readyz(), "503 not_ready [draining]")
			if n.logged("draining") != 1 {
				t.Errorf("log %v, want one draining line", n.log)
			}
		})
	}
}

func TestANodeDrainingWhileCatchingUpNeverOpensReadiness(t *testing.T) {
	n := newNode(t, "writer")
	n.set("orders", consumers.Behind, true, 30)
	n.beginCatchUp()

	n.health.BeginDraining(nil)
	n.set("orders", consumers.Current, true, 0)
	n.at(10 * time.Minute) // past the catch-up deadline too

	if n.health.Lifecycle() != Draining {
		t.Errorf("lifecycle %s, want draining", n.health.Lifecycle())
	}
	expect(t, n.readyz(), "503 not_ready [draining]")
}

func TestDrainingIsIdempotentAndStaysDraining(t *testing.T) {
	n := newNode(t, "writer")
	n.beginCatchUp()

	n.health.BeginDraining(nil)
	n.health.BeginDraining(nil)

	if got := n.logged("draining"); got != 1 {
		t.Errorf("logged %d draining lines, want 1", got)
	}
	expect(t, n.readyz(), "503 not_ready [draining]")
}

func TestANodeStillBootingHasNothingToDrain(t *testing.T) {
	n := newNode(t, "writer")
	var lines []string

	n.health.BeginDraining(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })

	if n.health.Lifecycle() != Booting || len(lines) != 0 {
		t.Errorf("lifecycle %s, log %v; want booting and silent", n.health.Lifecycle(), lines)
	}
}

// --- Readiness: read_models ---

func TestAReadModelThatFallsBehindWhileServingReportsByRole(t *testing.T) {
	for _, tc := range []struct {
		role  string
		state consumers.State
		want  string
	}{
		{"writer", consumers.Behind, "200 degraded [projection_behind]"},
		{"reader", consumers.Behind, "503 not_ready [projection_behind]"},
		{"writer", consumers.Blocked, "200 degraded [projection_blocked]"},
		{"reader", consumers.Blocked, "503 not_ready [projection_blocked]"},
	} {
		n := newNode(t, tc.role)
		n.set("orders", consumers.Current, true, 0)
		n.beginCatchUp()

		n.set("orders", tc.state, true, 0)

		expect(t, n.readyz(), tc.want)
		// falling behind later is a condition, not a lifecycle step
		if n.health.Lifecycle() != Serving {
			t.Errorf("%s %s: lifecycle %s", tc.role, tc.state, n.health.Lifecycle())
		}
	}
}

func TestTheWorstReadModelDecidesBlockedOverBehind(t *testing.T) {
	n := newNode(t, "reader")
	n.set("orders", consumers.Current, true, 0)
	n.beginCatchUp()

	n.set("orders", consumers.Behind, true, 0)
	n.set("customers", consumers.Blocked, true, 0)

	expect(t, n.readyz(), "503 not_ready [projection_blocked]")
}

func TestConsumersThatAreNotReadModelsNeverAffectReadiness(t *testing.T) {
	n := newNode(t, "reader")
	n.set("orders", consumers.Current, true, 0)
	n.set("ship-reactor", consumers.Blocked, false, 999)
	n.beginCatchUp()

	expect(t, n.readyz(), "200 ready []")
	if _, body := n.health.Readyz(); body.Checks.ProjectionLagSeconds != 0 {
		t.Errorf("projection_lag_seconds = %v, want 0", body.Checks.ProjectionLagSeconds)
	}
}

func TestProjectionLagIsTheLargestAmongTheReadModels(t *testing.T) {
	n := newNode(t, "writer")
	n.set("orders", consumers.Current, true, 0.25)
	n.set("customers", consumers.Current, true, 1.5)
	n.beginCatchUp()

	_, body := n.health.Readyz()
	if body.Checks.ProjectionLagSeconds != 1.5 || body.Checks.WriteLagSeconds != 0 {
		t.Errorf("checks = %+v", body.Checks)
	}
}

// --- Machine 1: the catch-up deadline ---

func TestAWriterPastItsCatchUpDeadlineServesAnywayAndReportsDegraded(t *testing.T) {
	n := newNode(t, "writer")
	n.set("orders", consumers.Blocked, true, 0)
	n.beginCatchUp()

	n.at(59 * time.Second)
	if got := n.readyz(); !strings.HasPrefix(got, "503") {
		t.Errorf("before the deadline: %s", got)
	}

	n.at(61 * time.Second)
	expect(t, n.readyz(), "200 degraded [projection_blocked]")
	if n.health.Lifecycle() != Serving || n.logged("catch-up deadline reached; serving anyway") != 1 {
		t.Errorf("lifecycle %s, log %v", n.health.Lifecycle(), n.log)
	}
}

func TestAReaderPastItsCatchUpDeadlineKeepsCatchingUpUntilCurrent(t *testing.T) {
	n := newNode(t, "reader")
	n.set("orders", consumers.Blocked, true, 0)
	n.beginCatchUp()

	n.at(61 * time.Second)
	expect(t, n.readyz(), "503 not_ready [catching_up,projection_blocked]")
	n.readyz()
	if n.logged("catch-up deadline reached") != 1 {
		t.Errorf("want the deadline logged once, got %v", n.log)
	}

	n.set("orders", consumers.Current, true, 0)
	expect(t, n.readyz(), "200 ready []")
}

func TestBootCompletesOnlyOnce(t *testing.T) {
	n := newNode(t, "writer")
	n.beginCatchUp()

	if err := n.health.BeginCatchUp(n.status, time.Minute, nil); err == nil {
		t.Error("want an error the second time")
	}
}

// --- Over HTTP ---

func TestReadyzOnTheOpsPortIs503WhileBootingAnd200OnceServingInTheContractsShape(t *testing.T) {
	n := newNode(t, "writer")
	s := startOn(t, n.health)
	get := func() (int, string) {
		resp, err := http.Get(fmt.Sprintf("http://%s/readyz", s.Addr()))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var raw json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(raw)
	}

	code, raw := get()
	if code != http.StatusServiceUnavailable || !strings.Contains(raw, `"reasons":["starting"]`) {
		t.Errorf("booting: %d %s", code, raw)
	}

	n.beginCatchUp()
	code, raw = get()
	want := `{"status":"ready","role":"writer","node_id":"0192b5c4-7e1a-7c3e-9f00-5b2d8a1c4e77",` +
		`"contract_version":"1.0","reasons":[],` +
		`"checks":{"write_lag_seconds":0,"projection_lag_seconds":0,"dependencies":{}}}`
	if code != http.StatusOK || raw != want {
		t.Errorf("serving: %d\n got %s\nwant %s", code, raw, want)
	}
}
