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
// Apply only records the work, in an outbox (package outbox, R4): the
// checkpoint advances at once, and one slow or failing integration never
// holds up the log. The outbox Worker (Consumer.Worker) does the work when
// it is due: build the request, call out, turn the response into follow-up
// commands (kept in the entry, so a later retry doesn't call out again),
// and dispatch them. A failed call, or a follow-up the gateway answers with
// a conflict (re-decided on the next try, as batching re-decides), timeout
// or maintenance, is due again after the backoff: a due time, never a pause.
// A permanent failure, or the attempts spent, dead-letters. A dead letter is
// a manual operator action from then on — see the README's "Dead letters"
// section for exactly what's available (list/dismiss reuse the pocketcqrs
// CLI against this component's local store; retry does not, since that CLI
// command re-delivers through the JS function runtime, a different tier).
package extcaller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/outbound"
	"github.com/jamestryand/pocketcqrs/outbox"

	"github.com/jamestryand/pocketcqrs/internal/gatewayclient"
)

// Command is one follow-up command to dispatch to a pocketcqrs gateway —
// the shape Gateway.Dispatch receives. Deliberately its own type, not
// gatewayclient.Command: gatewayclient stays under internal/, unreachable
// from outside this module, so an external consumer implementing Gateway
// needs a Command it can actually construct.
type Command struct {
	Aggregate string
	ID        string
	Name      string
	Payload   json.RawMessage

	// IdempotencyKeyParts, hashed together, derive the dispatch's
	// idempotency key deterministically — see gatewayclient.Command's own
	// doc comment for the full reasoning; the contract is identical here.
	IdempotencyKeyParts []string

	// CausationID and CorrelationID identify this follow-up's place in a
	// reaction chain, mirroring reactors.Dispatch's own metadata (see
	// pocketcqrs's reactors.go) — the gateway only honors them for a caller
	// its ExternalCallerCollection recognizes. Consumer.Apply always sets
	// both.
	CausationID   string
	CorrelationID string
}

// RetryableError marks a dispatch failure worth trying again: a conflict
// (the gateway re-decides against the stream as it then is), maintenance, a
// timeout or the deployment unreachable. Any other dispatch error (a domain
// rejection) dead-letters. A Gateway of your own wraps its own the same way.
type RetryableError struct{ Err error }

func (e *RetryableError) Error() string { return e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// Gateway dispatches one follow-up command to a pocketcqrs deployment.
// Satisfied by the value NewGatewayClient returns (which wraps the real
// HTTP client this repo keeps under internal/gatewayclient); an external
// module consuming this package supplies its own implementation instead —
// see NEEDS.md's "extcaller cannot be consumed as an external Go module
// dependency" for why this is an interface rather than a concrete type.
type Gateway interface {
	Dispatch(ctx context.Context, cmd Command) error
}

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

// RetryPolicy bounds how many times the worker tries one event's call (and,
// separately, its follow-ups' dispatch) before dead-lettering. Deliberately
// just a fixed attempt count and a fixed backoff, not a per-target policy —
// see Rule's doc comment for why a fuller shape isn't built yet.
type RetryPolicy struct {
	// MaxAttempts is the number of tries before giving up. Must be at least
	// 1; 1 means no retry.
	MaxAttempts int
	// Backoff is how long after a failed try the next one is due.
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
	// Gateway dispatches follow-up commands. Build one with
	// NewGatewayClient against a real pocketcqrs deployment, or supply your
	// own implementation.
	Gateway Gateway
	// DeadLetters records permanent per-event failures. Point this at a
	// locally-writable *events.Store this component owns outright — never
	// the upstream deployment's own events.db (see internal/localstore).
	DeadLetters *events.Store
	// Outbox holds the work Apply records until the worker has done it: its
	// own SQLite file, this component's own.
	Outbox *outbox.Store
	// Now is the clock (time.Now when nil).
	Now func() time.Time
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
	if cfg.Outbox == nil {
		return nil, fmt.Errorf("extcaller: Config.Outbox must not be nil")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
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

// IsEffect implements consumers.Effect: the external caller calls out, so
// it makes no call for an imported event, which already happened elsewhere.
func (c *Consumer) IsEffect() bool { return true }

// Apply implements consumers.Consumer: it records the event's work in the
// outbox, once per event (a redelivery is the same entry), for the worker.
// It fails only when the work can't be recorded, so the checkpoint never
// passes work nobody will do.
func (c *Consumer) Apply(ctx context.Context, ev events.Event) error {
	if _, ok := c.rules[ev.Type]; !ok {
		return nil
	}
	payload, err := json.Marshal(work{Event: ev})
	if err != nil {
		return err
	}
	return c.cfg.Outbox.Enqueue(ctx, c.Name()+"|"+ev.ID, c.Name(), payload, c.cfg.Now())
}

// work is an outbox entry's state: the event, and once the call has been
// made, its follow-ups and how far their dispatch has got.
type work struct {
	Event     events.Event `json:"event"`
	Called    bool         `json:"called,omitempty"`
	FollowUps []FollowUp   `json:"followUps,omitempty"`
	// Next is the first follow-up not yet dispatched.
	Next int `json:"next,omitempty"`
	// Tries counts this phase's tries (the call, then the dispatch).
	Tries int `json:"tries,omitempty"`
}

// Worker is the outbox worker that does this consumer's work; run it
// (Worker.Run) beside the engine.
func (c *Consumer) Worker() *outbox.Worker {
	return &outbox.Worker{Store: c.cfg.Outbox, Handlers: map[string]outbox.Handler{c.Name(): c.Handle},
		Now: c.cfg.Now, Logger: c.cfg.Logger}
}

// Handle does one entry's work (an outbox.Handler).
func (c *Consumer) Handle(ctx context.Context, e outbox.Entry) outbox.Outcome {
	var w work
	if err := json.Unmarshal(e.Payload, &w); err != nil {
		c.cfg.Logger("outbox entry unreadable, dropped", "consumer", c.Name(), "id", e.ID, "error", err)
		return outbox.Outcome{Done: true}
	}
	ev := w.Event
	rule, ok := c.rules[ev.Type]
	if !ok {
		c.deadLetter(ctx, ev, fmt.Errorf("no rule for %s any more", ev.Type))
		return outbox.Outcome{Done: true}
	}
	retry := func(cause error, phase string) outbox.Outcome {
		w.Tries++
		if w.Tries >= c.cfg.Retry.MaxAttempts {
			c.deadLetter(ctx, ev, fmt.Errorf("%s after %d attempt(s): %w", phase, w.Tries, cause))
			return outbox.Outcome{Done: true}
		}
		payload, _ := json.Marshal(w)
		return outbox.Outcome{RetryAt: c.cfg.Now().Add(c.cfg.Retry.Backoff), Payload: payload, Err: cause}
	}

	if !w.Called {
		req, err := rule.BuildRequest(ev)
		if err != nil {
			c.deadLetter(ctx, ev, fmt.Errorf("building outbound request: %w", err))
			return outbox.Outcome{Done: true}
		}
		resp, err := c.cfg.Outbound.Do(ctx, req)
		if err != nil {
			if errors.Is(err, outbound.ErrHostNotAllowed) {
				c.deadLetter(ctx, ev, fmt.Errorf("calling out: %w", err))
				return outbox.Outcome{Done: true}
			}
			return retry(err, "calling out")
		}
		if w.FollowUps, err = rule.HandleResponse(ev, resp); err != nil {
			c.deadLetter(ctx, ev, fmt.Errorf("handling response: %w", err))
			return outbox.Outcome{Done: true}
		}
		w.Called, w.Tries = true, 0
	}

	for i := w.Next; i < len(w.FollowUps); i++ {
		f := w.FollowUps[i]
		cmd := Command{
			Aggregate:           f.Aggregate,
			ID:                  f.ID,
			Name:                f.Name,
			Payload:             f.Payload,
			IdempotencyKeyParts: []string{c.Name(), ev.ID, strconv.Itoa(i)},
			CausationID:         ev.ID,
			CorrelationID:       correlationID(ev),
		}
		if err := c.cfg.Gateway.Dispatch(ctx, cmd); err != nil {
			where := fmt.Sprintf("dispatching follow-up %d/%d (%s %s/%s)", i+1, len(w.FollowUps), f.Name, f.Aggregate, f.ID)
			var again *RetryableError
			if errors.As(err, &again) {
				w.Next = i // the ones before it are dispatched; their keys make a resend harmless anyway
				return retry(err, where)
			}
			// A rejection dead-letters the SOURCE event rather than this
			// follow-up alone: partial application (an earlier follow-up in
			// this same slice already succeeded) is a real possibility the
			// operator needs visibility into, not something to paper over.
			c.deadLetter(ctx, ev, fmt.Errorf("%s: %w", where, err))
			return outbox.Outcome{Done: true}
		}
		c.cfg.Logger("follow-up dispatched",
			"consumer", c.Name(), "event", ev.ID, "command", f.Name, "target", f.Aggregate+"/"+f.ID)
	}
	return outbox.Outcome{Done: true}
}

// correlationID inherits the causing event's own correlationId metadata,
// defaulting to the event's own id if absent when it is itself the root of
// the chain. Mirrors pocketcqrs's reactors.go correlationID rule exactly;
// reimplemented here because that helper is unexported, and this is a
// separate Go module regardless.
func correlationID(ev events.Event) string {
	var meta map[string]any
	if err := json.Unmarshal(ev.Metadata, &meta); err == nil {
		if corr, ok := meta["correlationId"].(string); ok && corr != "" {
			return corr
		}
	}
	return ev.ID
}

// gatewayClientAdapter adapts *gatewayclient.Client to Gateway, translating
// between this package's public Command and gatewayclient's own (internal,
// unreachable from outside this module) Command/Result types.
type gatewayClientAdapter struct{ c *gatewayclient.Client }

// Dispatch implements Gateway.
func (a gatewayClientAdapter) Dispatch(ctx context.Context, cmd Command) error {
	_, err := a.c.Dispatch(ctx, gatewayclient.Command{
		Aggregate:           cmd.Aggregate,
		ID:                  cmd.ID,
		Name:                cmd.Name,
		Payload:             cmd.Payload,
		IdempotencyKeyParts: cmd.IdempotencyKeyParts,
		CausationID:         cmd.CausationID,
		CorrelationID:       cmd.CorrelationID,
	})
	if err == nil {
		return nil
	}
	var status *gatewayclient.ErrStatus
	if !errors.As(err, &status) {
		return &RetryableError{Err: err} // the deployment unreachable
	}
	switch status.Status {
	case http.StatusConflict, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return &RetryableError{Err: err}
	}
	return err
}

// NewGatewayClient builds a Gateway that dispatches over HTTP to one real
// pocketcqrs deployment's command gateway. baseURL is the deployment's root
// (e.g. "http://localhost:8090"); token is sent as "Authorization: Bearer
// <token>" on every request — minting it (a superuser token or a dedicated
// service-account record) is an operator decision, out of scope here.
func NewGatewayClient(baseURL, token string, timeout time.Duration) Gateway {
	return gatewayClientAdapter{c: gatewayclient.New(baseURL, token, timeout)}
}

// deadLetter records a permanent per-event failure.
func (c *Consumer) deadLetter(ctx context.Context, ev events.Event, cause error) {
	c.cfg.Logger("event delivery failed, dead-lettered",
		"consumer", c.Name(), "event", ev.ID, "position", ev.Position, "error", cause)
	if err := c.cfg.DeadLetters.AddDeadLetter(ctx, c.Name(), ev, cause); err != nil {
		c.cfg.Logger("failed to write dead letter", "consumer", c.Name(), "error", err)
	}
}
