package pii

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/outbox"
)

// Governed erasure (decision 3), ported from dotnetcqrs's ErasureGovernance:
// the retention policy that says whether a request may proceed, and the
// governor that drives a request to a conclusion. The lifecycle itself is
// erasure.go.

// RetentionDecision is a retention policy's answer to "may this subject be
// erased now?". Held: Reason is a short code stored in the clear on the
// subject's stream, and ReviewAt is when to ask again (zero: not on a
// schedule, as while a legal hold waits to be released).
type RetentionDecision struct {
	Allowed  bool
	Reason   string
	ReviewAt time.Time
}

// Allow is a decision that no retention duty stands.
func Allow() RetentionDecision { return RetentionDecision{Allowed: true} }

// Hold is a decision to keep the data, asking again at reviewAt.
func Hold(reason string, reviewAt time.Time) RetentionDecision {
	return RetentionDecision{Reason: reason, ReviewAt: reviewAt}
}

// RetentionPolicy says whether a subject's data must still be kept. Owned by
// the business, not the library: which duties apply and for how long is a
// legal question per system. Implement it for anything bespoke, or configure
// PeriodRetentionPolicy for the common case; CompositeRetentionPolicy
// combines several duties.
type RetentionPolicy interface {
	Evaluate(ctx context.Context, subjectID string, now time.Time) (RetentionDecision, error)
}

// SubjectActivity is when the subject last did something that starts or
// extends a retention duty (their last entry, last payment, leaving date):
// the one domain fact PeriodRetentionPolicy can't know for itself.
type SubjectActivity interface {
	// LastActivity is the most recent such moment; false when there is none.
	LastActivity(ctx context.Context, subjectID string) (time.Time, bool, error)
}

// AllowAllRetentionPolicy approves every request: a system with nothing to
// retain, and tests.
type AllowAllRetentionPolicy struct{}

func (AllowAllRetentionPolicy) Evaluate(context.Context, string, time.Time) (RetentionDecision, error) {
	return Allow(), nil
}

// PeriodRetentionPolicy keeps a subject's data for Years and Months after
// their last activity, then allows the request. A subject with no recorded
// activity has nothing to retain. Reason defaults to "retention-period".
type PeriodRetentionPolicy struct {
	Years, Months int
	Activity      SubjectActivity
	Reason        string
}

func (p PeriodRetentionPolicy) Evaluate(ctx context.Context, subjectID string, now time.Time) (RetentionDecision, error) {
	last, ok, err := p.Activity.LastActivity(ctx, subjectID)
	if err != nil || !ok {
		return Allow(), err
	}
	until := last.AddDate(p.Years, p.Months, 0)
	if !now.Before(until) {
		return Allow(), nil
	}
	reason := p.Reason
	if reason == "" {
		reason = "retention-period"
	}
	return Hold(reason, until), nil
}

// CompositeRetentionPolicy is several duties at once (tax, employment,
// pension): held while any holds, until the latest review date among them.
type CompositeRetentionPolicy []RetentionPolicy

func (c CompositeRetentionPolicy) Evaluate(ctx context.Context, subjectID string, now time.Time) (RetentionDecision, error) {
	var reasons []string
	var review time.Time
	held := false
	for _, p := range c {
		d, err := p.Evaluate(ctx, subjectID, now)
		if err != nil {
			return RetentionDecision{}, err
		}
		if d.Allowed {
			continue
		}
		held = true
		if d.Reason != "" && !contains(reasons, d.Reason) {
			reasons = append(reasons, d.Reason)
		}
		if d.ReviewAt.After(review) {
			review = d.ReviewAt
		}
	}
	if !held {
		return Allow(), nil
	}
	reason := strings.Join(reasons, ", ")
	if len([]rune(reason)) > MaxReasonLength {
		reason = string([]rune(reason)[:MaxReasonLength])
	}
	if reason == "" {
		reason = "retention"
	}
	return Hold(reason, review), nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

const (
	// ErasureGovernorName is the governor's checkpoint name.
	ErasureGovernorName = "pii:erasure-governor"
	// ErasureReviewConsumer is the outbox consumer a held request's review
	// is recorded under, due on its review date.
	ErasureReviewConsumer = "pii:erasure-review"
	// SystemActor is recorded on everything the governor decides.
	SystemActor = "system:retention-policy"
)

// ErasureGovernor drives an erasure request to a conclusion. A consumer on
// the subject's own stream:
//
//   - ErasureRequested (and LegalHoldReleased): ask the RetentionPolicy, then
//     send ApproveErasure or HoldErasure. A legal hold overrides the policy.
//   - ErasureApproved: send EraseSubject, which the KeyDestroyer turns into a
//     destroyed key.
//   - ErasureHeld with a review date: record a review in Outbox due on that
//     date; Review (the outbox handler) asks the policy again then, so a held
//     request doesn't wait forever. Where dotnetcqrs reviews on a timer loop
//     over its queue, this is the due time itself (R4), never a poll.
//
// Everything it sends is idempotent and attributed to SystemActor, so the
// engine's at-least-once delivery is safe. It reads the subject's own stream
// (the write side's own record), never a read model. An effect: an imported
// request was ruled on by the deployment it came from.
type ErasureGovernor struct {
	Registry *decider.Registry
	Loader   decider.StreamLoader
	Policy   RetentionPolicy
	// Outbox holds the reviews of held requests; nil reviews none (a held
	// request then waits for a new request or a legal hold's release).
	Outbox *outbox.Store
	// Now is the clock (time.Now when nil).
	Now    func() time.Time
	Logger func(msg string, args ...any)
}

func (*ErasureGovernor) Name() string   { return ErasureGovernorName }
func (*ErasureGovernor) IsEffect() bool { return true }

func (g *ErasureGovernor) now() time.Time {
	if g.Now != nil {
		return g.Now().UTC()
	}
	return time.Now().UTC()
}

func (g *ErasureGovernor) log(msg string, args ...any) {
	if g.Logger != nil {
		g.Logger(msg, args...)
	}
}

func (g *ErasureGovernor) Apply(ctx context.Context, ev events.Event) error {
	if ev.Aggregate != SubjectAggregate {
		return nil
	}
	switch ev.Type {
	case ErasureRequestedEvent, LegalHoldReleasedEvent:
		return g.Evaluate(ctx, ev.AggregateID)
	case ErasureApprovedEvent:
		return g.dispatch(ctx, ev.AggregateID, EraseSubjectCommand, nil)
	case ErasureHeldEvent:
		var p subjectPayload
		_ = json.Unmarshal(ev.Data, &p)
		if p.ReviewAt == nil || g.Outbox == nil {
			return nil
		}
		review := p.ReviewAt.UTC()
		payload, _ := json.Marshal(map[string]string{"subject": ev.AggregateID})
		return g.Outbox.Enqueue(ctx, fmt.Sprintf("%s|%s|%s", ErasureReviewConsumer, ev.AggregateID, review.Format(time.RFC3339)),
			ErasureReviewConsumer, payload, review)
	}
	return nil
}

// Review asks the policy again for a held request whose review date has come
// (an outbox.Handler). A failure is due again in an hour.
func (g *ErasureGovernor) Review(ctx context.Context, e outbox.Entry) outbox.Outcome {
	var p struct {
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil || p.Subject == "" {
		g.log("erasure review unreadable, dropped", "id", e.ID)
		return outbox.Outcome{Done: true}
	}
	if err := g.Evaluate(ctx, p.Subject); err != nil {
		return outbox.Outcome{RetryAt: g.now().Add(time.Hour), Err: err}
	}
	return outbox.Outcome{Done: true}
}

// Evaluate rules on one subject's open request: nothing happens if there is
// none.
func (g *ErasureGovernor) Evaluate(ctx context.Context, subjectID string) error {
	stream, err := g.Loader.LoadStream(ctx, SubjectAggregate, subjectID)
	if err != nil {
		return err
	}
	d := DataSubject()
	state := d.InitialState()
	for _, ev := range stream {
		if state, err = d.Evolve(state, ev); err != nil {
			return err
		}
	}
	if state.Erased || (state.Stage != StageRequested && state.Stage != StageHeld) {
		return nil
	}
	if state.LegalHold {
		// a legal hold overrides any policy; the request waits, with no
		// review date (releasing the hold is what re-opens it)
		if state.Stage == StageRequested {
			return g.dispatch(ctx, subjectID, HoldErasureCommand, map[string]any{"reason": "legal-hold"})
		}
		return nil
	}
	decision, err := g.Policy.Evaluate(ctx, subjectID, g.now())
	if err != nil {
		return err
	}
	if decision.Allowed {
		return g.dispatch(ctx, subjectID, ApproveErasureCommand, nil)
	}
	reason := decision.Reason
	if reason == "" {
		reason = "retention"
	}
	payload := map[string]any{"reason": reason}
	if !decision.ReviewAt.IsZero() {
		payload["reviewAt"] = decision.ReviewAt.UTC().Format(time.RFC3339)
	}
	return g.dispatch(ctx, subjectID, HoldErasureCommand, payload)
}

func (g *ErasureGovernor) dispatch(ctx context.Context, subjectID, command string, payload map[string]any) error {
	data := json.RawMessage(`{}`)
	if payload != nil {
		data, _ = json.Marshal(payload)
	}
	meta := map[string]any{"actor": SystemActor, "now": g.now().Format("2006-01-02T15:04:05.000Z")}
	_, err := g.Registry.HandleWithMeta(ctx, SubjectAggregate, subjectID, decider.Command{Name: command, Payload: data}, meta)
	if decider.IsRejection(err) {
		// the subject moved on between reading and deciding (erased, held, a
		// hold placed): the current state wins, and retrying would fail the
		// same way, so this is not a delivery failure
		g.log("erasure command not applied", "command", command, "subject", subjectID, "reason", err)
		return nil
	}
	return err
}
