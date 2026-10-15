package pii

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

// The built-in data-subject aggregate. Erasure isn't a property of any
// domain aggregate that happens to mention a person: an order's stream holds
// the customer's email, but the customer outlives the order and is erased
// independently of it. So the subject gets its own stream (id = subject id),
// and every reader learns of an erasure the way it learns anything else:
// from an event. Deliberately not modelled in eventmodelschema — erasure is
// a runtime concern, not something a document describes.
//
// Erasure is governed, not automatic (decision 3; dotnetcqrs's DataSubject,
// with the same command and event names): a request is not an erasure. The
// lifecycle is None -> Requested -> (Held <-> Requested) -> Approved ->
// Erased, and a legal hold blocks approval and erasure whatever the stage.
// Whether a request may be approved is a retention-policy question
// (RetentionPolicy), answered outside this pure decider by ErasureGovernor.
//
// Reasons are stored in the clear on the subject's own stream and survive
// the erasure (the stream is never rewritten), so a reason is a short code
// or non-identifying text, never personal data; over MaxReasonLength
// characters is verdictRefused. EraseSubject stays as the administrator's direct
// override (and the governor's last step); it is verdictRefused while a legal hold
// stands. SubjectErased still carries no payload, so a stream written before
// this lifecycle still reads.
//
// Erased is terminal. A person who is erased and later returns is a NEW
// subject with a new id and a new key, never a reactivation: reusing the id
// gains nothing (a destroyed key's material is gone, so a new key of the
// same name can't read the old ciphertext) and would undo the erasure by
// reconnecting a live identity to the history erasure severed. Subject ids
// are therefore opaque, never derived from personal data, never reused.
const (
	SubjectAggregate = "dataSubject"

	RequestErasureCommand   = "RequestErasure"
	HoldErasureCommand      = "HoldErasure"
	ApproveErasureCommand   = "ApproveErasure"
	PlaceLegalHoldCommand   = "PlaceLegalHold"
	ReleaseLegalHoldCommand = "ReleaseLegalHold"
	EraseSubjectCommand     = "EraseSubject"

	ErasureRequestedEvent  = "ErasureRequested"
	ErasureHeldEvent       = "ErasureHeld"
	ErasureApprovedEvent   = "ErasureApproved"
	LegalHoldPlacedEvent   = "LegalHoldPlaced"
	LegalHoldReleasedEvent = "LegalHoldReleased"
	SubjectErasedEvent     = "SubjectErased"

	// MaxReasonLength is the longest reason accepted.
	MaxReasonLength = 200
)

// SubjectCommands is every command the aggregate accepts, for hosts that
// declare policies in a loop.
var SubjectCommands = []string{RequestErasureCommand, HoldErasureCommand, ApproveErasureCommand,
	PlaceLegalHoldCommand, ReleaseLegalHoldCommand, EraseSubjectCommand}

// DefaultRequiredRoles is who should be allowed to send each command, for a
// host whose roles are "manager" and "administrator": anyone who runs the
// roster may ask; only an administrator may hold, approve, place or release
// a legal hold, or erase directly. Nil for a command the aggregate doesn't
// have. The gateway refuses every dataSubject command while no Authorize
// hook is wired; a hook that is wired must cover every one of these, since
// erasure can't be undone.
func DefaultRequiredRoles(command string) []string {
	switch command {
	case RequestErasureCommand:
		return []string{"manager", "administrator"}
	case HoldErasureCommand, ApproveErasureCommand, PlaceLegalHoldCommand, ReleaseLegalHoldCommand, EraseSubjectCommand:
		return []string{"administrator"}
	}
	return nil
}

// ErasureStage is where a subject's erasure request stands.
type ErasureStage string

const (
	StageNone      ErasureStage = "None"
	StageRequested ErasureStage = "Requested"
	StageHeld      ErasureStage = "Held"
	StageApproved  ErasureStage = "Approved"
	// StageErased is terminal.
	StageErased ErasureStage = "Erased"
)

// SubjectState is the folded state of a data subject's stream.
type SubjectState struct {
	Erased      bool
	Stage       ErasureStage
	LegalHold   bool
	RequestedBy string
	Reason      string
	// ReviewAt is when a held request is looked at again (zero: not on a
	// schedule).
	ReviewAt time.Time
}

type subjectPayload struct {
	RequestedBy string     `json:"requestedBy,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	ReviewAt    *time.Time `json:"reviewAt,omitempty"`
}

type verdict int

const (
	verdictRecords verdict = iota
	verdictIgnored
	verdictRefused
)

// judge is what a subject in state does with command, before reading its
// payload: verdictRecords an event, ignores it (idempotent: nothing new), or
// refuses it, and why. Decide and AcceptedCommands both use it, so a screen
// never offers what the subject would refuse or ignore.
func judge(state SubjectState, command string) (verdict, string) {
	const alreadyErased = "the subject has already been erased"
	open := state.Stage == StageRequested || state.Stage == StageHeld
	switch command {
	case EraseSubjectCommand:
		switch {
		case state.Erased:
			return verdictIgnored, "" // a retried erasure is harmless
		case state.LegalHold:
			return verdictRefused, "a legal hold is in place; the subject cannot be erased"
		}
		return verdictRecords, ""
	case RequestErasureCommand:
		if state.Erased || (state.Stage != "" && state.Stage != StageNone) {
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
// reason and review date verdictRecords nothing; a reason must be short).
func AcceptedCommands(state SubjectState) []string {
	var out []string
	for _, c := range SubjectCommands {
		if v, _ := judge(state, c); v == verdictRecords {
			out = append(out, c)
		}
	}
	return out
}

func checkedReason(reason string, required bool) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		if required {
			return "", errors.New("a reason is required")
		}
		return "", nil
	}
	if len([]rune(reason)) > MaxReasonLength {
		return "", fmt.Errorf("reason is limited to %d characters; use a short code, not personal data", MaxReasonLength)
	}
	return reason, nil
}

func subjectEvent(typ string, payload map[string]any) (events.NewEvent, error) {
	for k, v := range payload {
		if v == nil || v == "" {
			delete(payload, k)
		}
	}
	data, err := json.Marshal(payload)
	return events.NewEvent{Type: typ, Data: data}, err
}

// DataSubject is the pure decider. The stream id IS the subject id, so
// nothing in the payload names it, and no personal data reaches this
// aggregate. Time comes on the command (Command.Now); a command with none is
// verdictRefused rather than given the current time.
func DataSubject() *decider.Decider[SubjectState] {
	return &decider.Decider[SubjectState]{
		Commands:     slices.Clone(SubjectCommands),
		InitialState: func() SubjectState { return SubjectState{Stage: StageNone} },
		Decide:       decideSubject,
		Evolve:       evolveSubject,
	}
}

func decideSubject(cmd decider.Command, state SubjectState) ([]events.NewEvent, error) {
	v, why := judge(state, cmd.Name)
	switch v {
	case verdictRefused:
		return nil, errors.New(why)
	case verdictIgnored:
		return nil, nil
	}
	if cmd.Name == EraseSubjectCommand {
		return []events.NewEvent{{Type: SubjectErasedEvent, Data: json.RawMessage(`{}`)}}, nil
	}
	if cmd.Now == "" {
		return nil, errors.New("the command carries no time (Command.Now)")
	}
	var p subjectPayload
	if len(cmd.Payload) > 0 {
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return nil, fmt.Errorf("payload: %w", err)
		}
	}
	var ev events.NewEvent
	var err error
	switch cmd.Name {
	case RequestErasureCommand:
		reason, rerr := checkedReason(p.Reason, false)
		if rerr != nil {
			return nil, rerr
		}
		ev, err = subjectEvent(ErasureRequestedEvent, map[string]any{"requestedBy": cmd.Actor, "reason": reason, "requestedAt": cmd.Now})
	case HoldErasureCommand:
		reason, rerr := checkedReason(p.Reason, true)
		if rerr != nil {
			return nil, rerr
		}
		var review time.Time
		if p.ReviewAt != nil {
			review = p.ReviewAt.UTC()
		}
		// the same hold restated (a redelivered event, a policy re-run)
		if state.Stage == StageHeld && state.Reason == reason && state.ReviewAt.Equal(review) {
			return nil, nil
		}
		payload := map[string]any{"reason": reason, "heldBy": cmd.Actor, "heldAt": cmd.Now}
		if !review.IsZero() {
			payload["reviewAt"] = review.Format(time.RFC3339)
		}
		ev, err = subjectEvent(ErasureHeldEvent, payload)
	case ApproveErasureCommand:
		ev, err = subjectEvent(ErasureApprovedEvent, map[string]any{"approvedBy": cmd.Actor, "approvedAt": cmd.Now})
	case PlaceLegalHoldCommand:
		reason, rerr := checkedReason(p.Reason, true)
		if rerr != nil {
			return nil, rerr
		}
		ev, err = subjectEvent(LegalHoldPlacedEvent, map[string]any{"reason": reason, "placedBy": cmd.Actor, "placedAt": cmd.Now})
	case ReleaseLegalHoldCommand:
		ev, err = subjectEvent(LegalHoldReleasedEvent, map[string]any{"releasedBy": cmd.Actor, "releasedAt": cmd.Now})
	default:
		return nil, errors.New("unknown command: " + cmd.Name) // judge verdictRefused it already
	}
	if err != nil {
		return nil, err
	}
	return []events.NewEvent{ev}, nil
}

func evolveSubject(state SubjectState, ev events.Event) (SubjectState, error) {
	read := func() subjectPayload {
		var p subjectPayload
		_ = json.Unmarshal(ev.Data, &p)
		return p
	}
	switch ev.Type {
	case ErasureRequestedEvent:
		p := read()
		state.Stage, state.RequestedBy, state.Reason, state.ReviewAt = StageRequested, p.RequestedBy, p.Reason, time.Time{}
	case ErasureHeldEvent:
		p := read()
		state.Stage, state.Reason, state.ReviewAt = StageHeld, p.Reason, time.Time{}
		if p.ReviewAt != nil {
			state.ReviewAt = p.ReviewAt.UTC()
		}
	case ErasureApprovedEvent:
		state.Stage = StageApproved
	case LegalHoldPlacedEvent:
		state.LegalHold = true
	case LegalHoldReleasedEvent:
		state.LegalHold = false
	case SubjectErasedEvent:
		state.Erased, state.Stage = true, StageErased
	}
	return state, nil
}
