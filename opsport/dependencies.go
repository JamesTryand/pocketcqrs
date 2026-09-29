package opsport

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// Required dependency names in /readyz's dependencies. The event store is
// this node's own; the others are shared (they fail every node at once).
const (
	DepEventStore = "event_store"
	DepWriter     = "writer"
)

// Defaults for --cqrsDependencyFailures and --cqrsDependencyCheckInterval.
const (
	DefaultDependencyFailures      = 3
	DefaultDependencyCheckInterval = 5 * time.Second
)

// Dependencies are a node's required dependencies (contract section 4.6,
// STATE-MACHINES.md machine 3, RequiredDependency): the event store; the
// master, on a secondary. Each is checked on a loop by a function returning an
// error when it is not usable. A dependency is up after a successful check and
// down after failuresToDown consecutive failures (or a failed first check).
// Run CheckAll once before boot completes, so no dependency is left unchecked
// once the node serves. NATS is never a required dependency. Safe for
// concurrent use.
type Dependencies struct {
	failuresToDown int
	logf           func(string, ...any)

	mu     sync.Mutex
	checks []namedCheck
	states map[string]depState
}

type namedCheck struct {
	name  string
	check func(context.Context) error
}

type depState struct {
	up       bool
	failures int
}

// DependencyState is one dependency's last known state.
type DependencyState struct {
	Name string
	Up   bool
}

// NewDependencies starts with nothing checked. logf (may be nil) hears each
// dependency going down and coming back.
func NewDependencies(failuresToDown int, logf func(string, ...any)) *Dependencies {
	if failuresToDown < 1 {
		failuresToDown = 1
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Dependencies{failuresToDown: failuresToDown, logf: logf, states: map[string]depState{}}
}

// Add registers a dependency and how to check it.
func (d *Dependencies) Add(name string, check func(context.Context) error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.checks = append(d.checks, namedCheck{name, check})
}

// States is every dependency checked so far: the event store first, then by
// name.
func (d *Dependencies) States() []DependencyState {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]DependencyState, 0, len(d.states))
	for name, s := range d.states {
		out = append(out, DependencyState{name, s.up})
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Name == DepEventStore) != (out[j].Name == DepEventStore) {
			return out[i].Name == DepEventStore
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// CheckAll checks every dependency once and records the results.
func (d *Dependencies) CheckAll(ctx context.Context) {
	d.mu.Lock()
	checks := append([]namedCheck(nil), d.checks...)
	d.mu.Unlock()
	for _, c := range checks {
		err := c.check(ctx)
		if err != nil && errors.Is(ctx.Err(), context.Canceled) {
			return
		}
		d.Record(c.name, err == nil)
	}
}

// Run checks every interval until ctx is done.
func (d *Dependencies) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		d.CheckAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Record applies one check's outcome (machine 3).
func (d *Dependencies) Record(name string, ok bool) {
	d.mu.Lock()
	s, known := d.states[name]
	var message string
	if ok {
		if known && !s.up {
			message = "dependency " + name + ": up again"
		}
		d.states[name] = depState{up: true}
	} else {
		failures := s.failures + 1
		down := !known || !s.up || failures >= d.failuresToDown
		if down && (!known || s.up) {
			message = "dependency " + name + ": down"
		}
		d.states[name] = depState{up: !down, failures: failures}
	}
	d.mu.Unlock()
	if message != "" {
		d.logf("%s", message)
	}
}
