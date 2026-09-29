package telemetry_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/nodeidentity"
	"github.com/jamestryand/pocketcqrs/opsport"
	"github.com/jamestryand/pocketcqrs/telemetry"
)

const nodeID = "0192b5c4-7e1a-7c3e-9f00-5b2d8a1c4e77"

var started = time.Date(2026, 9, 29, 8, 30, 12, 345_000_000, time.UTC)

// servingNode is a writer that has resolved its identity and finished boot.
func servingNode(t *testing.T, status ...consumers.Status) *opsport.Health {
	t.Helper()
	h := opsport.New("node-3", started)
	h.SetIdentity(nodeidentity.Identity{
		NodeID: nodeID, Kind: nodeidentity.Persistent, Instance: "timesheets", Host: "node-3",
		Stack: nodeidentity.Stack, Role: "writer", StartedAt: started,
	})
	if err := h.BeginCatchUp(func() []consumers.Status { return status }, time.Minute, nil); err != nil {
		t.Fatal(err)
	}
	return h
}

// fakeTransport records what it is sent; Fail and Hang make it misbehave.
type fakeTransport struct {
	mu     sync.Mutex
	sent   []sent
	fail   bool
	hang   bool
	closed bool
}

type sent struct {
	key     string
	payload []byte
}

func (f *fakeTransport) Publish(ctx context.Context, key string, payload []byte) error {
	f.mu.Lock()
	hang, fail := f.hang, f.fail
	f.mu.Unlock()
	if hang {
		<-ctx.Done()
		return ctx.Err()
	}
	if fail {
		return errors.New("bus is down")
	}
	f.mu.Lock()
	f.sent = append(f.sent, sent{key, append([]byte(nil), payload...)})
	f.mu.Unlock()
	return nil
}

func (f *fakeTransport) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *fakeTransport) set(fail, hang bool) {
	f.mu.Lock()
	f.fail, f.hang = fail, hang
	f.mu.Unlock()
}

func (f *fakeTransport) all() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sent(nil), f.sent...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) logf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *logs) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// run starts the publisher's loop and returns a stop that ends it and closes the publisher.
func run(p *telemetry.Publisher) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	return func() { cancel(); <-done; _ = p.Close() }
}

// --- Settings ---

func TestUnsetMeansOffWithTheDefaultInterval(t *testing.T) {
	s, err := telemetry.ParseSettings("", "  ")
	if err != nil || s.Enabled() || s.Interval != 15*time.Second {
		t.Errorf("settings %+v, err %v; want off, 15s", s, err)
	}
}

func TestAURLTurnsItOnAndTheIntervalIsSecondsWithDecimals(t *testing.T) {
	for in, want := range map[string]time.Duration{"0.5": 500 * time.Millisecond, "15": 15 * time.Second, "2s": 2 * time.Second} {
		s, err := telemetry.ParseSettings("nats://bus.example:4222", in)
		if err != nil || !s.Enabled() || s.URL.Scheme != "nats" || s.Interval != want {
			t.Errorf("interval %q: settings %+v, err %v; want %s", in, s, err, want)
		}
	}
}

func TestAnInvalidURLFailsTheBootWithoutEchoingIt(t *testing.T) {
	for _, in := range []string{"not a url", "amqp://", "://x"} {
		_, err := telemetry.ParseSettings(in, "")
		var bad *telemetry.InvalidSettingError
		if !errors.As(err, &bad) || bad.Name != telemetry.URLName || strings.Contains(err.Error(), in) {
			t.Errorf("url %q: err %v; want an InvalidSettingError for %s that does not quote it", in, err, telemetry.URLName)
		}
	}
}

func TestAnInvalidIntervalFailsTheBoot(t *testing.T) {
	for _, in := range []string{"0", "-5", "soon", "-1s"} {
		_, err := telemetry.ParseSettings("", in)
		var bad *telemetry.InvalidSettingError
		if !errors.As(err, &bad) || bad.Name != telemetry.IntervalName {
			t.Errorf("interval %q: err %v; want an InvalidSettingError for %s", in, err, telemetry.IntervalName)
		}
	}
}

func TestASchemeWithNoTransportFailsTheBootAndNeverLeaksCredentials(t *testing.T) {
	transports := telemetry.NewTransports().Register("nats", func(*url.URL, func(string, ...any)) (telemetry.Transport, error) {
		return &fakeTransport{}, nil
	})
	u, _ := url.Parse("kafka://user:s3cret@bus.example:9092")

	_, err := transports.Open(u, nil)

	var bad *telemetry.InvalidSettingError
	if !errors.As(err, &bad) || !strings.Contains(err.Error(), "kafka") || !strings.Contains(err.Error(), "nats") ||
		strings.Contains(err.Error(), "s3cret") {
		t.Errorf("err %v; want kafka unavailable, nats available, no credentials", err)
	}
}

func TestARegisteredSchemeBuildsItsTransportCaseInsensitively(t *testing.T) {
	made := &fakeTransport{}
	transports := telemetry.NewTransports().Register("NATS", func(*url.URL, func(string, ...any)) (telemetry.Transport, error) {
		return made, nil
	})
	u, _ := url.Parse("nats://bus:4222")
	got, err := transports.Open(u, nil)
	if err != nil || got != telemetry.Transport(made) {
		t.Errorf("got %v, %v", got, err)
	}
}

// --- Payload ---

func decode(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(payload, &v); err != nil {
		t.Fatalf("payload is not JSON: %v\n%s", err, payload)
	}
	return v
}

// jsonKeys is the top-level keys in the order they appear.
func jsonKeys(t *testing.T, payload []byte) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for dec.More() {
		k, _ := dec.Token()
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func TestThePayloadHasTheContractsEnvelopeAndEverySeries(t *testing.T) {
	lag := 0.0
	h := servingNode(t, consumers.Status{Name: "orders", ReadModel: true, State: consumers.Current, LagSeconds: &lag})
	snap := h.Metrics().Snapshot(context.Background(), h)

	payload := telemetry.Payload(snap, nodeID, "writer", time.Date(2026, 9, 29, 12, 0, 0, 123_000_000, time.UTC))

	if got := strings.Join(jsonKeys(t, payload), ","); got != "contract_version,node_id,role,sent_at,series" {
		t.Errorf("keys %s", got)
	}
	v := decode(t, payload)
	if v["contract_version"] != "1.0" || v["node_id"] != nodeID || v["role"] != "writer" || v["sent_at"] != "2026-09-29T12:00:00.123Z" {
		t.Errorf("envelope %v", v)
	}
	series := v["series"].(map[string]any)
	names := make([]string, 0, len(series))
	for name := range series {
		names = append(names, name)
	}
	sort.Strings(names)
	want := []string{"cqrs_command_duration_seconds", "cqrs_commands_total", "cqrs_consumer_lag", "cqrs_consumer_state",
		"cqrs_deadletter_depth", "cqrs_dependency_up", "cqrs_events_appended_total", "cqrs_node_info",
		"cqrs_projection_lag_seconds", "cqrs_readiness_status", "cqrs_write_lag_seconds"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("series %v, want %v", names, want)
	}
}

func TestThePayloadCarriesExactlyTheFiguresMetricsRenders(t *testing.T) {
	lag := 0.0
	h := servingNode(t, consumers.Status{Name: "orders", ReadModel: true, State: consumers.Current, LagSeconds: &lag})
	m := h.Metrics()
	m.RecordCommand(200, 3*time.Millisecond)
	m.RecordCommand(200, 80*time.Millisecond)
	m.RecordCommand(409, 2*time.Millisecond)
	m.EventAppended()
	m.SetDeadLetterDepth(func(context.Context) (int64, error) { return 2, nil }) // known: numeric in both forms
	snap := m.Snapshot(context.Background(), h)

	text := opsport.RenderSnapshot(snap)
	series := decode(t, telemetry.Payload(snap, nodeID, "writer", started))["series"].(map[string]any)

	// every plain sample in /metrics is in the JSON with the same labels and value
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "cqrs_command_duration_seconds") {
			continue
		}
		space := strings.LastIndexByte(line, ' ')
		head, value := line[:space], line[space+1:]
		name, labels := head, "{}"
		if i := strings.IndexByte(head, '{'); i >= 0 {
			name, labels = head[:i], head[i:]
		}
		want, _ := strconv.ParseFloat(value, 64) // "NaN" parses to NaN: not known, null in the payload
		found := false
		for _, e := range series[name].([]any) {
			entry := e.(map[string]any)
			got := map[string]string{}
			for k, v := range entry["labels"].(map[string]any) {
				got[k] = v.(string)
			}
			if !sameLabels(labels, got) {
				continue
			}
			if math.IsNaN(want) {
				found = entry["value"] == nil
			} else if v, ok := entry["value"].(float64); ok && v == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s%s = %s is in /metrics but not in the payload", name, labels, value)
		}
	}

	// histograms: cumulative buckets, sum and count for each outcome
	var accepted map[string]any
	entries := series["cqrs_command_duration_seconds"].([]any)
	for _, e := range entries {
		if e.(map[string]any)["labels"].(map[string]any)["status"] == "accepted" {
			accepted = e.(map[string]any)
		}
	}
	buckets := accepted["buckets"].(map[string]any)
	if len(entries) != 5 || accepted["count"].(float64) != 2 || buckets["+Inf"].(float64) != 2 || buckets["0.005"].(float64) != 1 ||
		buckets["0.1"].(float64) != 2 || len(buckets) != 15 || math.Abs(accepted["sum"].(float64)-0.083) > 1e-9 {
		t.Errorf("accepted histogram %v (of %d)", accepted, len(entries))
	}
}

// sameLabels compares the rendered `{a="1",b="2"}` with a label map.
func sameLabels(rendered string, got map[string]string) bool {
	rendered = strings.Trim(rendered, "{}")
	if rendered == "" {
		return len(got) == 0
	}
	want := map[string]string{}
	for _, kv := range strings.Split(rendered, ",") {
		k, v, _ := strings.Cut(kv, "=")
		want[k] = strings.Trim(v, `"`)
	}
	if len(want) != len(got) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func TestAValueThatIsNotKnownIsNullNotNaN(t *testing.T) {
	h := servingNode(t) // no dead-letter source: cqrs_deadletter_depth is NaN in /metrics
	snap := h.Metrics().Snapshot(context.Background(), h)
	if !strings.Contains(opsport.RenderSnapshot(snap), "cqrs_deadletter_depth NaN") {
		t.Fatal("expected NaN in /metrics")
	}

	series := decode(t, telemetry.Payload(snap, nodeID, "writer", started))["series"].(map[string]any)

	if v := series["cqrs_deadletter_depth"].([]any)[0].(map[string]any)["value"]; v != nil {
		t.Errorf("value %v, want null", v)
	}
}

// --- Publisher ---

func newPublisher(h *opsport.Health, f *fakeTransport, interval time.Duration, l *logs) *telemetry.Publisher {
	var logf func(string, ...any)
	if l != nil {
		logf = l.logf
	}
	return telemetry.NewPublisher(h, f, interval, logf)
}

func TestItPublishesAtOnceThenOnTheIntervalKeyedByTheNodeID(t *testing.T) {
	f := &fakeTransport{}
	p := newPublisher(servingNode(t), f, 40*time.Millisecond, nil)

	stop := run(p)
	waitFor(t, "four snapshots", func() bool { return len(f.all()) >= 4 })
	stop()

	for _, s := range f.all() {
		if s.key != nodeID {
			t.Errorf("key %q, want the node id", s.key)
		}
	}
	first := decode(t, f.all()[0].payload)["series"].(map[string]any)["cqrs_readiness_status"].([]any)
	if first[0].(map[string]any)["value"].(float64) != 1 { // ready
		t.Errorf("first snapshot readiness %v", first)
	}
	if !f.closed {
		t.Error("the transport was not closed")
	}
}

func TestItWaitsForAnIdentityBecauseTheKeyIsTheNodeID(t *testing.T) {
	f := &fakeTransport{}
	h := opsport.New("node-3", started) // booting: no identity yet
	p := newPublisher(h, f, 40*time.Millisecond, nil)

	stop := run(p)
	time.Sleep(400 * time.Millisecond)
	if n := len(f.all()); n != 0 {
		t.Fatalf("published %d snapshots before there was an identity", n)
	}
	h.SetIdentity(nodeidentity.Identity{NodeID: nodeID, Kind: nodeidentity.Persistent, Instance: "timesheets",
		Host: "node-3", Stack: nodeidentity.Stack, Role: "writer", StartedAt: started})

	waitFor(t, "a snapshot once identity resolved", func() bool { return len(f.all()) > 0 })
	stop()
}

func TestADeadBusDropsSnapshotsLogsOnceAndResumesWithoutABacklog(t *testing.T) {
	f := &fakeTransport{fail: true}
	l := &logs{}
	p := newPublisher(servingNode(t), f, 30*time.Millisecond, l)

	stop := run(p)
	waitFor(t, "eight drops", func() bool { return p.Dropped() >= 8 })
	if n := len(f.all()); n != 0 {
		t.Errorf("%d snapshots were queued while the bus was down", n)
	}
	if n := l.count("dropped"); n != 1 {
		t.Errorf("logged %d drop lines, want once per outage", n)
	}

	f.set(false, false)
	waitFor(t, "a snapshot after recovery", func() bool { return len(f.all()) > 0 })
	time.Sleep(200 * time.Millisecond)
	stop()

	// what arrived after recovery is what the ticks since then produced, not the 8+ dropped
	if n := len(f.all()); n < 1 || n > 12 {
		t.Errorf("%d snapshots after recovery; expected about the ticks since, not a backlog", n)
	}
	if n := l.count("resumed"); n != 1 {
		t.Errorf("logged %d resumed lines, want 1", n)
	}
}

func TestAHangingBusIsDroppedAfterTheTimeoutAndNeverBlocksTheNextSnapshot(t *testing.T) {
	f := &fakeTransport{hang: true}
	p := newPublisher(servingNode(t), f, 30*time.Millisecond, nil)
	p.PublishTimeout = 80 * time.Millisecond

	stop := run(p)
	waitFor(t, "three timed-out drops", func() bool { return p.Dropped() >= 3 })
	f.set(false, false)
	waitFor(t, "a snapshot once the bus answers", func() bool { return len(f.all()) > 0 })
	stop()
}

func TestAFailingBusChangesNeitherReadinessNorLiveness(t *testing.T) {
	f := &fakeTransport{fail: true}
	h := servingNode(t)
	beforeCode, before := h.Readyz()
	p := newPublisher(h, f, 20*time.Millisecond, nil)

	stop := run(p)
	waitFor(t, "five drops", func() bool { return p.Dropped() >= 5 })
	duringCode, during := h.Readyz()
	stop()

	if beforeCode != duringCode || before.Status != during.Status || strings.Join(before.Reasons, ",") != strings.Join(during.Reasons, ",") {
		t.Errorf("readiness moved: %d %s %v -> %d %s %v", beforeCode, before.Status, before.Reasons, duringCode, during.Status, during.Reasons)
	}
	for name := range during.Checks.Dependencies {
		if strings.Contains(strings.ToLower(name), "nats") || strings.Contains(strings.ToLower(name), "telemetry") {
			t.Errorf("the bus is listed as a dependency: %s", name)
		}
	}
}

func TestWhenDrainingBeginsOneMoreSnapshotGoesOutSayingSoBeforeCloseReturns(t *testing.T) {
	f := &fakeTransport{}
	h := servingNode(t)
	p := newPublisher(h, f, 5*time.Minute, nil) // only the first and the drain snapshot can arrive

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	waitFor(t, "the first snapshot", func() bool { return len(f.all()) == 1 })

	h.BeginDraining(nil)
	p.NotifyDraining()
	cancel()
	<-done
	_ = p.Close()

	all := f.all()
	if len(all) != 2 {
		t.Fatalf("%d snapshots, want 2 (first and drain)", len(all))
	}
	status := decode(t, all[1].payload)["series"].(map[string]any)["cqrs_readiness_status"].([]any)
	var on string
	for _, e := range status {
		entry := e.(map[string]any)
		if entry["value"].(float64) == 1 {
			on = entry["labels"].(map[string]any)["status"].(string)
		}
	}
	if on != "not_ready" {
		t.Errorf("the drain snapshot says %q, want not_ready", on)
	}
}

func TestStoppingDoesNotWaitForTheInterval(t *testing.T) {
	p := newPublisher(servingNode(t), &fakeTransport{}, 5*time.Minute, nil)
	stop := run(p)
	time.Sleep(100 * time.Millisecond)

	started := time.Now()
	stop()

	if time.Since(started) > 5*time.Second {
		t.Error("stopping waited for the interval")
	}
}
