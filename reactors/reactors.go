// Package reactors implements cross-aggregate reactions (sagas/process
// managers): durable consumers that map committed events to new commands,
// dispatched in-process through the decider registry — so reactions become
// events like everything else, never out-of-band state changes.
//
// Delivery is at-least-once, so reactions must be idempotent. The standard
// pattern is a deterministic target aggregate id derived from the source
// event (e.g. "fulfill-<orderId>"): replays then hit a domain rejection
// ("already exists"), which the reactor logs and skips.
package reactors

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

// Reaction is a command to dispatch in response to an event.
type Reaction struct {
	Aggregate string
	ID        string
	Command   decider.Command
}

// Reactor maps events to reactions.
type Reactor interface {
	// Name is the durable checkpoint key.
	Name() string
	// React maps one committed event to zero or more reactions.
	React(ev events.Event) []Reaction
}

// consumer adapts a Reactor to the consumers engine.
type consumer struct {
	reactor  Reactor
	registry *decider.Registry
	logger   func(msg string, args ...any)
	warn     func(msg string, args ...any)
}

// AsConsumer wraps r as a checkpointed consumer that dispatches its
// reactions through registry. warn may be nil (falls back to logger); it
// carries the permanent-fault level — see Dispatch.
func AsConsumer(r Reactor, registry *decider.Registry, logger, warn func(string, ...any)) consumers.Consumer {
	if logger == nil {
		logger = func(string, ...any) {}
	}
	return &consumer{reactor: r, registry: registry, logger: logger, warn: warn}
}

// Name implements consumers.Consumer (checkpointed as "reactor:<name>").
func (c *consumer) Name() string { return "reactor:" + c.reactor.Name() }

// Apply implements consumers.Consumer.
func (c *consumer) Apply(ctx context.Context, ev events.Event) error {
	return Dispatch(ctx, c.registry, c.reactor.Name(), ev, c.reactor.React(ev), c.logger, c.warn)
}

// Dispatcher is the slice of *decider.Registry that Dispatch needs. It is an
// interface so the retry-vs-continue split below can be tested directly:
// ErrConcurrency is genuinely hard to provoke against a real registry (it
// needs a race between load and append), and that half of the rule is
// exactly the half worth pinning.
type Dispatcher interface {
	HandleWithMeta(ctx context.Context, aggregate, id string, cmd decider.Command, meta map[string]any) ([]events.Event, error)
}

// Dispatch sends reactions to the registry on behalf of the reactor named
// name, in response to ev.
//
// It is exported because there are two reactor tiers — Go reactors and JS
// `//@trigger reactor` function files — and the rule below is the whole
// contract of both. Two copies would drift, and the halves that would drift
// are the ones that matter: retry-vs-continue, and the causation metadata
// the catalog's flow detection joins on.
//
// The metadata actor is deliberately "reactor:<name>" for BOTH tiers, even
// though they use different durable checkpoint keys — events/stats.go's
// ReactorFlows filters on that prefix, so matching it is what earns a JS
// reactor its edges in the catalog, the explorer and the mermaid diagram.
// warn is used for the one rejection kind that is knowably PERMANENT rather
// than a domain refusal (see the switch below); it may be nil, in which case
// it falls back to logger. Both tiers pass their own, because the level rule
// is part of the shared contract this function exists to keep in one place.
func Dispatch(ctx context.Context, registry Dispatcher, name string, ev events.Event, reactions []Reaction, logger, warn func(string, ...any)) error {
	if logger == nil {
		logger = func(string, ...any) {}
	}
	if warn == nil {
		warn = logger
	}
	for _, reaction := range reactions {
		meta := map[string]any{
			"actor":         "reactor:" + name,
			"causationId":   ev.ID,
			"correlationId": correlationID(ev),
		}
		_, err := registry.HandleWithMeta(ctx, reaction.Aggregate, reaction.ID, reaction.Command, meta)
		switch {
		case err == nil:
			logger("reaction dispatched",
				"reactor", name, "cause", ev.ID,
				"target", reaction.Aggregate+"/"+reaction.ID, "command", reaction.Command.Name)
		case errors.Is(err, events.ErrConcurrency):
			// the target stream moved between load and append; stop and
			// retry the whole event next pass
			return err
		case errors.Is(err, decider.ErrUnknownAggregate):
			// PERMANENT wiring fault, not a domain refusal: no redelivery
			// can ever succeed, so logging this at the same level as the
			// idempotency path below is how a lost reaction stays invisible
			// (F-2). The reactor gate refuses this at load, so reaching here
			// means the target went away UNDER a live reactor — the case a
			// load-time check cannot catch.
			//
			// Still log-and-continue rather than return: blocking the log on
			// a fault that will never clear would stop every other reaction
			// too. The checkpoint advancing is the known cost, and it is why
			// this is loud.
			warn("reaction dropped: target aggregate is not registered",
				"reactor", name, "cause", ev.ID,
				"target", reaction.Aggregate+"/"+reaction.ID, "command", reaction.Command.Name,
				"error", err)
		default:
			// domain rejection (incl. the idempotency path, e.g. "already
			// exists"): log and continue — never block the log.
			//
			// This stays INFO deliberately. At-least-once delivery means a
			// redelivered reaction hitting the target's own "already exists"
			// rule IS the idempotency mechanism working, and promoting the
			// whole arm to a warning would make correct operation noisy.
			// "unknown command" also lands here and should not — it is
			// permanent — but it arrives as an untyped errors.New from the
			// aggregate's own Decide, so there is nothing to match on. That
			// is why the gate is at load time. See f2-dispatch-gate-scope.md.
			logger("reaction rejected",
				"reactor", name, "cause", ev.ID,
				"target", reaction.Aggregate+"/"+reaction.ID, "command", reaction.Command.Name,
				"error", err)
		}
	}
	return nil
}

// correlationID inherits the triggering event's correlation, defaulting to
// the triggering event itself (it is the root of the chain).
func correlationID(ev events.Event) string {
	var meta map[string]any
	if err := json.Unmarshal(ev.Metadata, &meta); err == nil {
		if corr, ok := meta["correlationId"].(string); ok && corr != "" {
			return corr
		}
	}
	return ev.ID
}
