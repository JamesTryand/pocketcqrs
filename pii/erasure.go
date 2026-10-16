package pii

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

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
// The core has two commands. RequestErasure records that someone asked;
// EraseSubject records the erasure, and the KeyDestroyer turns it into a
// destroyed key. A request is not an erasure: what decides one (retention
// duties, legal holds, review dates) belongs to a module, such as package
// retention, which registers its own dataSubject decider over this one and
// only ever adds refusals to it. With no module, nothing acts on a request
// until the host does.
//
// A reason is stored in the clear on the subject's own stream and survives
// the erasure (the stream is never rewritten), so it is a short code or
// non-identifying text, never personal data; over MaxReasonLength characters
// is refused. SubjectErased carries no payload.
//
// Erased is terminal. A person who is erased and later returns is a NEW
// subject with a new id and a new key, never a reactivation: reusing the id
// gains nothing (a destroyed key's material is gone, so a new key of the
// same name can't read the old ciphertext) and would undo the erasure by
// reconnecting a live identity to the history erasure severed. Subject ids
// are therefore opaque, never derived from personal data, never reused.
const (
	SubjectAggregate = "dataSubject"

	RequestErasureCommand = "RequestErasure"
	EraseSubjectCommand   = "EraseSubject"

	ErasureRequestedEvent = "ErasureRequested"
	SubjectErasedEvent    = "SubjectErased"

	// MaxReasonLength is the longest reason accepted.
	MaxReasonLength = 200
)

// SubjectCommands is every command the core aggregate accepts.
var SubjectCommands = []string{RequestErasureCommand, EraseSubjectCommand}

// SubjectState is the core fold of a data subject's stream. A module's
// events leave it unchanged.
type SubjectState struct {
	Erased bool
	// Requested: an erasure has been asked for. It stays true; only the
	// erasure closes a request.
	Requested   bool
	RequestedBy string
	Reason      string
}

// AcceptedCommands is the core commands a subject in state would record an
// event for: what a screen may offer, before the caller's role is
// considered.
func AcceptedCommands(state SubjectState) []string {
	var out []string
	if !state.Erased && !state.Requested {
		out = append(out, RequestErasureCommand)
	}
	if !state.Erased {
		out = append(out, EraseSubjectCommand)
	}
	return out
}

// CheckReason trims a reason and refuses one that is missing (when
// required) or too long to be a code.
func CheckReason(reason string, required bool) (string, error) {
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

// SubjectEvent is a dataSubject event with payload's empty values left out.
func SubjectEvent(typ string, payload map[string]any) (events.NewEvent, error) {
	for k, v := range payload {
		if v == nil || v == "" {
			delete(payload, k)
		}
	}
	data, err := json.Marshal(payload)
	return events.NewEvent{Type: typ, Data: data}, err
}

// DataSubject is the core's pure decider. The stream id IS the subject id,
// so nothing in the payload names it, and no personal data reaches this
// aggregate. Time comes on the command (Command.Now); a request with none is
// refused rather than given the current time.
func DataSubject() *decider.Decider[SubjectState] {
	return &decider.Decider[SubjectState]{
		Commands:     slices.Clone(SubjectCommands),
		InitialState: func() SubjectState { return SubjectState{} },
		Decide:       DecideSubject,
		Evolve:       EvolveSubject,
	}
}

// RegisterDataSubjects registers the core aggregate. A host that stores any
// personal data needs it (Wire does it), or nothing can ever be erased.
func RegisterDataSubjects(r *decider.Registry) {
	decider.Register(r, SubjectAggregate, DataSubject())
}

// DecideSubject decides a core command. A module deciding the same aggregate
// calls it for these two once its own refusals have passed.
func DecideSubject(cmd decider.Command, state SubjectState) ([]events.NewEvent, error) {
	switch cmd.Name {
	case EraseSubjectCommand:
		if state.Erased {
			return nil, nil // a retried erasure is harmless
		}
		return []events.NewEvent{{Type: SubjectErasedEvent, Data: json.RawMessage(`{}`)}}, nil
	case RequestErasureCommand:
		if state.Erased || state.Requested {
			return nil, nil // idempotent while one is open, meaningless after erasure
		}
		if cmd.Now == "" {
			return nil, errors.New("the command carries no time (Command.Now)")
		}
		var p struct {
			Reason string `json:"reason"`
		}
		if len(cmd.Payload) > 0 {
			if err := json.Unmarshal(cmd.Payload, &p); err != nil {
				return nil, fmt.Errorf("payload: %w", err)
			}
		}
		reason, err := CheckReason(p.Reason, false)
		if err != nil {
			return nil, err
		}
		ev, err := SubjectEvent(ErasureRequestedEvent, map[string]any{"requestedBy": cmd.Actor, "reason": reason, "requestedAt": cmd.Now})
		if err != nil {
			return nil, err
		}
		return []events.NewEvent{ev}, nil
	}
	return nil, errors.New("unknown command: " + cmd.Name)
}

// EvolveSubject folds a core event; any other event leaves state unchanged.
func EvolveSubject(state SubjectState, ev events.Event) (SubjectState, error) {
	switch ev.Type {
	case ErasureRequestedEvent:
		var p struct {
			RequestedBy string `json:"requestedBy"`
			Reason      string `json:"reason"`
		}
		_ = json.Unmarshal(ev.Data, &p)
		state.Requested, state.RequestedBy, state.Reason = true, p.RequestedBy, p.Reason
	case SubjectErasedEvent:
		state.Erased = true
	}
	return state, nil
}
