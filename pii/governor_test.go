package pii

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/kms"
	"github.com/jamestryand/pocketcqrs/outbox"
)

// The governor, ported from dotnetcqrs's ErasureWorkflowTests. Where
// dotnetcqrs reviews held requests on a timer loop over its queue, this
// governor records a review in the outbox due on the hold's review date; the
// rig's clock moves and the worker takes what is due.

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type fixedActivity time.Time

func (f fixedActivity) LastActivity(context.Context, string) (time.Time, bool, error) {
	return time.Time(f), true, nil
}

type rig struct {
	t        *testing.T
	store    *events.Store
	registry *decider.Registry
	kms      *kms.Memory
	engine   *consumers.Engine
	box      *outbox.Store
	worker   *outbox.Worker
	governor *ErasureGovernor
	now      time.Time
}

func newRig(t *testing.T, policy RetentionPolicy) *rig {
	t.Helper()
	store, r := bareSubjects(t)
	box, err := outbox.Open(filepath.Join(t.TempDir(), "outbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { box.Close() })
	g := &rig{t: t, store: store, registry: r, kms: kms.NewMemory(), box: box, now: epoch}
	g.governor = &ErasureGovernor{Registry: r, Loader: store, Policy: policy, Outbox: box, Now: func() time.Time { return g.now }}
	g.engine = consumers.NewEngine(store, nil)
	g.engine.Register(KeyDestroyer{KMS: g.kms})
	g.engine.Register(g.governor)
	g.worker = &outbox.Worker{Store: box, Now: func() time.Time { return g.now },
		Handlers: map[string]outbox.Handler{ErasureReviewConsumer: g.governor.Review}}
	return g
}

func (g *rig) send(id, cmd, payload, actor string) {
	g.t.Helper()
	if payload == "" {
		payload = "{}"
	}
	if actor == "" {
		actor = "u"
	}
	if _, err := g.registry.HandleWithMeta(context.Background(), SubjectAggregate, id,
		decider.Command{Name: cmd, Payload: json.RawMessage(payload)},
		map[string]any{"actor": actor, "now": g.now.Format("2006-01-02T15:04:05.000Z")}); err != nil {
		g.t.Fatal(err)
	}
}

// settle runs the engine and the reviews due until nothing moves.
func (g *rig) settle() {
	g.t.Helper()
	ctx := context.Background()
	for range 10 {
		if err := g.engine.RunOnce(ctx); err != nil {
			g.t.Fatal(err)
		}
		if n, err := g.worker.RunOnce(ctx); err != nil {
			g.t.Fatal(err)
		} else if n == 0 {
			if err := g.engine.RunOnce(ctx); err != nil {
				g.t.Fatal(err)
			}
			return
		}
	}
}

func (g *rig) types(id string) []string {
	stream, _ := g.store.LoadStream(context.Background(), SubjectAggregate, id)
	var out []string
	for _, ev := range stream {
		out = append(out, ev.Type)
	}
	return out
}

func (g *rig) state(id string) SubjectState {
	d := DataSubject()
	s := d.InitialState()
	stream, _ := g.store.LoadStream(context.Background(), SubjectAggregate, id)
	for _, ev := range stream {
		s, _ = d.Evolve(s, ev)
	}
	return s
}

func TestWithNoRetentionDutyARequestIsApprovedAndTheKeyIsDestroyed(t *testing.T) {
	g := newRig(t, AllowAllRetentionPolicy{})
	ctx := context.Background()
	_ = g.kms.EnsureKey(ctx, "s1")
	g.send("s1", RequestErasureCommand, "", "u:manager-1")
	g.settle()
	if got := g.types("s1"); !slices.Equal(got, []string{ErasureRequestedEvent, ErasureApprovedEvent, SubjectErasedEvent}) {
		t.Fatalf("events %v", got)
	}
	if !kms.IsSubjectErased(g.kms.EnsureKey(ctx, "s1")) {
		t.Fatal("the key wasn't destroyed")
	}
}

func TestAPeriodPolicyHoldsTheRequestThenTheReviewClosesItWhenThePeriodEnds(t *testing.T) {
	last := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	g := newRig(t, PeriodRetentionPolicy{Years: 6, Activity: fixedActivity(last), Reason: "payroll-tax"})
	ctx := context.Background()
	_ = g.kms.EnsureKey(ctx, "s1")
	g.send("s1", RequestErasureCommand, "", "")
	g.settle()
	held := g.state("s1")
	if held.Stage != StageHeld || held.Reason != "payroll-tax" || !held.ReviewAt.Equal(last.AddDate(6, 0, 0)) {
		t.Fatalf("held %+v", held)
	}
	// not due yet: nothing moves, the key is intact
	g.settle()
	if g.state("s1").Stage != StageHeld {
		t.Fatal("moved before its review date")
	}
	if err := g.kms.EnsureKey(ctx, "s1"); err != nil {
		t.Fatal("the key went early")
	}
	// the period ends: the review approves it, and the lifecycle closes itself
	g.now = last.AddDate(6, 0, 1)
	g.settle()
	if got := g.types("s1"); !slices.Equal(got, []string{ErasureRequestedEvent, ErasureHeldEvent, ErasureApprovedEvent, SubjectErasedEvent}) {
		t.Fatalf("events %v", got)
	}
}

func TestALegalHoldOverridesAPolicyThatWouldAllowAndReleasingItResumesTheRequest(t *testing.T) {
	g := newRig(t, AllowAllRetentionPolicy{})
	g.send("s1", PlaceLegalHoldCommand, `{"reason":"litigation"}`, "")
	g.send("s1", RequestErasureCommand, "", "")
	g.settle()
	held := g.state("s1")
	if held.Stage != StageHeld || held.Reason != "legal-hold" || !held.ReviewAt.IsZero() {
		t.Fatalf("held %+v", held)
	}
	g.now = g.now.AddDate(50, 0, 0) // no schedule releases it
	g.settle()
	if g.state("s1").Stage != StageHeld {
		t.Fatal("a schedule released a legal hold")
	}
	g.send("s1", ReleaseLegalHoldCommand, "", "")
	g.settle()
	if g.state("s1").Stage != StageErased {
		t.Fatalf("stage %s after release", g.state("s1").Stage)
	}
}

func TestAHoldPlacedAfterAPolicyHoldKeepsTheRequestFromBeingReviewedAway(t *testing.T) {
	g := newRig(t, PeriodRetentionPolicy{Years: 1, Activity: fixedActivity(epoch), Reason: "tax"})
	g.send("s1", RequestErasureCommand, "", "")
	g.settle()
	g.send("s1", PlaceLegalHoldCommand, `{"reason":"audit"}`, "")
	g.now = epoch.AddDate(5, 0, 0)
	g.settle() // the review comes due; the governor reads the stream and refuses to approve
	if g.state("s1").Stage != StageHeld {
		t.Fatalf("stage %s", g.state("s1").Stage)
	}
}

func TestARedeliveredRequestEventDoesNotApproveTwice(t *testing.T) {
	g := newRig(t, AllowAllRetentionPolicy{})
	g.send("s1", RequestErasureCommand, "", "")
	stream, _ := g.store.LoadStream(context.Background(), SubjectAggregate, "s1")
	for range 2 { // at-least-once delivery
		if err := g.governor.Apply(context.Background(), stream[0]); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, typ := range g.types("s1") {
		if typ == ErasureApprovedEvent {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("approved %d times", n)
	}
}

func TestASubjectThatMovedOnIsNotADeliveryFailure(t *testing.T) {
	g := newRig(t, AllowAllRetentionPolicy{})
	g.send("s1", RequestErasureCommand, "", "")
	stream, _ := g.store.LoadStream(context.Background(), SubjectAggregate, "s1")
	g.send("s1", EraseSubjectCommand, "", "") // erased directly, before the governor ruled
	if err := g.governor.Apply(context.Background(), stream[0]); err != nil {
		t.Fatalf("must not fail: %v", err)
	}
	if g.state("s1").Stage != StageErased {
		t.Fatal("not erased")
	}
}

func TestACompositePolicyHoldsUntilTheLatestDutyEnds(t *testing.T) {
	g := newRig(t, CompositeRetentionPolicy{
		PeriodRetentionPolicy{Years: 6, Activity: fixedActivity(epoch), Reason: "tax"},
		PeriodRetentionPolicy{Years: 7, Activity: fixedActivity(epoch), Reason: "employment"},
	})
	g.send("s1", RequestErasureCommand, "", "")
	g.settle()
	held := g.state("s1")
	if held.Reason != "tax, employment" || !held.ReviewAt.Equal(epoch.AddDate(7, 0, 0)) {
		t.Fatalf("held %+v", held)
	}
}

// The governor is an effect: it doesn't act on an imported request, which
// another deployment's governor ruled on.
func TestTheGovernorIsAnEffect(t *testing.T) {
	if !consumers.IsEffect(&ErasureGovernor{}) {
		t.Fatal("not an effect")
	}
}

// A refusal of what the governor sends (the subject moved on: a legal hold
// placed after the approval) is the current state winning, not a delivery
// failure; the key stays.
func TestARefusedDispatchIsNotADeliveryFailure(t *testing.T) {
	g := newRig(t, AllowAllRetentionPolicy{})
	g.send("s1", RequestErasureCommand, "", "")
	g.send("s1", ApproveErasureCommand, "", "admin")
	g.send("s1", PlaceLegalHoldCommand, `{"reason":"audit"}`, "admin")
	stream, _ := g.store.LoadStream(context.Background(), SubjectAggregate, "s1")
	if err := g.governor.Apply(context.Background(), stream[1]); err != nil { // ErasureApproved
		t.Fatalf("a refused EraseSubject failed delivery: %v", err)
	}
	if g.state("s1").Erased {
		t.Fatal("erased under a legal hold")
	}
}
