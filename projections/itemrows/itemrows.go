// Package itemrows plans rowsPerListItem's rows (schema 3.16.0): a row per
// group of a stream's list items, in place of one row per stream, keyed
// '<stream>|<group>'.
//
// The stream's own State (its merged fields, its current list, the groups it
// has rows for) is kept by the host in a companion collection keyed by
// stream, so a projection never reads its own rows back. Plan is pure: the
// JS host (live and dry run) and generated Go projections all call it, so
// they agree.
package itemrows

import "fmt"

// Op is what one event does to a stream's rows.
type Op struct {
	Stream    string
	ListField string
	GroupBy   string
	// Sums maps a row field to the item subfield it sums (sumOfItems): nil
	// unless every one of the row's items carries a number for it.
	Sums map[string]string
	// Fields merge into the stream's fields; every row carries them.
	Fields map[string]any
	// Items, when HasItems (the event carries the list), replaces the list.
	Items    []any
	HasItems bool
	// Remove drops every row of the stream and its state.
	Remove bool
}

// State is a stream's companion row: what its rows are built from.
type State struct {
	Fields map[string]any `json:"fields"`
	Items  []any          `json:"items"`
	Groups []string       `json:"groups"`
}

// Row is one row to write, complete.
type Row struct {
	Key    string
	Fields map[string]any
}

// RowKey is a group's row key: '<stream>|<group>'.
func (op Op) RowKey(group string) string { return op.Stream + "|" + group }

// Plan applies op to the stream's prior state: the next state (nil when the
// stream is removed), the row keys to delete, and the rows to write.
func (op Op) Plan(prior State) (next *State, drop []string, rows []Row) {
	if op.Remove {
		for _, g := range prior.Groups {
			drop = append(drop, op.RowKey(g))
		}
		return nil, drop, nil
	}
	st := State{Fields: map[string]any{}, Items: prior.Items}
	for k, v := range prior.Fields {
		st.Fields[k] = v
	}
	for k, v := range op.Fields {
		if k != op.ListField {
			st.Fields[k] = v
		}
	}
	if op.HasItems {
		st.Items = op.Items
	}

	// groups in order of first appearance; an item without one is in no row
	byGroup := map[string][]any{}
	for _, item := range st.Items {
		m, _ := item.(map[string]any)
		g := m[op.GroupBy]
		if g == nil || g == "" {
			continue
		}
		key := fmt.Sprint(g)
		if _, seen := byGroup[key]; !seen {
			st.Groups = append(st.Groups, key)
		}
		byGroup[key] = append(byGroup[key], item)
	}
	for _, g := range prior.Groups {
		if _, kept := byGroup[g]; !kept {
			drop = append(drop, op.RowKey(g))
		}
	}
	for _, g := range st.Groups {
		fields := map[string]any{}
		for k, v := range st.Fields {
			fields[k] = v
		}
		items := byGroup[g]
		fields[op.GroupBy] = items[0].(map[string]any)[op.GroupBy]
		fields[op.ListField] = items
		for field, sub := range op.Sums {
			fields[field] = SumOfItems(items, sub)
		}
		rows = append(rows, Row{Key: op.RowKey(g), Fields: fields})
	}
	return &st, drop, rows
}

// SumOfItems is the sum of sub over items, or nil unless every item carries
// a number for it (amounts not yet in one currency are never added).
func SumOfItems(items []any, sub string) any {
	total := 0.0
	for _, item := range items {
		m, _ := item.(map[string]any)
		switch n := m[sub].(type) {
		case float64:
			total += n
		case int:
			total += float64(n)
		case int64:
			total += float64(n)
		default:
			return nil
		}
	}
	return total
}
