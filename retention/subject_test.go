package retention

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/pii"
)

// The governed lifecycle over the core data-subject aggregate, ported from
// dotnetcqrs's DataSubject with the same command and event names and the
// same cases (ErasureWorkflowTests, DataSubjectAcceptedCommandsTests).

func bareSubjects(t *testing.T) (*events.Store, *decider.Registry) {
	t.Helper()
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	r := decider.NewRegistry(store)
	RegisterDataSubjects(r)
	return store, r
}

func do(t *testing.T, r *decider.Registry, id, cmd, payload, actor string) ([]events.Event, error) {
	t.Helper()
	if payload == "" {
		payload = "{}"
	}
	if actor == "" {
		actor = "u"
	}
	return r.HandleWithMeta(context.Background(), pii.SubjectAggregate, id, decider.Command{Name: cmd, Payload: json.RawMessage(payload)},
		map[string]any{"actor": actor, "now": "2026-10-16T09:00:00.000Z"})
}

func must(t *testing.T) func([]events.Event, error) []events.Event {
	return func(evs []events.Event, err error) []events.Event {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return evs
	}
}

func refused(t *testing.T) func([]events.Event, error) {
	return func(_ []events.Event, err error) {
		t.Helper()
		if !decider.IsRejection(err) {
			t.Fatalf("want a refusal, got %v", err)
		}
	}
}

// The core's rules hold unchanged: the module adds to them, never relaxes
// them.
func TestTheCoreRequestAndErasureBehaveAsTheCoreDoes(t *testing.T) {
	store, r := bareSubjects(t)
	first := must(t)(do(t, r, "s1", pii.RequestErasureCommand, `{"reason":"asked"}`, "u:clerk-1"))
	if len(first) != 1 || first[0].Type != pii.ErasureRequestedEvent || !strings.Contains(string(first[0].Data), `"requestedBy":"u:clerk-1"`) {
		t.Fatalf("first: %+v", first)
	}
	if evs := must(t)(do(t, r, "s1", pii.RequestErasureCommand, "", "")); len(evs) != 0 {
		t.Fatal("asked twice, recorded twice")
	}
	_, err := do(t, r, "s2", pii.RequestErasureCommand, `{"reason":"`+strings.Repeat("x", pii.MaxReasonLength+1)+`"}`, "")
	if !decider.IsRejection(err) || !strings.Contains(err.Error(), "not personal data") {
		t.Fatalf("long reason: %v", err)
	}
	must(t)(do(t, r, "s3", pii.EraseSubjectCommand, "", ""))
	stream, _ := store.LoadStream(context.Background(), pii.SubjectAggregate, "s3")
	if len(stream) != 1 || string(stream[0].Data) != "{}" {
		t.Fatalf("SubjectErased must still carry no payload: %+v", stream)
	}
	if evs := must(t)(do(t, r, "s3", pii.RequestErasureCommand, "", "")); len(evs) != 0 {
		t.Fatal("a request after erasure was recorded")
	}
}

func TestHoldAndApproveNeedAnOpenRequest(t *testing.T) {
	_, r := bareSubjects(t)
	refused(t)(do(t, r, "s1", HoldErasureCommand, `{"reason":"x"}`, ""))
	refused(t)(do(t, r, "s1", ApproveErasureCommand, "", ""))
}

func TestAHoldNeedsAReasonAndCanBeRestatedWithoutDuplicatingTheEvent(t *testing.T) {
	store, r := bareSubjects(t)
	must(t)(do(t, r, "s1", pii.RequestErasureCommand, "", ""))
	refused(t)(do(t, r, "s1", HoldErasureCommand, "{}", ""))
	hold := `{"reason":"statutory-records","reviewAt":"2030-01-01T00:00:00Z"}`
	if evs := must(t)(do(t, r, "s1", HoldErasureCommand, hold, "")); len(evs) != 1 {
		t.Fatal("the hold wasn't recorded")
	}
	if evs := must(t)(do(t, r, "s1", HoldErasureCommand, hold, "")); len(evs) != 0 {
		t.Fatal("the same hold restated was recorded again")
	}
	if evs := must(t)(do(t, r, "s1", HoldErasureCommand, `{"reason":"statutory-records","reviewAt":"2031-01-01T00:00:00Z"}`, "")); len(evs) != 1 {
		t.Fatal("a new review date is news")
	}
	stream, _ := store.LoadStream(context.Background(), pii.SubjectAggregate, "s1")
	if len(stream) != 3 {
		t.Fatalf("stream %d", len(stream))
	}
}

func TestALegalHoldBlocksApprovalAndErasureUntilReleased(t *testing.T) {
	_, r := bareSubjects(t)
	must(t)(do(t, r, "s1", pii.RequestErasureCommand, "", ""))
	must(t)(do(t, r, "s1", PlaceLegalHoldCommand, `{"reason":"litigation"}`, ""))
	refused(t)(do(t, r, "s1", ApproveErasureCommand, "", ""))
	refused(t)(do(t, r, "s1", pii.EraseSubjectCommand, "", ""))
	if evs := must(t)(do(t, r, "s1", PlaceLegalHoldCommand, `{"reason":"again"}`, "")); len(evs) != 0 {
		t.Fatal("a second legal hold was recorded")
	}
	must(t)(do(t, r, "s1", ReleaseLegalHoldCommand, "", ""))
	if evs := must(t)(do(t, r, "s1", ApproveErasureCommand, "", "")); len(evs) != 1 {
		t.Fatal("not approved after release")
	}
	if evs := must(t)(do(t, r, "s1", ReleaseLegalHoldCommand, "", "")); len(evs) != 0 {
		t.Fatal("nothing to release, yet recorded")
	}
}

func TestNothingButANoOpIsPossibleAfterErasure(t *testing.T) {
	_, r := bareSubjects(t)
	must(t)(do(t, r, "s1", pii.EraseSubjectCommand, "", ""))
	if evs := must(t)(do(t, r, "s1", pii.EraseSubjectCommand, "", "")); len(evs) != 0 {
		t.Fatal("erased twice")
	}
	refused(t)(do(t, r, "s1", PlaceLegalHoldCommand, `{"reason":"x"}`, ""))
	refused(t)(do(t, r, "s1", ApproveErasureCommand, "", ""))
}

// A_subject_accepts_what_its_stage_and_legal_hold_allow, worked by hand.
var acceptedByHand = []struct {
	stage Stage
	hold  bool
	want  []string
}{
	{StageNone, false, []string{pii.RequestErasureCommand, PlaceLegalHoldCommand, pii.EraseSubjectCommand}},
	{StageRequested, false, []string{HoldErasureCommand, ApproveErasureCommand, PlaceLegalHoldCommand, pii.EraseSubjectCommand}},
	{StageRequested, true, []string{HoldErasureCommand, ReleaseLegalHoldCommand}},
	{StageHeld, false, []string{HoldErasureCommand, ApproveErasureCommand, PlaceLegalHoldCommand, pii.EraseSubjectCommand}},
	{StageHeld, true, []string{HoldErasureCommand, ReleaseLegalHoldCommand}},
	{StageApproved, false, []string{PlaceLegalHoldCommand, pii.EraseSubjectCommand}},
	{StageApproved, true, []string{ReleaseLegalHoldCommand}},
	{StageErased, false, nil},
}

func subjectAt(stage Stage, hold bool) State {
	return State{
		SubjectState: pii.SubjectState{Erased: stage == StageErased, Requested: stage != StageNone},
		Stage:        stage, LegalHold: hold, HoldReason: "earlier-reason",
	}
}

func TestASubjectAcceptsWhatItsStageAndLegalHoldAllow(t *testing.T) {
	for _, c := range acceptedByHand {
		got := AcceptedCommands(subjectAt(c.stage, c.hold))
		slices.Sort(got)
		want := slices.Clone(c.want)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s hold=%v: %v, want %v", c.stage, c.hold, got, want)
		}
	}
}

func TestExactlyTheAcceptedCommandsRecordAnEventWhenDecided(t *testing.T) {
	d := DataSubject()
	for _, c := range acceptedByHand {
		state := subjectAt(c.stage, c.hold)
		accepted := AcceptedCommands(state)
		for _, cmd := range Commands {
			evs, err := d.Decide(decider.Command{Name: cmd, Payload: json.RawMessage(`{"reason":"a-new-reason"}`),
				Actor: "admin-1", Now: "2026-10-14T09:00:00.000Z"}, state)
			records := err == nil && len(evs) > 0
			if records != slices.Contains(accepted, cmd) {
				t.Errorf("%s at %s hold=%v: records=%v, offered=%v", cmd, c.stage, c.hold, records, slices.Contains(accepted, cmd))
			}
		}
	}
}

func TestApprovingAnApprovedRequestAgainRecordsNothingAndIsNotRefused(t *testing.T) {
	evs, err := DataSubject().Decide(decider.Command{Name: ApproveErasureCommand, Payload: json.RawMessage(`{}`),
		Actor: "admin-1", Now: "2026-10-14T09:00:00.000Z"}, subjectAt(StageApproved, false))
	if err != nil || len(evs) != 0 {
		t.Fatalf("%v %v", evs, err)
	}
}

func TestAModuleCommandWithNoTimeIsRefused(t *testing.T) {
	for _, cmd := range []string{pii.RequestErasureCommand, HoldErasureCommand, PlaceLegalHoldCommand} {
		_, err := DataSubject().Decide(decider.Command{Name: cmd, Payload: json.RawMessage(`{"reason":"r"}`)}, subjectAt(StageRequested, false))
		if cmd == pii.RequestErasureCommand {
			_, err = DataSubject().Decide(decider.Command{Name: cmd, Payload: json.RawMessage(`{}`)}, subjectAt(StageNone, false))
		}
		if err == nil || !strings.Contains(err.Error(), "carries no time") {
			t.Errorf("%s: got %v", cmd, err)
		}
	}
}

func TestTheFoldTracksStageHoldAndReview(t *testing.T) {
	d := DataSubject()
	s := d.InitialState()
	for _, ev := range []events.Event{
		{Type: pii.ErasureRequestedEvent, Data: json.RawMessage(`{"requestedBy":"u:clerk-1","reason":"asked"}`)},
		{Type: ErasureHeldEvent, Data: json.RawMessage(`{"reason":"statutory-records","reviewAt":"2032-03-01T00:00:00+00:00"}`)},
		{Type: LegalHoldPlacedEvent, Data: json.RawMessage(`{"reason":"audit"}`)},
	} {
		s, _ = d.Evolve(s, ev)
	}
	if s.Stage != StageHeld || !s.LegalHold || s.HoldReason != "statutory-records" || s.Reason != "asked" ||
		s.RequestedBy != "u:clerk-1" || s.ReviewAt.Format("2006-01-02T15:04:05Z07:00") != "2032-03-01T00:00:00Z" {
		t.Fatalf("held: %+v", s)
	}
	for _, ev := range []events.Event{
		{Type: LegalHoldReleasedEvent, Data: json.RawMessage(`{}`)},
		{Type: ErasureApprovedEvent, Data: json.RawMessage(`{}`)},
		{Type: pii.SubjectErasedEvent, Data: json.RawMessage(`{}`)},
	} {
		s, _ = d.Evolve(s, ev)
	}
	if s.Stage != StageErased || !s.Erased || s.LegalHold {
		t.Fatalf("erased: %+v", s)
	}
}
