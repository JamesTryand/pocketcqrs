// Package extcaller implements the external-service-caller: a
// consumers.Consumer that matches committed events against configured
// rules, calls a third-party REST API through a bounded outbound.Client, and
// dispatches the response as a follow-up command through a pocketcqrs
// gateway — never appends a raw event, so the target deployment's decider
// keeps authority to accept or reject the result.
//
// This is a plain consumers.Consumer, not a reactors.Reactor: Reactor.React
// is a pure, synchronous, no-I/O function, and this component's whole job is
// an I/O call reactors.Reactor has no way to make.
//
// Delivery from the event log is at-least-once, so Apply always returns nil
// (matching the effect-function tier's eventFunction.Apply, not the reactor
// tier's Dispatch): a permanently failing rule or exhausted retry budget
// dead-letters and the checkpoint still advances, so one stuck integration
// can never block the rest of the log. A dead-lettered event's checkpoint has
// already passed it, so retrying it is a manual operator action, not
// something the engine does on its own — see the README's "Dead letters"
// section for exactly what's available (list/dismiss reuse the pocketcqrs
// CLI against this component's local store; retry does not, since that CLI
// command re-delivers through the JS function runtime, a different tier).
package extcaller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/outbound"

	"github.com/jamestryand/pocketcqrs-extensions/internal/gatewayclient"
)

// FollowUp is one command a Rule's HandleResponse wants dispatched. The
// consumer derives its own deterministic Idempotency-Key from the source
// event and this follow-up's position in the slice HandleResponse returned,
// so rule authors never invent one themselves — the same reasoning
// reactors.reactionCommandID documents: it must be DERIVED, not authored, or
// a redelivery of the same source event stops looking like a redelivery.
type FollowUp struct {
	Aggregate string
	ID        string
	Name      string
	Payload   json.RawMessage
}

// Rule maps one event type to an outbound request and maps that request's
// response to zero or more follow-up commands.
//
// Deliberately plain Go functions, not a declarative mapping DSL (URL
// templates, field-path response extraction, etc.): no real third-party
// integration exists yet to design such a thing against (NEEDS.md defers
// exactly this), and a config format invented against zero real callers is
// the wrong kind of abstraction to build speculatively. A Rule is assembled
// in Go by whoever wires up main.go for a given deployment.
type Rule struct {
	// EventType is matched against events.Event.Type exactly.
	EventType string
	// BuildRequest turns the causing event into the outbound call to make.
	BuildRequest func(ev events.Event) (outbound.Request, error)
	// HandleResponse turns a successful outbound.Response into zero or more
	// follow-up commands. A non-nil error is treated as a permanent failure
	// for this event (dead-lettered), same as a BuildRequest or outbound
	// call failure.
	HandleResponse func(ev events.Event, resp *outbound.Response) ([]FollowUp, error)
}

// RetryPolicy bounds how many times Apply calls out for one event before
// giving up and dead-lettering. Deliberately just a fixed attempt count and a
// fixed backoff, not a per-target/configurable policy — see Rule's doc
// comment for why a fuller shape isn't built yet.
type RetryPolicy struct {
	// MaxAttempts is the total number of outbound.Client.Do calls to make
	// before giving up. Must be at least 1; 1 means no retry.
	MaxAttempts int
	// Backoff is the fixed pause between attempts.
	Backoff time.Duration
}

// Config configures one Consumer.
type Config struct {
	// Name identifies this consumer instance: its durable checkpoint key is
	// "extcall:<Name>" and its dead letters are recorded under the same
	// string, matching the "<tier>:<name>" naming convention the other
	// consumer tiers use (e.g. "fn:<name>", "reactor:<name>").
	Name string
	// Rules are matched by event type; at most one Rule may claim a given
	// EventType.
	Rules []Rule
	// Outbound makes the third-party call. Must already be configured with
	// this deployment's global host allow-list — extcaller does not modify
	// or bypass it.
	Outbound *outbound.Client
	// Gateway dispatches follow-up commands.
	Gateway *gatewayclient.Client
	// DeadLetters records permanent per-event failures. Point this at a
	// locally-writable *events.Store this component owns outright — never
	// the upstream deployment's own events.db (see internal/localstore).
	DeadLetters *events.Store
	// Retry bounds the outbound call attempts. Zero value means exactly one
	// attempt (no retry).
	Retry RetryPolicy
	// Logger receives operational log lines; nil is a valid no-op.
	Logger func(msg string, args ...any)
}

// Consumer is a consumers.Consumer implementing the external-service-caller.
type Consumer struct {
	cfg   Config
	rules map[string]Rule
}

// New builds a Consumer from cfg.
func New(cfg Config) (*Consumer, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("extcaller: Config.Name must not be empty")
	}
	if cfg.Outbound == nil {
		return nil, fmt.Errorf("extcaller: Config.Outbound must not be nil")
	}
	if cfg.Gateway == nil {
		return nil, fmt.Errorf("extcaller: Config.Gateway must not be nil")
	}
	if cfg.DeadLetters == nil {
		return nil, fmt.Errorf("extcaller: Config.DeadLetters must not be nil")
	}
	if cfg.Retry.MaxAttempts <= 0 {
		cfg.Retry.MaxAttempts = 1
	}
	if cfg.Logger == nil {
		cfg.Logger = func(string, ...any) {}
	}

	rules := make(map[string]Rule, len(cfg.Rules))
	for _, r := range cfg.Rules {
		if r.EventType == "" {
			return nil, fmt.Errorf("extcaller: a Rule has an empty EventType")
		}
		if _, dup := rules[r.EventType]; dup {
			return nil, fmt.Errorf("extcaller: more than one Rule claims event type %q", r.EventType)
		}
		if r.BuildRequest == nil || r.HandleResponse == nil {
			return nil, fmt.Errorf("extcaller: Rule for %q must set BuildRequest and HandleResponse", r.EventType)
		}
		rules[r.EventType] = r
	}

	return &Consumer{cfg: cfg, rules: rules}, nil
}

// Name implements consumers.Consumer: the durable checkpoint key.
func (c *Consumer) Name() string { return "extcall:" + c.cfg.Name }

// Apply implements consumers.Consumer. Always returns nil — see the package
// doc comment for why a permanent failure dead-letters rather than stalling
// the checkpoint.
func (c *Consumer) Apply(ctx context.Context, ev events.Event) error {
	rule, ok := c.rules[ev.Type]
	if !ok {
		return nil
	}

	req, err := rule.BuildRequest(ev)
	if err != nil {
		c.deadLetter(ctx, ev, fmt.Errorf("building outbound request: %w", err))
		return nil
	}

	resp, err := c.callWithRetry(ctx, req)
	if err != nil {
		c.deadLetter(ctx, ev, fmt.Errorf("calling out after %d attempt(s): %w", c.cfg.Retry.MaxAttempts, err))
		return nil
	}

	followUps, err := rule.HandleResponse(ev, resp)
	if err != nil {
		c.deadLetter(ctx, ev, fmt.Errorf("handling response: %w", err))
		return nil
	}

	for i, f := range followUps {
		cmd := gatewayclient.Command{
			Aggregate:           f.Aggregate,
			ID:                  f.ID,
			Name:                f.Name,
			Payload:             f.Payload,
			IdempotencyKeyParts: []string{c.Name(), ev.ID, strconv.Itoa(i)},
		}
		if _, err := c.cfg.Gateway.Dispatch(ctx, cmd); err != nil {
			// A follow-up dispatch failure (domain rejection, concurrency
			// conflict, the target deployment unreachable) dead-letters the
			// SOURCE event rather than retrying just this follow-up: partial
			// application (an earlier follow-up in this same slice already
			// succeeded) is a real possibility the operator needs visibility
			// into, not something to silently paper over.
			c.deadLetter(ctx, ev, fmt.Errorf("dispatching follow-up %d/%d (%s %s/%s): %w",
				i+1, len(followUps), f.Name, f.Aggregate, f.ID, err))
			return nil
		}
		c.cfg.Logger("follow-up dispatched",
			"consumer", c.Name(), "event", ev.ID, "command", f.Name, "target", f.Aggregate+"/"+f.ID)
	}
	return nil
}

// callWithRetry calls req through cfg.Outbound up to cfg.Retry.MaxAttempts
// times, pausing cfg.Retry.Backoff between attempts. One attempt means no
// retry, matching outbound.Client.Do's own "one attempt, no retry"
// contract — the retry budget lives here, one layer up, so it stays visible
// and boundable rather than hidden inside the HTTP client.
func (c *Consumer) callWithRetry(ctx context.Context, req outbound.Request) (*outbound.Response, error) {
	var lastErr error
	for attempt := 1; attempt <= c.cfg.Retry.MaxAttempts; attempt++ {
		resp, err := c.cfg.Outbound.Do(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if attempt == c.cfg.Retry.MaxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(c.cfg.Retry.Backoff):
		}
	}
	return nil, lastErr
}

// deadLetter records a permanent per-event failure.
func (c *Consumer) deadLetter(ctx context.Context, ev events.Event, cause error) {
	c.cfg.Logger("event delivery failed, dead-lettered",
		"consumer", c.Name(), "event", ev.ID, "position", ev.Position, "error", cause)
	if err := c.cfg.DeadLetters.AddDeadLetter(ctx, c.Name(), ev, cause); err != nil {
		c.cfg.Logger("failed to write dead letter", "consumer", c.Name(), "error", err)
	}
}
