package emschema

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/jamestryand/pocketcqrs/scaffold"
)

// Outcome is one alternative of a slice's outcomes: the events emitted
// together. Schema 3.3.0 writes it as a list of event ids; 3.12.0 also as
// an object, adding when (a list of {all: [conditions]} groups, or "hand")
// and set (event id -> field -> literal); 3.13.0 adds repeated (events
// emitted zero or more times, one per item, their items hand-written).
type Outcome struct {
	EventIDs    []string                              `json:"eventIds"`
	Description string                                `json:"description,omitempty"`
	When        json.RawMessage                       `json:"when,omitempty"`
	Set         map[string]map[string]json.RawMessage `json:"set,omitempty"`
	Repeated    []string                              `json:"repeated,omitempty"`
}

// UnmarshalJSON reads either form.
func (o *Outcome) UnmarshalJSON(data []byte) error {
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("[")) {
		*o = Outcome{}
		return json.Unmarshal(data, &o.EventIDs)
	}
	type plain Outcome
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*o = Outcome(p)
	return nil
}

// MarshalJSON writes the 3.3.0 list form when that is all there is.
func (o Outcome) MarshalJSON() ([]byte, error) {
	if o.Description == "" && len(o.When) == 0 && len(o.Set) == 0 && len(o.Repeated) == 0 {
		if o.EventIDs == nil {
			return []byte("[]"), nil
		}
		return json.Marshal(o.EventIDs)
	}
	type plain Outcome
	return json.Marshal(plain(o))
}

// mapOutcomes turns a slice's outcomes into the scaffold's, checked as
// dotnetcqrs's mapper checks them: every id belongs to the slice's events;
// the alternatives are tried in order, so every one but the last says when
// it applies and the last (the default) doesn't; several alternatives with
// no when at all (3.3.0's form) say what may happen but not which, so they
// are refused; set names this alternative's own, non-repeated events and
// their fields, with literals of the field's type; a repeated event carries
// fields (what tells one item from the next). Condition fields are checked
// by the generator (scaffold Domain.Validate), as refusals' are.
func (m *mapper) mapOutcomes(s Slice, eventIDs []string, events []scaffold.Event) []scaffold.Outcome {
	if len(s.Outcomes) == 0 {
		return nil
	}
	where := func(i int) string { return fmt.Sprintf("slice %q outcome %d", s.ID, i) }
	allBare := true
	for _, o := range s.Outcomes {
		allBare = allBare && len(o.When) == 0
	}
	if len(s.Outcomes) > 1 && allBare {
		m.rep.errorf("slice %q lists %d outcomes but not which applies (schema 3.3.0); say when each applies "+
			"with when (schema 3.12.0), or \"hand\" for a hand-written condition", s.ID, len(s.Outcomes))
		return nil
	}
	eventOf := map[string]scaffold.Event{}
	for _, e := range events {
		eventOf[e.Name] = e
	}
	var out []scaffold.Outcome
	for i, o := range s.Outcomes {
		last := i == len(s.Outcomes)-1
		switch {
		case !allBare && last && len(o.When) > 0:
			m.rep.errorf("%s: the last outcome is the default and must have no when", where(i))
		case !allBare && !last && len(o.When) == 0:
			m.rep.errorf("%s needs a when: only the last outcome may have none", where(i))
		}
		built := scaffold.Outcome{}
		for _, id := range o.EventIDs {
			if !contains(eventIDs, id) {
				m.rep.errorf("%s names event %q, which is not one of the slice's events", where(i), id)
				continue
			}
			built.Events = append(built.Events, m.eventType(id))
		}
		for _, id := range o.Repeated {
			if !contains(o.EventIDs, id) {
				m.rep.errorf("%s: repeated names event %q, which is not one of this outcome's events", where(i), id)
				continue
			}
			if len(eventOf[m.eventType(id)].Fields) == 0 {
				m.rep.errorf("%s: repeated event %q carries no fields, so nothing tells one item from the next", where(i), id)
			}
			built.Repeated = append(built.Repeated, m.eventType(id))
		}
		if len(o.When) > 0 {
			var hand string
			if json.Unmarshal(o.When, &hand) == nil {
				if hand != "hand" {
					m.rep.errorf("%s: when is a list of {all: [...]} groups, or \"hand\"; got %q", where(i), hand)
				}
				built.ByHand = true
			} else {
				groups, err := outcomeGroups(o.When)
				if err != nil {
					m.rep.errorf("%s: when: %v", where(i), err)
				}
				built.When = groups
			}
		}
		if len(o.Set) > 0 {
			built.Set = map[string]map[string]any{}
			for id, fields := range o.Set {
				if !contains(o.EventIDs, id) {
					m.rep.errorf("%s: set names event %q, which is not one of this outcome's events", where(i), id)
					continue
				}
				if contains(o.Repeated, id) {
					m.rep.errorf("%s: set names event %q, which is repeated: its values are hand-written, one per item", where(i), id)
					continue
				}
				ev := eventOf[m.eventType(id)]
				values := map[string]any{}
				for name, raw := range fields {
					field, ok := fieldNamed(ev.Fields, scaffold.SanitizeName(name))
					if !ok {
						m.rep.errorf("%s: set names field %q, which event %q does not carry", where(i), name, id)
						continue
					}
					var lit any
					_ = json.Unmarshal(raw, &lit)
					typ := map[string]string{"string": "text", "float64": "number", "bool": "bool"}[fmt.Sprintf("%T", lit)]
					switch {
					case field.PII:
						m.rep.errorf("%s: set fixes personal-data field %q of event %q; a model never holds personal data", where(i), name, id)
					case typ != field.Type:
						m.rep.errorf("%s: set gives field %q (%s) of event %q a %s value", where(i), name, field.Type, id, typeOrNull(typ))
					default:
						values[field.Name] = lit
					}
				}
				built.Set[m.eventType(id)] = values
			}
		}
		out = append(out, built)
	}
	return out
}

// outcomeGroups reads when's {all: [conditions]} groups, strictly: a key a
// condition doesn't have is refused, as everywhere else in the document.
func outcomeGroups(raw json.RawMessage) ([][]scaffold.RefusalCondition, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var groups []struct {
		All []RefusalCondition `json:"all"`
	}
	if err := dec.Decode(&groups); err != nil {
		return nil, err
	}
	out := make([][]scaffold.RefusalCondition, 0, len(groups))
	for _, g := range groups {
		var conds []scaffold.RefusalCondition
		for _, c := range g.All {
			cond, err := mapRefusalCondition(c)
			if err != nil {
				return nil, err
			}
			conds = append(conds, cond)
		}
		out = append(out, conds)
	}
	return out, nil
}

func fieldNamed(fields []scaffold.Field, name string) (scaffold.Field, bool) {
	for _, f := range fields {
		if f.Name == name {
			return f, true
		}
	}
	return scaffold.Field{}, false
}

func typeOrNull(t string) string {
	if t == "" {
		return "null"
	}
	return t
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
