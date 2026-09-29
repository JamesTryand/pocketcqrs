package opsport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/consumers"
)

// GET /metrics (contract sections 6 and 7): every cqrs_ series present from
// the first scrape, counters zero-initialised for every outcome, the fixed
// buckets.

var families = []string{
	"cqrs_node_info", "cqrs_readiness_status", "cqrs_commands_total", "cqrs_command_duration_seconds",
	"cqrs_events_appended_total", "cqrs_write_lag_seconds", "cqrs_projection_lag_seconds", "cqrs_consumer_lag",
	"cqrs_consumer_state", "cqrs_dependency_up", "cqrs_deadletter_depth",
}

// value is the value of the one sample line series (name plus labels,
// exactly).
func value(t *testing.T, body, series string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, series+" ") {
			found = append(found, strings.TrimPrefix(line, series+" "))
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: %d samples, want 1", series, len(found))
	}
	return found[0]
}

func expectValue(t *testing.T, body, series, want string) {
	t.Helper()
	if got := value(t, body, series); got != want {
		t.Errorf("%s = %s, want %s", series, got, want)
	}
}

func TestWhileBootingEveryFamilyIsPresentAndTheCountersAreZeroForEveryOutcome(t *testing.T) {
	h := New("node-3", started)

	body := h.Metrics().Render(context.Background(), h)

	for _, f := range families {
		if !strings.Contains(body, "# TYPE "+f+" ") {
			t.Errorf("family %s missing", f)
		}
	}
	for _, o := range Outcomes {
		expectValue(t, body, fmt.Sprintf(`cqrs_commands_total{status="%s"}`, o), "0")
		expectValue(t, body, fmt.Sprintf(`cqrs_command_duration_seconds_count{status="%s"}`, o), "0")
	}
	expectValue(t, body, "cqrs_events_appended_total", "0")
	expectValue(t, body, `cqrs_readiness_status{status="not_ready"}`, "1")
	expectValue(t, body, `cqrs_readiness_status{status="ready"}`, "0")
	expectValue(t, body,
		`cqrs_node_info{node_id="",instance="",host="node-3",role="",stack="pocketcqrs",contract_version="1.0"}`, "1")
	expectValue(t, body, "cqrs_deadletter_depth", "NaN")
}

func TestTheDurationBucketsAreTheContractsFixedBoundaries(t *testing.T) {
	h := New("node-3", started)
	body := h.Metrics().Render(context.Background(), h)

	var got []string
	for _, m := range regexp.MustCompile(`cqrs_command_duration_seconds_bucket\{status="accepted",le="([^"]+)"\}`).FindAllStringSubmatch(body, -1) {
		got = append(got, m[1])
	}
	want := "0.001 0.0025 0.005 0.01 0.025 0.05 0.1 0.25 0.5 1 2.5 5 10 30 +Inf"
	if strings.Join(got, " ") != want {
		t.Errorf("buckets %v, want %s", got, want)
	}
}

func TestOutcomesFollowTheContractsStatusTable(t *testing.T) {
	for status, want := range map[int]string{
		200: "accepted", 400: "rejected", 401: "rejected", 403: "rejected", 404: "rejected", 410: "rejected",
		422: "rejected", 409: "conflict", 503: "unavailable", 500: "error", 502: "error", 504: "error",
	} {
		if got := OutcomeOf(status); got != want {
			t.Errorf("OutcomeOf(%d) = %s, want %s", status, got, want)
		}
	}
}

func TestARecordedCommandCountsOnceAndFillsTheCumulativeBucketsItFits(t *testing.T) {
	h := New("node-3", started)
	h.Metrics().RecordCommand(200, 30*time.Millisecond)
	h.Metrics().RecordCommand(409, 2*time.Millisecond)

	body := h.Metrics().Render(context.Background(), h)

	expectValue(t, body, `cqrs_commands_total{status="accepted"}`, "1")
	expectValue(t, body, `cqrs_commands_total{status="conflict"}`, "1")
	expectValue(t, body, `cqrs_command_duration_seconds_bucket{status="accepted",le="0.025"}`, "0")
	expectValue(t, body, `cqrs_command_duration_seconds_bucket{status="accepted",le="0.05"}`, "1")
	expectValue(t, body, `cqrs_command_duration_seconds_bucket{status="accepted",le="+Inf"}`, "1")
	expectValue(t, body, `cqrs_command_duration_seconds_sum{status="accepted"}`, "0.03")
}

func TestOnceServingTheIdentityReadinessAndEveryConsumerAreReported(t *testing.T) {
	n := newNode(t, "writer")
	m := n.health.Metrics()
	m.EventAppended()
	m.SetDeadLetterDepth(func(context.Context) (int64, error) { return 2, nil })
	n.set("orders", consumers.Current, true, 0.25)
	n.set("ship", consumers.Blocked, false, 90)
	lag := int64(4)
	n.mu.Lock()
	n.consumers[1].LagPositions = &lag
	n.mu.Unlock()
	n.beginCatchUp()

	body := m.Render(context.Background(), n.health)

	expectValue(t, body, `cqrs_node_info{node_id="0192b5c4-7e1a-7c3e-9f00-5b2d8a1c4e77",instance="timesheets",host="node-3",role="writer",stack="pocketcqrs",contract_version="1.0"}`, "1")
	expectValue(t, body, `cqrs_readiness_status{status="ready"}`, "1")
	expectValue(t, body, "cqrs_events_appended_total", "1")
	expectValue(t, body, "cqrs_deadletter_depth", "2")
	expectValue(t, body, "cqrs_write_lag_seconds", "0")
	// only read models have a projection lag; every consumer has a lag and a state
	expectValue(t, body, `cqrs_projection_lag_seconds{read_model="orders"}`, "0.25")
	if strings.Contains(body, `read_model="ship"`) {
		t.Error("a reactor has a projection lag")
	}
	expectValue(t, body, `cqrs_consumer_lag{consumer="ship"}`, "4")
	expectValue(t, body, `cqrs_consumer_lag{consumer="orders"}`, "NaN")
	expectValue(t, body, `cqrs_consumer_state{consumer="ship",state="blocked"}`, "1")
	expectValue(t, body, `cqrs_consumer_state{consumer="ship",state="current"}`, "0")
	expectValue(t, body, `cqrs_consumer_state{consumer="orders",state="current"}`, "1")
}

func TestAFailingDeadLetterReadIsNaN(t *testing.T) {
	h := New("node-3", started)
	h.Metrics().SetDeadLetterDepth(func(context.Context) (int64, error) { return 0, errors.New("store gone") })

	expectValue(t, h.Metrics().Render(context.Background(), h), "cqrs_deadletter_depth", "NaN")
}

func TestMetricsOnTheOpsPortIsPrometheusText(t *testing.T) {
	s := startOn(t, New("node-3", started))

	resp, err := http.Get(fmt.Sprintf("http://%s/metrics", s.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("%d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(string(body), "# TYPE cqrs_commands_total counter") {
		t.Errorf("body:\n%s", body)
	}
}
