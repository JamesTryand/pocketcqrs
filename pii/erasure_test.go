package pii

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

// The governed-erasure lifecycle (decision 3), ported from dotnetcqrs's
// DataSubject with the same command and event names and the same tests
// (ErasureWorkflowTests, DataSubjectAcceptedCommandsTests).

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
	return r.HandleWithMeta(context.Background(), SubjectAggregate, id, decider.Command{Name: cmd, Payload: json.RawMessage(payload)},
		map[string]any{"actor": actor})
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

func TestARequestRecordsWhoAskedAndWhyAndIsIdempotentWhileOpen(t *testing.T) {
	store, r := bareSubjects(t)
	first := must(t)(do(t, r, "s1", RequestErasureCommand, `{"reason":"data-subject-request"}`, "u:manager-1"))
	again := must(t)(do(t, r, "s1", RequestErasureCommand, "", ""))
	if len(first) != 1 || first[0].Type != ErasureRequestedEvent ||
		!strings.Contains(string(first[0].Data), `"requestedBy":"u:manager-1"`) ||
		!strings.Contains(string(first[0].Data), `"reason":"data-subject-request"`) {
		t.Fatalf("first: %+v", first)
	}
	if len(again) != 0 {
		t.Fatalf("again: %+v", again)
	}
	stream, _ := store.LoadStream(context.Background(), SubjectAggregate, "s1")
	if len(stream) != 1 {
		t.Fatalf("stream %d", len(stream))
	}
}

func TestAReasonIsRefusedWhenTooLongToBeACode(t *testing.T) {
	_, r := bareSubjects(t)
	_, err := do(t, r, "s1", RequestErasureCommand, `{"reason":"`+strings.Repeat("x", MaxReasonLength+1)+`"}`, "")
	if !decider.IsRejection(err) || !strings.Contains(err.Error(), "not personal data") {
		t.Fatalf("got %v", err)
	}
}

func TestHoldAndApproveNeedAnOpenRequest(t *testing.T) {
	_, r := bareSubjects(t)
	refused(t)(do(t, r, "s1", HoldErasureCommand, `{"reason":"x"}`, ""))
	refused(t)(do(t, r, "s1", ApproveErasureCommand, "", ""))
}

func TestAHoldNeedsAReasonAndCanBeRestatedWithoutDuplicatingTheEvent(t *testing.T) {
	store, r := bareSubjects(t)
	must(t)(do(t, r, "s1", RequestErasureCommand, "", ""))
	refused(t)(do(t, r, "s1", HoldErasureCommand, "{}", ""))
	hold := `{"reason":"payroll","reviewAt":"2030-01-01T00:00:00Z"}`
	if evs := must(t)(do(t, r, "s1", HoldErasureCommand, hold, "")); len(evs) != 1 {
		t.Fatal("the hold wasn't recorded")
	}
	if evs := must(t)(do(t, r, "s1", HoldErasureCommand, hold, "")); len(evs) != 0 {
		t.Fatal("the same hold restated was recorded again")
	}
	if evs := must(t)(do(t, r, "s1", HoldErasureCommand, `{"reason":"payroll","reviewAt":"2031-01-01T00:00:00Z"}`, "")); len(evs) != 1 {
		t.Fatal("a new review date is news")
	}
	stream, _ := store.LoadStream(context.Background(), SubjectAggregate, "s1")
	if len(stream) != 3 {
		t.Fatalf("stream %d", len(stream))
	}
}

func TestALegalHoldBlocksApprovalAndErasureUntilReleased(t *testing.T) {
	_, r := bareSubjects(t)
	must(t)(do(t, r, "s1", RequestErasureCommand, "", ""))
	must(t)(do(t, r, "s1", PlaceLegalHoldCommand, `{"reason":"litigation"}`, ""))
	refused(t)(do(t, r, "s1", ApproveErasureCommand, "", ""))
	refused(t)(do(t, r, "s1", EraseSubjectCommand, "", ""))
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
	must(t)(do(t, r, "s1", EraseSubjectCommand, "", ""))
	if evs := must(t)(do(t, r, "s1", EraseSubjectCommand, "", "")); len(evs) != 0 {
		t.Fatal("erased twice")
	}
	if evs := must(t)(do(t, r, "s1", RequestErasureCommand, "", "")); len(evs) != 0 {
		t.Fatal("a request after erasure was recorded")
	}
	refused(t)(do(t, r, "s1", PlaceLegalHoldCommand, `{"reason":"x"}`, ""))
	refused(t)(do(t, r, "s1", ApproveErasureCommand, "", ""))
}

func TestTheDirectEraseOverrideStillWorksForASubjectWithNoRequest(t *testing.T) {
	store, r := bareSubjects(t)
	if evs := must(t)(do(t, r, "s1", EraseSubjectCommand, "", "")); len(evs) != 1 {
		t.Fatal("not erased")
	}
	stream, _ := store.LoadStream(context.Background(), SubjectAggregate, "s1")
	if len(stream) != 1 || string(stream[0].Data) != "{}" {
		t.Fatalf("SubjectErased must still carry no payload: %+v", stream)
	}
}

func TestEveryCommandHasADefaultRolePolicyAndOnlyTheRequestIsOpenToAManager(t *testing.T) {
	for _, c := range SubjectCommands {
		if DefaultRequiredRoles(c) == nil {
			t.Errorf("%s has no default roles", c)
		}
	}
	if got := DefaultRequiredRoles(RequestErasureCommand); !slices.Equal(got, []string{"manager", "administrator"}) {
		t.Errorf("request: %v", got)
	}
	for _, c := range SubjectCommands {
		if c != RequestErasureCommand && !slices.Equal(DefaultRequiredRoles(c), []string{"administrator"}) {
			t.Errorf("%s: %v", c, DefaultRequiredRoles(c))
		}
	}
	if DefaultRequiredRoles("NotACommand") != nil {
		t.Error("an unknown command has roles")
	}
}

// A_subject_accepts_what_its_stage_and_legal_hold_allow, worked by hand.
var acceptedByHand = []struct {
	stage ErasureStage
	hold  bool
	want  []string
}{
	{StageNone, false, []string{RequestErasureCommand, PlaceLegalHoldCommand, EraseSubjectCommand}},
	{StageRequested, false, []string{HoldErasureCommand, ApproveErasureCommand, PlaceLegalHoldCommand, EraseSubjectCommand}},
	{StageRequested, true, []string{HoldErasureCommand, ReleaseLegalHoldCommand}},
	{StageHeld, false, []string{HoldErasureCommand, ApproveErasureCommand, PlaceLegalHoldCommand, EraseSubjectCommand}},
	{StageHeld, true, []string{HoldErasureCommand, ReleaseLegalHoldCommand}},
	{StageApproved, false, []string{PlaceLegalHoldCommand, EraseSubjectCommand}},
	{StageApproved, true, []string{ReleaseLegalHoldCommand}},
	{StageErased, false, nil},
}

func subjectAt(stage ErasureStage, hold bool) SubjectState {
	return SubjectState{Erased: stage == StageErased, Stage: stage, LegalHold: hold, Reason: "earlier-reason"}
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
		for _, cmd := range SubjectCommands {
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

// The clock arrives on the command; a command with none is refused, never
// given the current time.
func TestACommandWithNoTimeIsRefused(t *testing.T) {
	_, err := DataSubject().Decide(decider.Command{Name: RequestErasureCommand, Payload: json.RawMessage(`{}`)}, SubjectState{})
	if err == nil || !strings.Contains(err.Error(), "carries no time") {
		t.Fatalf("got %v", err)
	}
	_ = time.Now
}
