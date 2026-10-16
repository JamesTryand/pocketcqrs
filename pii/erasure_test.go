package pii

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

// The core data-subject aggregate: a request, and the erasure. What decides
// a request (retention duties, legal holds) is a module's, not this
// package's: see package retention.

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

func TestARequestRecordsWhoAskedAndWhyAndIsIdempotentWhileOpen(t *testing.T) {
	store, r := bareSubjects(t)
	first := must(t)(do(t, r, "s1", RequestErasureCommand, `{"reason":"data-subject-request"}`, "u:clerk-1"))
	again := must(t)(do(t, r, "s1", RequestErasureCommand, "", ""))
	if len(first) != 1 || first[0].Type != ErasureRequestedEvent ||
		!strings.Contains(string(first[0].Data), `"requestedBy":"u:clerk-1"`) ||
		!strings.Contains(string(first[0].Data), `"reason":"data-subject-request"`) ||
		!strings.Contains(string(first[0].Data), `"requestedAt":"2026-10-16T09:00:00.000Z"`) {
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
	if _, err := CheckReason("  "+strings.Repeat("é", MaxReasonLength)+" ", true); err != nil {
		t.Fatalf("200 characters, trimmed, must pass: %v", err)
	}
	if _, err := CheckReason(" ", true); err == nil {
		t.Fatal("a required reason that is only white space must be refused")
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
}

func TestTheDirectEraseWorksForASubjectWithNoRequest(t *testing.T) {
	store, r := bareSubjects(t)
	if evs := must(t)(do(t, r, "s1", EraseSubjectCommand, "", "")); len(evs) != 1 {
		t.Fatal("not erased")
	}
	stream, _ := store.LoadStream(context.Background(), SubjectAggregate, "s1")
	if len(stream) != 1 || string(stream[0].Data) != "{}" {
		t.Fatalf("SubjectErased must still carry no payload: %+v", stream)
	}
}

// The core aggregate has the request and the erasure, nothing else: a
// workflow over them (hold, approve, legal hold) is a module's.
func TestTheCoreAggregateKnowsOnlyTheRequestAndTheErasure(t *testing.T) {
	if !slices.Equal(SubjectCommands, []string{RequestErasureCommand, EraseSubjectCommand}) {
		t.Fatalf("commands %v", SubjectCommands)
	}
	_, r := bareSubjects(t)
	if got := r.Commands(SubjectAggregate); !slices.Equal(got, []string{EraseSubjectCommand, RequestErasureCommand}) {
		t.Fatalf("registered %v", got)
	}
	must(t)(do(t, r, "s1", RequestErasureCommand, "", ""))
	if _, err := do(t, r, "s1", "HoldErasure", `{"reason":"x"}`, ""); err == nil {
		t.Fatal("the core aggregate accepted a module's command")
	}
	if evs := must(t)(do(t, r, "s1", EraseSubjectCommand, "", "")); len(evs) != 1 {
		t.Fatal("an open request must not stop the erasure")
	}
}

func TestASubjectAcceptsWhatItsStateAllows(t *testing.T) {
	for _, c := range []struct {
		state SubjectState
		want  []string
	}{
		{SubjectState{}, []string{RequestErasureCommand, EraseSubjectCommand}},
		{SubjectState{Requested: true}, []string{EraseSubjectCommand}},
		{SubjectState{Requested: true, Erased: true}, nil},
		{SubjectState{Erased: true}, nil},
	} {
		if got := AcceptedCommands(c.state); !slices.Equal(got, c.want) {
			t.Errorf("%+v: %v, want %v", c.state, got, c.want)
		}
		for _, cmd := range SubjectCommands {
			evs, err := DecideSubject(decider.Command{Name: cmd, Payload: json.RawMessage(`{}`), Actor: "a", Now: "2026-10-16T09:00:00.000Z"}, c.state)
			if records := err == nil && len(evs) > 0; records != slices.Contains(c.want, cmd) {
				t.Errorf("%s at %+v: records=%v", cmd, c.state, records)
			}
		}
	}
}

func TestTheFoldTracksTheRequestAndTheErasure(t *testing.T) {
	s, _ := EvolveSubject(SubjectState{}, events.Event{Type: ErasureRequestedEvent,
		Data: json.RawMessage(`{"requestedBy":"u:clerk-1","reason":"asked"}`)})
	if !s.Requested || s.RequestedBy != "u:clerk-1" || s.Reason != "asked" || s.Erased {
		t.Fatalf("requested: %+v", s)
	}
	s, _ = EvolveSubject(s, events.Event{Type: "ErasureHeld", Data: json.RawMessage(`{"reason":"other"}`)})
	if s.Reason != "asked" {
		t.Fatalf("a module's event changed the core state: %+v", s)
	}
	s, _ = EvolveSubject(s, events.Event{Type: SubjectErasedEvent, Data: json.RawMessage(`{}`)})
	if !s.Erased {
		t.Fatalf("erased: %+v", s)
	}
}

// The clock arrives on the command; a command with none is refused, never
// given the current time. The erasure carries no payload, so needs none.
func TestACommandWithNoTimeIsRefused(t *testing.T) {
	_, err := DataSubject().Decide(decider.Command{Name: RequestErasureCommand, Payload: json.RawMessage(`{}`)}, SubjectState{})
	if err == nil || !strings.Contains(err.Error(), "carries no time") {
		t.Fatalf("got %v", err)
	}
	if evs, err := DataSubject().Decide(decider.Command{Name: EraseSubjectCommand}, SubjectState{}); err != nil || len(evs) != 1 {
		t.Fatalf("erase: %v %v", evs, err)
	}
}
