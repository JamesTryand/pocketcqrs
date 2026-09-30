// Package telemetry is the optional telemetry push (health/telemetry contract
// section 8): a best-effort snapshot of the node's cqrs_ series sent to a
// message bus, so a monitor on the bus sees a mixed estate without scraping
// every node. It is never a dependency: a snapshot the bus cannot take in time
// is dropped, and nothing here reaches /healthz or /readyz.
package telemetry

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Transport delivers a snapshot to a bus. The push builds the payload and
// decides when to send; a transport only delivers bytes, so NATS is one
// implementation and Kafka or RabbitMQ can be others without the push
// changing.
//
// The contract binds one logical shape, topic cqrs.telemetry.metrics with key
// <node_id>, to each transport (NATS: subject cqrs.telemetry.metrics.<node_id>;
// Kafka: that topic, keyed; RabbitMQ: a topic exchange of that name, routing
// key the node id), and the transport applies its own binding.
//
// Best-effort by contract: Publish must not queue what it cannot send. If the
// bus is unreachable it returns an error and the push drops that snapshot.
// Connecting and reconnecting happen in the background, never on the caller's
// time.
type Transport interface {
	// Publish delivers one snapshot for the node key (its node_id), or returns
	// an error, including when ctx ends first.
	Publish(ctx context.Context, key string, payload []byte) error
	Close() error
}

// Factory builds a transport for a CQRS_TELEMETRY_URL. logf hears connection
// events.
type Factory func(u *url.URL, logf func(string, ...any)) (Transport, error)

// Transports are the transports this binary can push through, by URL scheme.
// None is registered by default: main registers what it ships, so no
// transport is hard-wired into the push.
type Transports struct{ byScheme map[string]Factory }

// NewTransports is an empty registry.
func NewTransports() *Transports { return &Transports{byScheme: map[string]Factory{}} }

// Register adds a transport for a URL scheme.
func (t *Transports) Register(scheme string, f Factory) *Transports {
	t.byScheme[strings.ToLower(scheme)] = f
	return t
}

// Schemes lists the registered schemes, sorted.
func (t *Transports) Schemes() []string {
	out := make([]string, 0, len(t.byScheme))
	for s := range t.byScheme {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// Open builds the transport for u's scheme. A scheme this binary has no
// transport for is an *InvalidSettingError: a misconfiguration stops the node
// from starting, an unreachable bus does not.
func (t *Transports) Open(u *url.URL, logf func(string, ...any)) (Transport, error) {
	f, ok := t.byScheme[strings.ToLower(u.Scheme)]
	if !ok {
		available := "none"
		if s := t.Schemes(); len(s) > 0 {
			available = strings.Join(s, ", ")
		}
		return nil, &InvalidSettingError{Name: URLName, Problem: fmt.Sprintf(
			"the scheme %q has no telemetry transport in this binary (available: %s)", u.Scheme, available)}
	}
	return f(u, logf)
}
