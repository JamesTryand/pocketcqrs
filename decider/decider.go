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
	"sort"
	"sync"
	"time"

	"github.com/jamestryand/pocketcqrs/events"
)

// ErrUnknownAggregate is returned when no decider is registered for an aggregate.
var ErrUnknownAggregate = errors.New("decider: unknown aggregate")

// Command is an incoming intent: a name plus its JSON payload.
//
// Actor and Now mirror exactly what the JS decider binding already receives
// (command.actor, command.now) — populated by Register[S]'s closure from the
// meta map HandleWithMeta/DecideWithMeta thread through (meta["actor"],
// meta["now"]), before Decide is called. Both are the empty string when the
// caller supplied no meta (e.g. an anonymous command, or a bare Handle call
// with no HandleWithMeta wrapper) — a decider checking Actor for
// authorization should treat "" as "no actor", the same way an untyped/JS
// decider already must.
//
// Historically these reached a typed Decider[S]'s own Decide only via the
// lower-level RegisterUntyped path (used in this codebase exclusively for
// the JS adapter) — Register[S]'s generated closure discarded the meta
// parameter entirely. That made any authorization logic ("does this actor
// hold permission for this command") impossible to express in the
// documented, idiomatic Go decider pattern. See platform/pocketbase-cqrs-faas
// FAULTS-AND-WORK.md F-14 for the finding that surfaced this.
//
// Provenance is a separate question from Actor: Actor answers "who/what
// issued this command" (a user id, or "reactor:<name>" for reactor
// automation); Provenance answers "did the causal chain behind this command
// cross a trust boundary" (e.g. a peer deployment, once federation exists).
// It is empty for everything the gateway and local reactors produce today —
// only a trusted local write path is meant to ever set it — so a decider
// checking it for elevated trust is trusting whatever wrote that meta, not
// pattern-matching Actor's string convention. See
// platform/pocketbase-cqrs-faas NEEDS.md's federation trust model item for
// the full reasoning.
type Command struct {
	Name       string          `json:"name"`
	Payload    json.RawMessage `json:"payload"`
	Actor      string          `json:"actor,omitempty"`
	Now        string          `json:"now,omitempty"`
	Provenance string          `json:"provenance,omitempty"`
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
	// Commands optionally names the commands this decider accepts.
	//
	// It is documentation, not enforcement: Decide still adjudicates, and an
	// unlisted command is not rejected here. It exists because commands
	// leave NO trace in the log — events are recoverable empirically,
	// commands are not — so without a declaration the catalog cannot show
	// them, an export cannot reproduce them, and nothing can validate a
	// payload later.
	Commands []string
}

// Registry maps aggregate names to their deciders and executes commands.
// The map is mutex-guarded so deciders can be swapped (hot reload) while
// the gateway is serving commands.
type Registry struct {
	store    *events.Store
	mu       sync.RWMutex
	deciders map[string]erased
}

// erased is a type-erased Decider[S].
type erased struct {
	initial  func() any
	decide   func(Command, any, map[string]any) ([]events.NewEvent, error)
	evolve   func(any, events.Event) (any, error)
	commands []string
}

// Untyped is a type-erased decider contract, used by adapters whose state
// has no static Go type (e.g. JavaScript deciders).
type Untyped struct {
	Initial func() any
	Decide  func(cmd Command, state any, meta map[string]any) ([]events.NewEvent, error)
	Evolve  func(state any, ev events.Event) (any, error)
	// Commands optionally names the commands accepted — see Decider.Commands.
	Commands []string
}

// NewRegistry creates a Registry executing commands against store.
func NewRegistry(store *events.Store) *Registry {
	return &Registry{store: store, deciders: map[string]erased{}}
}

// Register adds a decider for an aggregate name.
func Register[S any](r *Registry, aggregate string, d *Decider[S]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deciders[aggregate] = erased{
		initial: func() any { return d.InitialState() },
		decide: func(cmd Command, state any, meta map[string]any) ([]events.NewEvent, error) {
			// Fill Command.Actor/Now from meta before Decide sees it — see
			// Command's own doc comment for why this exists (F-14). meta may
			// be nil (a bare Handle call with no HandleWithMeta wrapper);
			// the type assertions below leave Actor/Now as "" in that case,
			// same as an anonymous command.
			if actor, ok := meta["actor"].(string); ok {
				cmd.Actor = actor
			}
			if now, ok := meta["now"].(string); ok {
				cmd.Now = now
			}
			if provenance, ok := meta["provenance"].(string); ok {
				cmd.Provenance = provenance
			}
			return d.Decide(cmd, state.(S))
		},
		evolve: func(state any, ev events.Event) (any, error) {
			return d.Evolve(state.(S), ev)
		},
		commands: d.Commands,
	}
}

// RegisterUntyped adds an adapter-provided decider (e.g. a JavaScript one).
func (r *Registry) RegisterUntyped(aggregate string, d Untyped) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deciders[aggregate] = erased{
		initial: d.Initial, decide: d.Decide, evolve: d.Evolve, commands: d.Commands,
	}
}

// Unregister drops the decider for aggregate (no-op if absent): a JS
// decider whose file was removed stops serving commands (404).
func (r *Registry) Unregister(aggregate string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.deciders, aggregate)
}

// Has reports whether an aggregate is registered.
func (r *Registry) Has(aggregate string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.deciders[aggregate]
	return ok
}

// Aggregates returns the registered aggregate names, sorted.
func (r *Registry) Aggregates() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.deciders))
	for name := range r.deciders {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Commands returns the commands an aggregate declares, sorted, or nil when
// it declares none. Declaring is optional, so an empty result means "not
// stated" — never "accepts nothing".
func (r *Registry) Commands(aggregate string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.deciders[aggregate]
	if !ok || len(d.commands) == 0 {
		return nil
	}
	out := append([]string(nil), d.commands...)
	sort.Strings(out)
	return out
}

// CommandApplied reports whether a command has already produced events on a
// stream, by delegating to the event store.
//
// It is on the registry because the registry is what callers deciding commands
// already hold — a reactor dispatching a reaction has no reason to be handed
// the store as well, and asking it to be would put the check somewhere it can
// be forgotten.
func (r *Registry) CommandApplied(ctx context.Context, aggregate, aggregateID, commandID string, afterPosition int64) (bool, error) {
	return r.store.CommandApplied(ctx, aggregate, aggregateID, commandID, afterPosition)
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
//
// The registry stamps "now" (UTC, PocketBase timestamp format) into meta
// if absent, before deciding: deciders receive it as part of the command
// context, and it is recorded in the produced events — the time the
// decider saw is part of history.
func (r *Registry) HandleWithMeta(ctx context.Context, aggregate, id string, cmd Command, meta map[string]any) ([]events.Event, error) {
	newEvents, expectedSequence, err := r.DecideWithMeta(ctx, r.store, aggregate, id, cmd, meta)
	if err != nil {
		return nil, err
	}
	if len(newEvents) == 0 {
		return nil, nil
	}
	return r.store.Append(ctx, aggregate, id, expectedSequence, newEvents)
}

// StreamLoader loads a stream. *events.Store satisfies it (the ordinary
// case, via HandleWithMeta); a caller controlling its own append — the
// batching writer, deciding several commands before committing any of them
// — can supply one that also sees not-yet-durable events from earlier in
// the same batch, transparently to the decider itself.
type StreamLoader interface {
	LoadStream(ctx context.Context, aggregate, id string) ([]events.Event, error)
}

// DecideWithMeta is HandleWithMeta's fold-and-decide half, without the
// append: it loads the stream via loader (not necessarily the registry's
// own store), folds state, decides, merges meta onto every produced event's
// metadata exactly as HandleWithMeta does, and returns the events plus the
// expected sequence they were decided against — leaving the caller in
// control of when and how the append transaction happens.
//
// "now" is stamped into meta under the same rule HandleWithMeta documents:
// once at the top, if absent, before deciding — a caller that needs a
// decide replayed later to reproduce byte-identical events (see the
// batching writer's crash-recovery requirements) must capture and reuse
// meta itself; DecideWithMeta only ever fills in what's missing, never
// re-derives what's already there.
func (r *Registry) DecideWithMeta(ctx context.Context, loader StreamLoader, aggregate, id string, cmd Command, meta map[string]any) ([]events.NewEvent, int64, error) {
	r.mu.RLock()
	d, ok := r.deciders[aggregate]
	r.mu.RUnlock()
	if !ok {
		return nil, 0, fmt.Errorf("%w: %q", ErrUnknownAggregate, aggregate)
	}

	if meta == nil {
		meta = map[string]any{}
	}
	if _, ok := meta["now"]; !ok {
		meta["now"] = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	}

	stream, err := loader.LoadStream(ctx, aggregate, id)
	if err != nil {
		return nil, 0, err
	}

	state := d.initial()
	for _, ev := range stream {
		if state, err = d.evolve(state, ev); err != nil {
			return nil, 0, fmt.Errorf("decider: evolve %s/%s: %w", aggregate, id, err)
		}
	}

	newEvents, err := d.decide(cmd, state, meta)
	if err != nil {
		return nil, 0, err
	}

	for i := range newEvents {
		newEvents[i].Metadata = mergeMeta(newEvents[i].Metadata, meta)
	}

	return newEvents, int64(len(stream)), nil
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
