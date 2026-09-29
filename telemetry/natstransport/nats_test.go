package natstransport_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/nodeidentity"
	"github.com/jamestryand/pocketcqrs/opsport"
	"github.com/jamestryand/pocketcqrs/telemetry"
	"github.com/jamestryand/pocketcqrs/telemetry/natstransport"
)

// The NATS binding of the telemetry push against a real nats-server: the subject is
// cqrs.telemetry.metrics.<node_id>, the payload is the snapshot, and a bus that goes away and
// comes back costs snapshots, not the node. Runs when POCKETCQRS_NATS_SERVER names a nats-server
// binary (build one from github.com/nats-io/nats-server); skipped otherwise.

const nodeID = "0192b5c4-7e1a-7c3e-9f00-5b2d8a1c4e77"

var started = time.Date(2026, 9, 29, 8, 30, 12, 345_000_000, time.UTC)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type server struct {
	t    *testing.T
	bin  string
	port int
	cmd  *exec.Cmd
}

func (s *server) start() {
	s.t.Helper()
	s.cmd = exec.Command(s.bin, "-a", "127.0.0.1", "-p", fmt.Sprint(s.port))
	if err := s.cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", s.port), 100*time.Millisecond); err == nil {
			c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.t.Fatal("nats-server did not start")
}

func (s *server) stop() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_, _ = s.cmd.Process.Wait()
		s.cmd = nil
	}
}

func TestASnapshotArrivesOnTheNodeIDsSubjectAndSurvivesABusRestart(t *testing.T) {
	bin := os.Getenv("POCKETCQRS_NATS_SERVER")
	if bin == "" {
		t.Skip("POCKETCQRS_NATS_SERVER does not name a nats-server binary")
	}
	srv := &server{t: t, bin: bin, port: freePort(t)}
	srv.start()
	t.Cleanup(srv.stop)
	busURL := fmt.Sprintf("nats://127.0.0.1:%d", srv.port)

	// a monitor on the bus, subscribed to every node's snapshots; it reconnects across the restart
	monitor, err := nats.Connect(busURL, nats.MaxReconnects(-1), nats.ReconnectWait(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer monitor.Close()
	var mu sync.Mutex
	var received []*nats.Msg
	if _, err := monitor.Subscribe("cqrs.telemetry.metrics.>", func(m *nats.Msg) {
		mu.Lock()
		received = append(received, m)
		mu.Unlock()
	}); err != nil {
		t.Fatal(err)
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(received) }
	waitFor := func(what string, seconds int, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(time.Duration(seconds) * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}

	h := opsport.New("node-3", started)
	h.SetIdentity(nodeidentity.Identity{NodeID: nodeID, Kind: nodeidentity.Persistent, Instance: "timesheets",
		Host: "node-3", Stack: nodeidentity.Stack, Role: "writer", StartedAt: started})
	if err := h.BeginCatchUp(func() []consumers.Status { return nil }, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	var logMu sync.Mutex
	var log []string
	logf := func(f string, a ...any) { logMu.Lock(); log = append(log, fmt.Sprintf(f, a...)); logMu.Unlock() }
	u, _ := url.Parse(busURL)
	transport, err := natstransport.Register(telemetry.NewTransports()).Open(u, logf)
	if err != nil {
		t.Fatal(err)
	}
	pub := telemetry.NewPublisher(h, transport, 100*time.Millisecond, logf)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pub.Run(ctx); close(done) }()

	// 1. it arrives, on the contract's subject, as the contract's payload
	waitFor("a snapshot", 20, func() bool { return count() > 0 })
	mu.Lock()
	first := received[0]
	mu.Unlock()
	if first.Subject != "cqrs.telemetry.metrics."+nodeID {
		t.Errorf("subject %q", first.Subject)
	}
	var body map[string]any
	if err := json.Unmarshal(first.Data, &body); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if body["contract_version"] != "1.0" || body["node_id"] != nodeID || len(body["series"].(map[string]any)) != 11 {
		t.Errorf("payload %s", first.Data)
	}

	// 2. the bus goes away: snapshots are dropped, and the node neither notices nor changes
	_, before := h.Readyz()
	srv.stop()
	waitFor("drops while the bus is down", 20, func() bool { return pub.Dropped() >= 3 })
	if _, during := h.Readyz(); during.Status != before.Status {
		t.Errorf("readiness moved from %s to %s while the bus was down", before.Status, during.Status)
	}
	droppedWhileDown := pub.Dropped()

	// 3. the bus comes back on the same address: publishing resumes on its own
	mu.Lock()
	received = nil
	mu.Unlock()
	srv.start()
	waitFor("publishing to resume", 40, func() bool { return count() > 0 })
	if droppedWhileDown == 0 || pub.Published() < 2 {
		t.Errorf("dropped %d, published %d", droppedWhileDown, pub.Published())
	}

	cancel()
	<-done
	_ = pub.Close()
	logMu.Lock()
	defer logMu.Unlock()
	if !strings.Contains(strings.Join(log, "\n"), "resumed") {
		t.Errorf("the log never said publishing resumed:\n%s", strings.Join(log, "\n"))
	}
}
