package opsport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/events"
)

// The writer heartbeat and a reader's replication freshness (contract
// section 5, STATE-MACHINES.md machine 4 and "Readiness: replication").

var measuredAt = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

type fakeHeartbeats struct {
	mu      sync.Mutex
	row     *events.Heartbeat
	fail    bool
	written []string
}

func (f *fakeHeartbeats) WriteHeartbeat(_ context.Context, node, url string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, node+" "+url)
	return nil
}

func (f *fakeHeartbeats) ReadHeartbeat(context.Context) (*events.Heartbeat, error) {
	if f.fail {
		return nil, errors.New("store gone")
	}
	return f.row, nil
}

func (f *fakeHeartbeats) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.written)
}

func row(writtenAt time.Time, opsURL string) *events.Heartbeat {
	return &events.Heartbeat{WriterNodeID: "writer-1", WriterOpsURL: opsURL,
		WrittenAt: writtenAt.UTC().Format(events.HeartbeatTimeLayout), Sequence: 7}
}

// writer is a fake master /healthz answering status; asked counts the probes.
func writer(t *testing.T, status int) (url string, asked *int) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			n++
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

func monitor(store HeartbeatStore) *ReplicationMonitor {
	m := NewReplicationMonitor(store, &http.Client{Timeout: 2 * time.Second}, 5*time.Second)
	m.now = func() time.Time { return measuredAt }
	return m
}

func expectStatus(t *testing.T, got ReplicationStatus, state ReplicationState, lag float64) {
	t.Helper()
	if got.State != state || got.WriteLagSeconds != lag {
		t.Errorf("got %+v, want state %d lag %v", got, state, lag)
	}
}

func TestTheWriterHeartbeatsItsNodeIDAndOpsURLUntilCancelled(t *testing.T) {
	store := &fakeHeartbeats{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunWriterHeartbeat(ctx, store, "writer-1", "http://writer:10056", 10*time.Millisecond, nil)
		close(done)
	}()
	for i := 0; i < 200 && store.count() < 3; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if store.count() < 3 || store.written[0] != "writer-1 http://writer:10056" {
		t.Errorf("written %v", store.written)
	}
}

func TestNoHeartbeatRowIsUnknown(t *testing.T) {
	m := monitor(&fakeHeartbeats{})
	expectStatus(t, m.Current(), ReplicationUnknown, 0)
	expectStatus(t, m.Measure(context.Background()), ReplicationUnknown, 0)
}

func TestAnUnreadableHeartbeatIsUnknown(t *testing.T) {
	expectStatus(t, monitor(&fakeHeartbeats{fail: true}).Measure(context.Background()), ReplicationUnknown, 0)
}

func TestAHeartbeatWithinTheThresholdIsFreshAndTheWriterIsNotAsked(t *testing.T) {
	url, asked := writer(t, http.StatusOK)
	m := monitor(&fakeHeartbeats{row: row(measuredAt.Add(-2500*time.Millisecond), url)})

	expectStatus(t, m.Measure(context.Background()), ReplicationFresh, 2.5)
	if *asked != 0 {
		t.Errorf("asked the writer %d times", *asked)
	}
}

func TestAHeartbeatFromTheFutureCountsAsAgeZero(t *testing.T) {
	m := monitor(&fakeHeartbeats{row: row(measuredAt.Add(3*time.Second), "http://w")})
	expectStatus(t, m.Measure(context.Background()), ReplicationFresh, 0)
}

func TestStaleWhileTheWriterAnswersIsStaleWriterUp(t *testing.T) {
	url, asked := writer(t, http.StatusOK)
	m := monitor(&fakeHeartbeats{row: row(measuredAt.Add(-30*time.Second), url)})

	expectStatus(t, m.Measure(context.Background()), StaleWriterUp, 30)
	if *asked != 1 {
		t.Errorf("asked the writer's /healthz %d times, want 1", *asked)
	}
}

func TestStaleWhileTheWriterDoesNotAnswerIsStaleWriterDown(t *testing.T) {
	failing, _ := writer(t, http.StatusServiceUnavailable)
	for _, url := range []string{failing, "http://127.0.0.1:1"} {
		m := monitor(&fakeHeartbeats{row: row(measuredAt.Add(-30*time.Second), url)})
		if got := m.Measure(context.Background()); got.State != StaleWriterDown {
			t.Errorf("%s: got %+v, want stale writer down", url, got)
		}
	}
}

// --- Readiness: replication ---

func servingReader(t *testing.T, role string, status *ReplicationStatus) *node {
	n := newNode(t, role)
	if status != nil {
		s := *status
		n.health.SetReplication(func() ReplicationStatus { return s })
	} else {
		n.health.replication.Store(nil)
	}
	n.beginCatchUp()
	return n
}

func TestAReaderReportsItsReplicationState(t *testing.T) {
	for state, want := range map[ReplicationState]string{
		ReplicationFresh:   "200 ready []",
		StaleWriterUp:      "503 not_ready [replication_stale]",
		StaleWriterDown:    "200 degraded [replication_stale]",
		ReplicationUnknown: "503 not_ready [replication_unknown]",
	} {
		n := servingReader(t, "reader", &ReplicationStatus{State: state, WriteLagSeconds: 12.5})
		expect(t, n.readyz(), want)
		if _, body := n.health.Readyz(); body.Checks.WriteLagSeconds != 12.5 {
			t.Errorf("state %d: write_lag_seconds %v", state, body.Checks.WriteLagSeconds)
		}
	}
}

func TestAReaderWithNoMeasurementIsUnknown(t *testing.T) {
	expect(t, servingReader(t, "reader", nil).readyz(), "503 not_ready [replication_unknown]")
}

func TestAWriterHasNoReplicationAndZeroWriteLag(t *testing.T) {
	n := servingReader(t, "writer", &ReplicationStatus{State: StaleWriterUp, WriteLagSeconds: 99})
	expect(t, n.readyz(), "200 ready []")
	if _, body := n.health.Readyz(); body.Checks.WriteLagSeconds != 0 {
		t.Errorf("write_lag_seconds %v", body.Checks.WriteLagSeconds)
	}
}

// --- CQRS_OPS_URL ---

func TestTheOpsURLDefaultsToTheHostAndOpsPort(t *testing.T) {
	if got, err := AdvertisedURL("", "", "node-3", 10056); err != nil || got != "http://node-3:10056" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestTheOpsURLDefaultsToASpecificBindAddress(t *testing.T) {
	for bind, want := range map[string]string{
		"127.0.0.1": "http://127.0.0.1:10056", "::1": "http://[::1]:10056", "0.0.0.0": "http://node-3:10056",
	} {
		if got, err := AdvertisedURL("", bind, "node-3", 10056); err != nil || got != want {
			t.Errorf("bind %s: got %q, %v; want %s", bind, got, err, want)
		}
	}
}

func TestAConfiguredOpsURLIsUsedAsGiven(t *testing.T) {
	for configured, want := range map[string]string{
		"http://writer.internal:9000": "http://writer.internal:9000",
		"https://writer.example/ops/": "https://writer.example/ops",
	} {
		if got, err := AdvertisedURL(configured, "", "node-3", 10056); err != nil || got != want {
			t.Errorf("%s: got %q, %v", configured, got, err)
		}
	}
}

func TestAnInvalidOpsURLFailsTheBoot(t *testing.T) {
	for _, configured := range []string{"writer:10056", "ftp://writer", "/relative"} {
		_, err := AdvertisedURL(configured, "", "node-3", 10056)
		if err == nil || !strings.Contains(err.Error(), EnvURL) {
			t.Errorf("%s: %v", configured, err)
		}
	}
}
