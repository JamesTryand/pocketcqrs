// Package retention governs erasure: whether a request to erase a data
// subject may proceed. It is opt-in, over package pii's core dataSubject
// aggregate (RequestErasure, EraseSubject), and only ever adds refusals to
// it.
//
// A request is not an erasure. Erasure is permanent, and a business is
// usually obliged to keep some records about a person for a time (GDPR
// Article 17(3) exempts what the law requires kept). So a request goes
// through a lifecycle: None -> Requested -> (Held <-> asked again) ->
// Approved -> Erased. A Policy, owned by the business, says whether it
// may proceed; a legal hold blocks approval and erasure whatever the
// stage. The Governor drives each request to a conclusion. Ported from
// dotnetcqrs's governed erasure, with the same command and event names.
//
// Who may send which command is the host's to declare in its gateway
// Authorize hook: typically a wider group may ask (RequestErasure) than may
// hold, approve, place or release a legal hold, or erase directly.
package retention

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/pii"
)

const (
	HoldErasureCommand      = "HoldErasure"
	ApproveErasureCommand   = "ApproveErasure"
	PlaceLegalHoldCommand   = "PlaceLegalHold"
	ReleaseLegalHoldCommand = "ReleaseLegalHold"

	ErasureHeldEvent       = "ErasureHeld"
	ErasureApprovedEvent   = "ErasureApproved"
	LegalHoldPlacedEvent   = "LegalHoldPlaced"
	LegalHoldReleasedEvent = "LegalHoldReleased"
)

// Commands is every command the governed aggregate accepts, the core's
// included.
var Commands = []string{pii.RequestErasureCommand, HoldErasureCommand, ApproveErasureCommand,
	PlaceLegalHoldCommand, ReleaseLegalHoldCommand, pii.EraseSubjectCommand}

// Stage is where a subject's erasure request stands.
type Stage string

const (
	StageNone      Stage = "None"
	StageRequested Stage = "Requested"
	StageHeld      Stage = "Held"
	StageApproved  Stage = "Approved"
	// StageErased is terminal.
	StageErased Stage = "Erased"
)

// State is the governed fold of a data subject's stream: the core's, plus
// the stage, the legal hold and the current hold.
type State struct {
	pii.SubjectState
	Stage     Stage
	LegalHold bool
	// HoldReason is why the request is held (empty unless Stage is Held).
	HoldReason string
	// ReviewAt is when a held request is looked at again (zero: not on a
	// schedule).
	ReviewAt time.Time
}

type verdict int

const (
	verdictRecords verdict = iota
	verdictIgnored
	verdictRefused
)

// judge is what a subject in state does with command, before reading its
// payload: records an event, ignores it (idempotent: nothing new), or
// refuses it, and why. Decide and AcceptedCommands both use it, so a screen
// never offers what the subject would refuse or ignore.
func judge(state State, command string) (verdict, string) {
	const alreadyErased = "the subject has already been erased"
	open := state.Stage == StageRequested || state.Stage == StageHeld
	switch command {
	case pii.EraseSubjectCommand:
		switch {
		case state.Erased:
			return verdictIgnored, "" // a retried erasure is harmless
		case state.LegalHold:
			return verdictRefused, "a legal hold is in place; the subject cannot be erased"
		}
		return verdictRecords, ""
	case pii.RequestErasureCommand:
		if state.Erased || state.Requested {
			return verdictIgnored, "" // idempotent while one is open, meaningless after erasure
		}
		return verdictRecords, ""
	case HoldErasureCommand:
		switch {
		case state.Erased:
			return verdictRefused, alreadyErased
		case !open:
			return verdictRefused, "there is no open erasure request to hold"
		}
		return verdictRecords, ""
	case ApproveErasureCommand:
		switch {
		case state.Erased:
			return verdictRefused, alreadyErased
		case state.Stage == StageApproved:
			return verdictIgnored, ""
		case !open:
			return verdictRefused, "there is no open erasure request to approve"
		case state.LegalHold:
			return verdictRefused, "a legal hold is in place; the request cannot be approved"
		}
		return verdictRecords, ""
	case PlaceLegalHoldCommand:
		switch {
		case state.Erased:
			return verdictRefused, alreadyErased
		case state.LegalHold:
			return verdictIgnored, ""
		}
		return verdictRecords, ""
	case ReleaseLegalHoldCommand:
		if !state.LegalHold {
			return verdictIgnored, ""
		}
		return verdictRecords, ""
	}
	return verdictRefused, "unknown command: " + command
}

// AcceptedCommands is the commands a subject in state would record an event
// for: what a screen may offer, before the caller's role is considered. Only
// what depends on the payload is left out (a hold restated with the same
// reason and review date records nothing; a reason must be short).
func AcceptedCommands(state State) []string {
	var out []string
	for _, c := range Commands {
		if v, _ := judge(state, c); v == verdictRecords {
			out = append(out, c)
		}
	}
	return out
}

// DataSubject is the governed pure decider: the core's two commands, after
// this package's refusals, and the four that govern them.
func DataSubject() *decider.Decider[State] {
	return &decider.Decider[State]{
		Commands:     slices.Clone(Commands),
		InitialState: func() State { return State{Stage: StageNone} },
		Decide:       decide,
		Evolve:       evolve,
	}
}

// RegisterDataSubjects registers the governed aggregate in place of the
// core's. pii.Wire keeps it, whichever is wired first.
func RegisterDataSubjects(r *decider.Registry) {
	decider.Register(r, pii.SubjectAggregate, DataSubject())
}

type payload struct {
	Reason   string     `json:"reason"`
	ReviewAt *time.Time `json:"reviewAt"`
}

func decide(cmd decider.Command, state State) ([]events.NewEvent, error) {
	v, why := judge(state, cmd.Name)
	switch v {
	case verdictRefused:
		return nil, errors.New(why)
	case verdictIgnored:
		return nil, nil
	}
	if cmd.Name == pii.RequestErasureCommand || cmd.Name == pii.EraseSubjectCommand {
		return pii.DecideSubject(cmd, state.SubjectState)
	}
	if cmd.Now == "" {
		return nil, errors.New("the command carries no time (Command.Now)")
	}
	var p payload
	if len(cmd.Payload) > 0 {
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return nil, fmt.Errorf("payload: %w", err)
		}
	}
	var ev events.NewEvent
	var err error
	switch cmd.Name {
	case HoldErasureCommand:
		reason, rerr := pii.CheckReason(p.Reason, true)
		if rerr != nil {
			return nil, rerr
		}
		var review time.Time
		if p.ReviewAt != nil {
			review = p.ReviewAt.UTC()
		}
		// the same hold restated (a redelivered event, a policy re-run)
		if state.Stage == StageHeld && state.HoldReason == reason && state.ReviewAt.Equal(review) {
			return nil, nil
		}
		fields := map[string]any{"reason": reason, "heldBy": cmd.Actor, "heldAt": cmd.Now}
		if !review.IsZero() {
			fields["reviewAt"] = review.Format(time.RFC3339)
		}
		ev, err = pii.SubjectEvent(ErasureHeldEvent, fields)
	case ApproveErasureCommand:
		ev, err = pii.SubjectEvent(ErasureApprovedEvent, map[string]any{"approvedBy": cmd.Actor, "approvedAt": cmd.Now})
	case PlaceLegalHoldCommand:
		reason, rerr := pii.CheckReason(p.Reason, true)
		if rerr != nil {
			return nil, rerr
		}
		ev, err = pii.SubjectEvent(LegalHoldPlacedEvent, map[string]any{"reason": reason, "placedBy": cmd.Actor, "placedAt": cmd.Now})
	case ReleaseLegalHoldCommand:
		ev, err = pii.SubjectEvent(LegalHoldReleasedEvent, map[string]any{"releasedBy": cmd.Actor, "releasedAt": cmd.Now})
	}
	if err != nil {
		return nil, err
	}
	return []events.NewEvent{ev}, nil
}

func evolve(state State, ev events.Event) (State, error) {
	core, err := pii.EvolveSubject(state.SubjectState, ev)
	if err != nil {
		return state, err
	}
	state.SubjectState = core
	switch ev.Type {
	case pii.ErasureRequestedEvent:
		state.Stage = StageRequested
	case ErasureHeldEvent:
		var p payload
		_ = json.Unmarshal(ev.Data, &p)
		state.Stage, state.HoldReason, state.ReviewAt = StageHeld, p.Reason, time.Time{}
		if p.ReviewAt != nil {
			state.ReviewAt = p.ReviewAt.UTC()
		}
	case ErasureApprovedEvent:
		state.Stage = StageApproved
	case LegalHoldPlacedEvent:
		state.LegalHold = true
	case LegalHoldReleasedEvent:
		state.LegalHold = false
	case pii.SubjectErasedEvent:
		state.Stage = StageErased
	}
	return state, nil
}
