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

// Label is one label of a sample.
type Label struct{ Name, Value string }

// Sample is one gauge or counter value with its labels in order; NaN means
// not known.
type Sample struct {
	Labels []Label
	Value  float64
}

// HistogramSample is one label set of a histogram family: cumulative counts
// per DurationBuckets boundary, then the total count and the sum.
type HistogramSample struct {
	Labels  []Label
	Buckets []int64
	Count   int64
	Sum     float64
}

// Family is one series of the contract's section 6: gauges and counters
// carry Samples, a histogram carries Histograms.
type Family struct {
	Name, Type, Help string
	Samples          []Sample
	Histograms       []HistogramSample
}

// Snapshot is every section-6 series at one moment, in the contract's order:
// what /metrics renders as text and the telemetry push serialises as JSON, so
// the two cannot disagree.
type Snapshot struct{ Families []Family }

// Render is the /metrics body. h supplies identity, readiness and the
// consumers; nothing here writes anything.
func (m *Metrics) Render(ctx context.Context, h *Health) string {
	return RenderSnapshot(m.Snapshot(ctx, h))
}

// Snapshot reads every section-6 series. h supplies identity, readiness and
// the consumers; nothing here writes anything.
func (m *Metrics) Snapshot(ctx context.Context, h *Health) Snapshot {
	id := h.Identity()
	_, readyz := h.Readyz()
	consumers := h.Consumers()
	var families []Family
	s := func(value float64, labels ...Label) Sample { return Sample{Labels: labels, Value: value} }

	var nodeID, instance, role string
	if id != nil {
		nodeID, instance, role = id.NodeID, id.Instance, id.Role
	}
	families = append(families, Family{Name: "cqrs_node_info", Type: "gauge",
		Help: "Node identity (node-identity contract); always 1.",
		Samples: []Sample{s(1, Label{"node_id", nodeID}, Label{"instance", instance}, Label{"host", h.host},
			Label{"role", role}, Label{"stack", nodeidentity.Stack}, Label{"contract_version", ContractVersion})}})

	ready := Family{Name: "cqrs_readiness_status", Type: "gauge", Help: "Current /readyz status, one-hot."}
	for _, status := range readinessStatuses {
		ready.Samples = append(ready.Samples, s(oneHot(status == readyz.Status), Label{"status", status}))
	}
	families = append(families, ready)

	m.mu.Lock()
	commands := map[string]histogram{}
	for o, hist := range m.commands {
		commands[o] = histogram{count: hist.count, sum: hist.sum, buckets: append([]int64(nil), hist.buckets...)}
	}
	m.mu.Unlock()

	total := Family{Name: "cqrs_commands_total", Type: "counter", Help: "Commands decided on this node, by outcome."}
	duration := Family{Name: "cqrs_command_duration_seconds", Type: "histogram",
		Help: "Command receipt to response, on the node that decides."}
	for _, o := range Outcomes {
		total.Samples = append(total.Samples, s(float64(commands[o].count), Label{"status", o}))
		duration.Histograms = append(duration.Histograms, HistogramSample{Labels: []Label{{"status", o}},
			Buckets: commands[o].buckets, Count: commands[o].count, Sum: commands[o].sum})
	}
	families = append(families, total, duration)

	families = append(families,
		Family{Name: "cqrs_events_appended_total", Type: "counter", Help: "Events appended by this node.",
			Samples: []Sample{s(float64(m.eventsAppended.Load()))}},
		Family{Name: "cqrs_write_lag_seconds", Type: "gauge",
			Help:    "Age of the writer heartbeat this node sees; 0 on a writer.",
			Samples: []Sample{s(readyz.Checks.WriteLagSeconds)}})

	projection := Family{Name: "cqrs_projection_lag_seconds", Type: "gauge",
		Help: "Age of the oldest event each read model has not applied."}
	lag := Family{Name: "cqrs_consumer_lag", Type: "gauge", Help: "Positions each consumer is behind the log head."}
	state := Family{Name: "cqrs_consumer_state", Type: "gauge", Help: "Each consumer's state, one-hot."}
	for _, c := range consumers {
		if c.ReadModel {
			projection.Samples = append(projection.Samples, s(orNaN(c.LagSeconds), Label{"read_model", c.Name}))
		}
	}
	for _, c := range consumers {
		l := math.NaN()
		if c.LagPositions != nil {
			l = float64(*c.LagPositions)
		}
		lag.Samples = append(lag.Samples, s(l, Label{"consumer", c.Name}))
	}
	for _, c := range consumers {
		for _, st := range consumerStates {
			state.Samples = append(state.Samples, s(oneHot(st == c.State.String()), Label{"consumer", c.Name}, Label{"state", st}))
		}
	}
	families = append(families, projection, lag, state)

	dependency := Family{Name: "cqrs_dependency_up", Type: "gauge", Help: "Each required dependency: 1 up, 0 down."}
	names := make([]string, 0, len(readyz.Checks.Dependencies))
	for name := range readyz.Checks.Dependencies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		dependency.Samples = append(dependency.Samples, s(oneHot(readyz.Checks.Dependencies[name] == "up"), Label{"dependency", name}))
	}
	families = append(families, dependency)

	depth := math.NaN()
	if f := m.deadLetters.Load(); f != nil {
		if n, err := (*f)(ctx); err == nil {
			depth = float64(n)
		}
	}
	families = append(families, Family{Name: "cqrs_deadletter_depth", Type: "gauge", Help: "Unresolved dead letters.",
		Samples: []Sample{s(depth)}})
	return Snapshot{Families: families}
}

// RenderSnapshot is the Prometheus text exposition of a snapshot.
func RenderSnapshot(snap Snapshot) string {
	var b strings.Builder
	for _, f := range snap.Families {
		family(&b, f.Name, f.Type, f.Help)
		for _, smp := range f.Samples {
			sample(&b, f.Name, smp.Value, flatten(smp.Labels)...)
		}
		for _, h := range f.Histograms {
			base := flatten(h.Labels)
			for i, le := range DurationBuckets {
				sample(&b, f.Name+"_bucket", float64(h.Buckets[i]), append(append([]string(nil), base...), "le", Number(le))...)
			}
			sample(&b, f.Name+"_bucket", float64(h.Count), append(append([]string(nil), base...), "le", "+Inf")...)
			sample(&b, f.Name+"_sum", h.Sum, base...)
			sample(&b, f.Name+"_count", float64(h.Count), base...)
		}
	}
	return b.String()
}

func flatten(labels []Label) []string {
	out := make([]string, 0, 2*len(labels))
	for _, l := range labels {
		out = append(out, l.Name, l.Value)
	}
	return out
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
	b.WriteString(Number(value))
	b.WriteByte('\n')
}

// Number is a number as Prometheus text and the push's bucket keys write it.
func Number(v float64) string {
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
