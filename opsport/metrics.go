package opsport

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jamestryand/pocketcqrs/nodeidentity"
)

// Outcomes are the command-outcome labels (contract section 7), in the
// contract's order.
var Outcomes = []string{"accepted", "rejected", "conflict", "unavailable", "error"}

// DurationBuckets are the fixed cqrs_command_duration_seconds bucket
// boundaries (contract section 6.4).
var DurationBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}

var (
	readinessStatuses = []string{"ready", "degraded", "not_ready"}
	consumerStates    = []string{"current", "behind", "blocked"}
)

// OutcomeOf is the status label for a gateway response (contract section 7):
// 2xx accepted; 409 conflict; 503 unavailable; 500, 502 and 504 error; 400,
// 401, 403, 404, 410 and 422 rejected. Any other 4xx counts as rejected and any
// other 5xx as error.
func OutcomeOf(httpStatus int) string {
	switch {
	case httpStatus >= 200 && httpStatus < 300:
		return "accepted"
	case httpStatus == 409:
		return "conflict"
	case httpStatus == 503:
		return "unavailable"
	case httpStatus >= 400 && httpStatus < 500:
		return "rejected"
	default:
		return "error"
	}
}

// Metrics is the GET /metrics series (contract sections 6 and 7), identical
// to dotnetcqrs's, in the Prometheus text exposition format. Health owns one,
// so it exists from process start and every series is present from the first
// scrape; counters start at zero for every status label and reset on restart.
//
// Label values are bounded by names known at boot (consumers, read models,
// dependencies) or by the contract's enumerations, never by an aggregate or
// stream id. Families whose label values are not known yet (consumers, while
// booting) are listed with no samples. Safe for concurrent use.
type Metrics struct {
	mu             sync.Mutex
	commands       map[string]*histogram
	eventsAppended atomic.Int64
	deadLetters    atomic.Pointer[func(context.Context) (int64, error)]
}

type histogram struct {
	count   int64
	sum     float64
	buckets []int64
}

func newMetrics() *Metrics {
	m := &Metrics{commands: map[string]*histogram{}}
	for _, o := range Outcomes {
		m.commands[o] = &histogram{buckets: make([]int64, len(DurationBuckets))}
	}
	return m
}

// RecordCommand counts and times one command this node decided, from receipt
// to response. A command a secondary forwards is recorded only by the master
// that decides it.
func (m *Metrics) RecordCommand(httpStatus int, d time.Duration) {
	seconds := d.Seconds()
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.commands[OutcomeOf(httpStatus)]
	h.count++
	h.sum += seconds
	for i, le := range DurationBuckets {
		if seconds <= le {
			h.buckets[i]++
		}
	}
}

// EventAppended counts one event appended by this node.
func (m *Metrics) EventAppended() { m.eventsAppended.Add(1) }

// SetDeadLetterDepth sets where cqrs_deadletter_depth comes from: a read of
// the unresolved dead letters. Until it is set, or when it fails, the gauge is
// NaN (unknown).
func (m *Metrics) SetDeadLetterDepth(depth func(context.Context) (int64, error)) {
	m.deadLetters.Store(&depth)
}

// Render is the /metrics body. h supplies identity, readiness and the
// consumers; nothing here writes anything.
func (m *Metrics) Render(ctx context.Context, h *Health) string {
	var b strings.Builder
	id := h.Identity()
	_, readyz := h.Readyz()
	consumers := h.Consumers()

	family(&b, "cqrs_node_info", "gauge", "Node identity (node-identity contract); always 1.")
	var nodeID, instance, role string
	if id != nil {
		nodeID, instance, role = id.NodeID, id.Instance, id.Role
	}
	sample(&b, "cqrs_node_info", 1, "node_id", nodeID, "instance", instance, "host", h.host,
		"role", role, "stack", nodeidentity.Stack, "contract_version", ContractVersion)

	family(&b, "cqrs_readiness_status", "gauge", "Current /readyz status, one-hot.")
	for _, s := range readinessStatuses {
		sample(&b, "cqrs_readiness_status", oneHot(s == readyz.Status), "status", s)
	}

	m.mu.Lock()
	commands := map[string]histogram{}
	for o, hist := range m.commands {
		commands[o] = histogram{count: hist.count, sum: hist.sum, buckets: append([]int64(nil), hist.buckets...)}
	}
	m.mu.Unlock()

	family(&b, "cqrs_commands_total", "counter", "Commands decided on this node, by outcome.")
	for _, o := range Outcomes {
		sample(&b, "cqrs_commands_total", float64(commands[o].count), "status", o)
	}
	family(&b, "cqrs_command_duration_seconds", "histogram", "Command receipt to response, on the node that decides.")
	for _, o := range Outcomes {
		hist := commands[o]
		for i, le := range DurationBuckets {
			sample(&b, "cqrs_command_duration_seconds_bucket", float64(hist.buckets[i]), "status", o, "le", number(le))
		}
		sample(&b, "cqrs_command_duration_seconds_bucket", float64(hist.count), "status", o, "le", "+Inf")
		sample(&b, "cqrs_command_duration_seconds_sum", hist.sum, "status", o)
		sample(&b, "cqrs_command_duration_seconds_count", float64(hist.count), "status", o)
	}

	family(&b, "cqrs_events_appended_total", "counter", "Events appended by this node.")
	sample(&b, "cqrs_events_appended_total", float64(m.eventsAppended.Load()))

	family(&b, "cqrs_write_lag_seconds", "gauge", "Age of the writer heartbeat this node sees; 0 on a writer.")
	sample(&b, "cqrs_write_lag_seconds", readyz.Checks.WriteLagSeconds)

	family(&b, "cqrs_projection_lag_seconds", "gauge", "Age of the oldest event each read model has not applied.")
	for _, c := range consumers {
		if c.ReadModel {
			sample(&b, "cqrs_projection_lag_seconds", orNaN(c.LagSeconds), "read_model", c.Name)
		}
	}
	family(&b, "cqrs_consumer_lag", "gauge", "Positions each consumer is behind the log head.")
	for _, c := range consumers {
		lag := math.NaN()
		if c.LagPositions != nil {
			lag = float64(*c.LagPositions)
		}
		sample(&b, "cqrs_consumer_lag", lag, "consumer", c.Name)
	}
	family(&b, "cqrs_consumer_state", "gauge", "Each consumer's state, one-hot.")
	for _, c := range consumers {
		for _, s := range consumerStates {
			sample(&b, "cqrs_consumer_state", oneHot(s == c.State.String()), "consumer", c.Name, "state", s)
		}
	}

	family(&b, "cqrs_dependency_up", "gauge", "Each required dependency: 1 up, 0 down.")
	names := make([]string, 0, len(readyz.Checks.Dependencies))
	for name := range readyz.Checks.Dependencies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sample(&b, "cqrs_dependency_up", oneHot(readyz.Checks.Dependencies[name] == "up"), "dependency", name)
	}

	family(&b, "cqrs_deadletter_depth", "gauge", "Unresolved dead letters.")
	depth := math.NaN()
	if f := m.deadLetters.Load(); f != nil {
		if n, err := (*f)(ctx); err == nil {
			depth = float64(n)
		}
	}
	sample(&b, "cqrs_deadletter_depth", depth)
	return b.String()
}

func family(b *strings.Builder, name, kind, help string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

// sample writes one line; labels alternate name, value.
func sample(b *strings.Builder, name string, value float64, labels ...string) {
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteByte('{')
		for i := 0; i < len(labels); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(labels[i])
			b.WriteString(`="`)
			b.WriteString(escape(labels[i+1]))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(number(value))
	b.WriteByte('\n')
}

func number(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func oneHot(on bool) float64 {
	if on {
		return 1
	}
	return 0
}

func orNaN(v *float64) float64 {
	if v == nil {
		return math.NaN()
	}
	return *v
}
