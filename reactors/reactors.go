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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"

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

// applied reports whether a command has already produced events on a stream.
//
// Optional at the type level so a test fake need not implement it — but the
// real *decider.Registry MUST, or redelivery protection silently does nothing.
// That is not a hypothetical: this interface was added before the registry
// implemented it, the assertion failed open, and three deliveries produced
// three events with nothing to say why. assertAppliedImplemented pins it.
type applied interface {
	CommandApplied(ctx context.Context, aggregate, aggregateID, commandID string, afterPosition int64) (bool, error)
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
//
// actor and provenance answer different questions and must not be conflated:
// actor says which reactor produced this reaction, unconditionally; provenance
// (present only when the causing event carries one — see causeProvenance)
// says whether that reactor's own cause crossed a trust boundary, e.g. a
// federated peer deployment once that exists. A local reactor reacting to a
// federation-ingested event inherits that event's provenance onto its own
// reactions, the same way correlationId already propagates across a reaction
// chain — so provenance survives however many local hops separate a command
// from the peer-originated event that ultimately caused it.
func Dispatch(ctx context.Context, registry Dispatcher, name string, ev events.Event, reactions []Reaction, logger, warn func(string, ...any)) error {
	if logger == nil {
		logger = func(string, ...any) {}
	}
	if warn == nil {
		warn = logger
	}
	for i, reaction := range reactions {
		// A reaction IS a command, so it carries a commandId like any other —
		// the same durable proof CommitBatch stamps for queued commands.
		//
		// It has to be DERIVED, not minted. Delivery is at-least-once, so a
		// redelivery of the same source event must produce the SAME id, or
		// every replay looks like new work. The stable inputs are to hand: the
		// reactor's name, the causing event's id, and this reaction's index.
		commandID := reactionCommandID(name, ev.ID, i)
		meta := map[string]any{
			"actor":         "reactor:" + name,
			"causationId":   ev.ID,
			"correlationId": correlationID(ev),
			"commandId":     commandID,
		}
		if provenance := causeProvenance(ev); provenance != "" {
			meta["provenance"] = provenance
		}

		// Skip a reaction whose events are already in the log.
		//
		// Until now a redelivered reaction relied on the TARGET having a
		// natural uniqueness rule to reject it — true for CreateTask, false
		// for AddOrderLine, where adding the same line twice is a legitimate
		// outcome. That is the same partial guarantee docs/reference/gateway.md
		// was wrong to claim for retried commands, one tier over. A derived id
		// plus this check does not depend on the domain's shape.
		//
		// A reaction cannot precede its cause, so the cause's own position
		// bounds the scan — which is what lets idx_events_command_id serve it
		// rather than walking the whole log.
		if store, ok := registry.(applied); ok {
			done, err := store.CommandApplied(ctx, reaction.Aggregate, reaction.ID, commandID, ev.Position-1)
			if err != nil {
				return err
			}
			if done {
				logger("reaction already applied, skipped",
					"reactor", name, "cause", ev.ID,
					"target", reaction.Aggregate+"/"+reaction.ID, "command", reaction.Command.Name)
				continue
			}
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

// assertAppliedImplemented fails to compile if the real dispatcher stops
// satisfying the optional check above. An optional interface that silently
// does nothing is exactly the failure this file already exists to prevent.
var _ applied = (*decider.Registry)(nil)

// reactionCommandID derives a stable id for one reaction of one delivery.
//
// Deterministic by construction: the same cause redelivered derives the same
// id, which is the whole point. Hashed rather than concatenated so the id is
// opaque and fixed-width, and so a reactor name containing the separator
// cannot collide with a different one.
func reactionCommandID(reactor, causeID string, index int) string {
	h := sha256.New()
	for _, part := range []string{"reaction", reactor, causeID, strconv.Itoa(index)} {
		h.Write([]byte(part))
		h.Write([]byte{0}) // separator, so parts cannot run together
	}
	return "reaction-" + hex.EncodeToString(h.Sum(nil)[:16])
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

// causeProvenance inherits the triggering event's provenance, if it has one.
//
// Unlike correlationID there is no meaningful self-value to fall back to: a
// plain local event has nothing to claim, so the zero value ("") is the
// correct answer, and callers must treat it as "omit the key" rather than
// stamp an empty string into metadata forever.
func causeProvenance(ev events.Event) string {
	var meta map[string]any
	if err := json.Unmarshal(ev.Metadata, &meta); err == nil {
		if p, ok := meta["provenance"].(string); ok {
			return p
		}
	}
	return ""
}
