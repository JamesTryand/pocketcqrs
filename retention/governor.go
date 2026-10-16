package retention

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/outbox"
	"github.com/jamestryand/pocketcqrs/pii"
)

const (
	// GovernorName is the governor's checkpoint name.
	GovernorName = "retention:governor"
	// ReviewConsumer is the outbox consumer a held request's review is
	// recorded under, due on its review date.
	ReviewConsumer = "retention:review"
	// SystemActor is recorded on everything the governor decides.
	SystemActor = "system:retention-policy"
)

// Governor drives an erasure request to a conclusion. A consumer on the
// subject's own stream:
//
//   - ErasureRequested (and LegalHoldReleased): ask the Policy, then send
//     ApproveErasure or HoldErasure. A legal hold overrides the policy.
//   - ErasureApproved: send EraseSubject, which pii's KeyDestroyer turns
//     into a destroyed key.
//   - ErasureHeld with a review date: record a review in Outbox due on that
//     date; Review (the outbox handler) asks the policy again then, so a held
//     request doesn't wait forever. The due time itself, never a poll.
//
// Everything it sends is idempotent and attributed to SystemActor, so the
// engine's at-least-once delivery is safe. It reads the subject's own stream
// (the write side's own record), never a read model. An effect: an imported
// request was ruled on by the deployment it came from.
type Governor struct {
	Registry *decider.Registry
	Loader   decider.StreamLoader
	Policy   Policy
	// Outbox holds the reviews of held requests; nil reviews none (a held
	// request then waits for a new request or a legal hold's release).
	Outbox *outbox.Store
	// Now is the clock (time.Now when nil).
	Now    func() time.Time
	Logger func(msg string, args ...any)
}

func (*Governor) Name() string   { return GovernorName }
func (*Governor) IsEffect() bool { return true }

func (g *Governor) now() time.Time {
	if g.Now != nil {
		return g.Now().UTC()
	}
	return time.Now().UTC()
}

func (g *Governor) log(msg string, args ...any) {
	if g.Logger != nil {
		g.Logger(msg, args...)
	}
}

func (g *Governor) Apply(ctx context.Context, ev events.Event) error {
	if ev.Aggregate != pii.SubjectAggregate {
		return nil
	}
	switch ev.Type {
	case pii.ErasureRequestedEvent, LegalHoldReleasedEvent:
		return g.Evaluate(ctx, ev.AggregateID)
	case ErasureApprovedEvent:
		return g.dispatch(ctx, ev.AggregateID, pii.EraseSubjectCommand, nil)
	case ErasureHeldEvent:
		var p payload
		_ = json.Unmarshal(ev.Data, &p)
		if p.ReviewAt == nil || g.Outbox == nil {
			return nil
		}
		review := p.ReviewAt.UTC()
		body, _ := json.Marshal(map[string]string{"subject": ev.AggregateID})
		return g.Outbox.Enqueue(ctx, fmt.Sprintf("%s|%s|%s", ReviewConsumer, ev.AggregateID, review.Format(time.RFC3339)),
			ReviewConsumer, body, review)
	}
	return nil
}

// Review asks the policy again for a held request whose review date has come
// (an outbox.Handler). A failure is due again in an hour.
func (g *Governor) Review(ctx context.Context, e outbox.Entry) outbox.Outcome {
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
func (g *Governor) Evaluate(ctx context.Context, subjectID string) error {
	stream, err := g.Loader.LoadStream(ctx, pii.SubjectAggregate, subjectID)
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
	fields := map[string]any{"reason": reason}
	if !decision.ReviewAt.IsZero() {
		fields["reviewAt"] = decision.ReviewAt.UTC().Format(time.RFC3339)
	}
	return g.dispatch(ctx, subjectID, HoldErasureCommand, fields)
}

func (g *Governor) dispatch(ctx context.Context, subjectID, command string, fields map[string]any) error {
	data := json.RawMessage(`{}`)
	if fields != nil {
		data, _ = json.Marshal(fields)
	}
	meta := map[string]any{"actor": SystemActor, "now": g.now().Format("2006-01-02T15:04:05.000Z")}
	_, err := g.Registry.HandleWithMeta(ctx, pii.SubjectAggregate, subjectID, decider.Command{Name: command, Payload: data}, meta)
	if decider.IsRejection(err) {
		// the subject moved on between reading and deciding (erased, held, a
		// hold placed): the current state wins, and retrying would fail the
		// same way, so this is not a delivery failure
		g.log("erasure command not applied", "command", command, "subject", subjectID, "reason", err)
		return nil
	}
	return err
}
