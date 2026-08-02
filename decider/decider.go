// Package decider implements the functional event-sourcing Decider pattern
// (see https://thinkbeforecoding.com/post/2021/12/17/functional-event-sourcing-decider):
// commands are decided against a folded stream state, producing the events
// that get appended to the event store.
package decider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"pocketcqrs/events"
)

// ErrUnknownAggregate is returned when no decider is registered for an aggregate.
var ErrUnknownAggregate = errors.New("decider: unknown aggregate")

// Command is an incoming intent: a name plus its JSON payload.
type Command struct {
	Name    string          `json:"name"`
	Payload json.RawMessage `json:"payload"`
}

// Decider is the write-side model of one aggregate type.
//
//	InitialState: the state before any event
//	Decide:       (command, state) -> new events (or a domain error)
//	Evolve:       (state, event)   -> new state
type Decider[S any] struct {
	InitialState func() S
	Decide       func(cmd Command, state S) ([]events.NewEvent, error)
	Evolve       func(state S, ev events.Event) (S, error)
}

// Registry maps aggregate names to their deciders and executes commands.
type Registry struct {
	store    *events.Store
	deciders map[string]erased
}

// erased is a type-erased Decider[S].
type erased struct {
	initial func() any
	decide  func(Command, any) ([]events.NewEvent, error)
	evolve  func(any, events.Event) (any, error)
}

// NewRegistry creates a Registry executing commands against store.
func NewRegistry(store *events.Store) *Registry {
	return &Registry{store: store, deciders: map[string]erased{}}
}

// Register adds a decider for an aggregate name.
func Register[S any](r *Registry, aggregate string, d *Decider[S]) {
	r.deciders[aggregate] = erased{
		initial: func() any { return d.InitialState() },
		decide: func(cmd Command, state any) ([]events.NewEvent, error) {
			return d.Decide(cmd, state.(S))
		},
		evolve: func(state any, ev events.Event) (any, error) {
			return d.Evolve(state.(S), ev)
		},
	}
}

// Has reports whether an aggregate is registered.
func (r *Registry) Has(aggregate string) bool {
	_, ok := r.deciders[aggregate]
	return ok
}

// Handle loads the stream, folds it into state, decides the command and
// appends the resulting events with optimistic concurrency.
//
// Domain errors from Decide are returned wrapped; events.ErrConcurrency is
// returned if the stream changed between load and append.
func (r *Registry) Handle(ctx context.Context, aggregate, id string, cmd Command) ([]events.Event, error) {
	return r.HandleWithMeta(ctx, aggregate, id, cmd, nil)
}

// HandleWithMeta is Handle with caller-supplied metadata (e.g. the
// authenticated actor) merged into every appended event's metadata.
// Caller-supplied keys win over decider-supplied ones.
func (r *Registry) HandleWithMeta(ctx context.Context, aggregate, id string, cmd Command, meta map[string]any) ([]events.Event, error) {
	d, ok := r.deciders[aggregate]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownAggregate, aggregate)
	}

	stream, err := r.store.LoadStream(ctx, aggregate, id)
	if err != nil {
		return nil, err
	}

	state := d.initial()
	for _, ev := range stream {
		if state, err = d.evolve(state, ev); err != nil {
			return nil, fmt.Errorf("decider: evolve %s/%s: %w", aggregate, id, err)
		}
	}

	newEvents, err := d.decide(cmd, state)
	if err != nil {
		return nil, err
	}
	if len(newEvents) == 0 {
		return nil, nil
	}

	if len(meta) > 0 {
		for i := range newEvents {
			newEvents[i].Metadata = mergeMeta(newEvents[i].Metadata, meta)
		}
	}

	return r.store.Append(ctx, aggregate, id, int64(len(stream)), newEvents)
}

// mergeMeta overlays extra onto the event's existing metadata (existing keys
// are kept unless also present in extra).
func mergeMeta(existing json.RawMessage, extra map[string]any) json.RawMessage {
	m := map[string]any{}
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &m)
	}
	for k, v := range extra {
		m[k] = v
	}
	out, _ := json.Marshal(m)
	return out
}
