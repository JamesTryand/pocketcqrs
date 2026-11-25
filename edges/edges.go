// Package edges carries a coordinating document's internal edges
// (eventmodelschema's architecture.schema.json 0.2.0): the message that
// crosses between two systems, what a sending system publishes, and how the
// receiving system translates it into one of its own commands.
//
// It names no carrier. A host passes Publish to whatever sends messages (a
// federation publisher, say) and Translate to whatever receives them; which
// carrier, and its subjects and credentials, is surface tooling's (decision
// 0033). The tables of edges a system has are generated from the coordinating
// document (`schema import --architecture ... --system ...`); this package is
// the one implementation of what they mean, so the generator runs an edge's
// examples against exactly the code a host runs.
package edges

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

// Message is architecture.schema.json's edgeMessage: a public event at its
// public version, with only the fields some internal edge from it maps.
type Message struct {
	// ID is the source event's own id: a carrier may drop duplicates by it,
	// and a receiver derives its command's id from it.
	ID      string         `json:"id"`
	System  string         `json:"system"`
	Event   string         `json:"event"`
	Version int            `json:"version"`
	Data    map[string]any `json:"data"`
	// CorrelationID is the source event's correlation, when it has one.
	CorrelationID string `json:"correlationId,omitempty"`
}

// Out is one public event this system publishes for its internal edges.
type Out struct {
	// EventType is the event's type in this system's own store.
	EventType string
	// System is this system's document id: every message names its sender
	// so, whatever a view calls it. Event and Version name the public event.
	System  string
	Event   string
	Version int
	// Fields are the event's fields it publishes, sorted: its public marker's
	// list, or else those some edge from it maps. Nothing else of the event
	// leaves.
	Fields []string
}

// In is one internal edge into this system.
type In struct {
	// ID is the edge's id in the coordinating document.
	ID string
	// FromSystem, FromEvent and FromVersion name the source event: the
	// sender by its document id, as its messages name it.
	FromSystem  string
	FromEvent   string
	FromVersion int
	// Aggregate and Command are the local target: the aggregate whose stream
	// the command goes to, and the command's name.
	Aggregate string
	Command   string
	// TargetField is the source event's field the target stream id comes
	// from (rule 2), so a redelivery reaches the same stream.
	TargetField string
	// Fields maps each command field to the source event field it takes its
	// value from (rule 1).
	Fields map[string]string
}

// ErrNoTarget is returned by Translate when the message carries no value for
// the edge's target field: the command has nowhere to go, so it is dropped.
var ErrNoTarget = errors.New("edges: the message has no value for the target field")

// Publish returns the message ev becomes, or false when no internal edge
// starts from its type (it stays in this system).
func Publish(outs []Out, ev events.Event) (Message, bool, error) {
	for _, o := range outs {
		if o.EventType != ev.Type {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			return Message{}, false, fmt.Errorf("edges: event %s (%s) data: %w", ev.ID, ev.Type, err)
		}
		return Message{ID: ev.ID, System: o.System, Event: o.Event, Version: o.Version,
			Data: Only(data, o.Fields), CorrelationID: correlation(ev)}, true, nil
	}
	return Message{}, false, nil
}

// Only keeps the named fields of data: what a sender publishes.
func Only(data map[string]any, fields []string) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		if v, ok := data[f]; ok {
			out[f] = v
		}
	}
	return out
}

// Matches says whether m is this edge's source event, at its version.
func (e In) Matches(m Message) bool {
	return m.System == e.FromSystem && m.Event == e.FromEvent && m.Version == e.FromVersion
}

// Translate makes the command m becomes on this edge: its aggregate, the
// target stream id (from TargetField) and the command, whose payload holds
// only the mapped fields m carries. A message with no value for TargetField
// returns ErrNoTarget.
func (e In) Translate(m Message) (aggregate, id string, cmd decider.Command, err error) {
	id, err = TargetID(m.Data[e.TargetField])
	if err != nil {
		return "", "", decider.Command{}, err
	}
	payload := map[string]any{}
	for cmdField, evField := range e.Fields {
		if v, ok := m.Data[evField]; ok {
			payload[cmdField] = v
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", "", decider.Command{}, err
	}
	return e.Aggregate, id, decider.Command{Name: e.Command, Payload: raw}, nil
}

// TranslateFields returns the target id and the payload Translate would send,
// as a map: for checking an edge's examples.
func (e In) TranslateFields(m Message) (id string, fields map[string]any, err error) {
	_, id, cmd, err := e.Translate(m)
	if err != nil {
		return "", nil, err
	}
	fields = map[string]any{}
	return id, fields, json.Unmarshal(cmd.Payload, &fields)
}

// TargetID is a target field's value as a stream id: a non-empty string, or a
// number written plainly. Anything else has no target.
func TargetID(v any) (string, error) {
	switch t := v.(type) {
	case string:
		if t != "" {
			return t, nil
		}
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case json.Number:
		return t.String(), nil
	}
	return "", ErrNoTarget
}

// Find returns the edges in ins whose source is m's event.
func Find(ins []In, m Message) []In {
	var out []In
	for _, e := range ins {
		if e.Matches(m) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// correlation is ev's own correlation id, else ev itself is the root (as a
// reactor's command takes it).
func correlation(ev events.Event) string {
	var meta map[string]any
	if err := json.Unmarshal(ev.Metadata, &meta); err == nil {
		if corr, ok := meta["correlationId"].(string); ok && corr != "" {
			return corr
		}
	}
	return ev.ID
}
