package retention

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/jamestryand/pocketcqrs/pii"
)

// Decision is a retention policy's answer to "may this subject be erased
// now?". Held: Reason is a short code stored in the clear on the subject's
// stream, and ReviewAt is when to ask again (zero: not on a schedule, as
// while a legal hold waits to be released).
type Decision struct {
	Allowed  bool
	Reason   string
	ReviewAt time.Time
}

// Allow is a decision that no retention duty stands.
func Allow() Decision { return Decision{Allowed: true} }

// Hold is a decision to keep the data, asking again at reviewAt.
func Hold(reason string, reviewAt time.Time) Decision {
	return Decision{Reason: reason, ReviewAt: reviewAt}
}

// Policy says whether a subject's data must still be kept. Owned by the
// business, not the library: which duties apply and for how long is a legal
// question per system. Implement it for anything bespoke, or configure
// PeriodPolicy for the common case; CompositePolicy combines several duties.
type Policy interface {
	Evaluate(ctx context.Context, subjectID string, now time.Time) (Decision, error)
}

// SubjectActivity is when the subject last did something that starts or
// extends a retention duty (a transaction, a contract ending, an account
// closing): the one domain fact PeriodPolicy can't know for itself.
type SubjectActivity interface {
	// LastActivity is the most recent such moment; false when there is none.
	LastActivity(ctx context.Context, subjectID string) (time.Time, bool, error)
}

// AllowAll approves every request: a system with nothing to retain, and
// tests.
type AllowAll struct{}

func (AllowAll) Evaluate(context.Context, string, time.Time) (Decision, error) {
	return Allow(), nil
}

// PeriodPolicy keeps a subject's data for Years and Months after their last
// activity, then allows the request. A subject with no recorded activity has
// nothing to retain. Reason defaults to "retention-period".
type PeriodPolicy struct {
	Years, Months int
	Activity      SubjectActivity
	Reason        string
}

func (p PeriodPolicy) Evaluate(ctx context.Context, subjectID string, now time.Time) (Decision, error) {
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

// CompositePolicy is several duties at once: held while any holds, until the
// latest review date among them, with their distinct reasons joined.
type CompositePolicy []Policy

func (c CompositePolicy) Evaluate(ctx context.Context, subjectID string, now time.Time) (Decision, error) {
	var reasons []string
	var review time.Time
	held := false
	for _, p := range c {
		d, err := p.Evaluate(ctx, subjectID, now)
		if err != nil {
			return Decision{}, err
		}
		if d.Allowed {
			continue
		}
		held = true
		if d.Reason != "" && !slices.Contains(reasons, d.Reason) {
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
	if len([]rune(reason)) > pii.MaxReasonLength {
		reason = string([]rune(reason)[:pii.MaxReasonLength])
	}
	if reason == "" {
		reason = "retention"
	}
	return Hold(reason, review), nil
}
