package functions

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// A JS projection's `pb` reads only the collections the projection itself
// declares (P1). Its own rows are a fold of the events it has seen, so
// reading them back stays deterministic: a rebuild reads what the first run
// read. Another read model is someone else's fold, at whatever point that
// fold had reached when this one ran, so a read of it is refused; a
// projection that needs those facts subscribes to the events they come from.

// projectionReader is a projection's live `pb`.
type projectionReader struct {
	spec  *ProjectionSpec
	inner Reader // nil reads nothing, as an unconfigured runtime's stub does
}

func (p projectionReader) FindRecord(collection, id string) (map[string]any, error) {
	if err := p.spec.checkRead(collection); err != nil || p.inner == nil {
		return nil, err
	}
	return p.inner.FindRecord(collection, id)
}

func (p projectionReader) Query(collection, filter string, limit int) ([]map[string]any, error) {
	if err := p.spec.checkRead(collection); err != nil || p.inner == nil {
		return nil, err
	}
	return p.inner.Query(collection, filter, limit)
}

func (spec *ProjectionSpec) checkRead(collection string) error {
	own := spec.Collections()
	if contains(own, collection) {
		return nil
	}
	return fmt.Errorf("projection %s may read only its own collections (%s), not %q: another read model "+
		"is a fold at whatever point it had reached, so a rebuild could differ; subscribe to the events it "+
		"is built from instead", spec.Name, strings.Join(own, ", "), collection)
}

// simulatedReader is a dry run's `pb`: the rows the simulation has produced
// so far, so a read-modify-write projection verifies as it runs live. A
// simulated row has no record id, so findRecord finds nothing; read your
// own rows back with query on the key.
type simulatedReader struct {
	spec *ProjectionSpec
	rows map[string]map[string]map[string]any
}

func (s simulatedReader) FindRecord(collection, _ string) (map[string]any, error) {
	return nil, s.spec.checkRead(collection)
}

func (s simulatedReader) Query(collection, filter string, limit int) ([]map[string]any, error) {
	if err := s.spec.checkRead(collection); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100 // as AppReader
	}
	rows, err := simulatedQuery(s.rows[collection], filter)
	if err != nil {
		return nil, fmt.Errorf("projection %s: %w", s.spec.Name, err)
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	// copies, so the script can't change the simulation by changing what it read
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		raw, _ := json.Marshal(r)
		var c map[string]any
		_ = json.Unmarshal(raw, &c)
		out = append(out, c)
	}
	return out, nil
}

// simulatedQuery evaluates a PocketBase filter over simulated rows, in key
// order. Only what a projection reading its own rows back needs is
// simulated: `field = value` and `field != value` terms joined by `&&`,
// with a quoted string, a number, true, false or null. Anything else is
// refused rather than answered with nothing, since an empty answer is how
// a scenario passes vacuously.
func simulatedQuery(rows map[string]map[string]any, filter string) ([]map[string]any, error) {
	terms, err := parseFilter(filter)
	if err != nil {
		return nil, fmt.Errorf("verify can't simulate the filter %q (%v); use field = 'value' terms joined by &&", filter, err)
	}
	keys := make([]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []map[string]any
	for _, k := range keys {
		if matchesAll(rows[k], terms) {
			out = append(out, rows[k])
		}
	}
	return out, nil
}

type filterTerm struct {
	field  string
	negate bool
	value  any // string, float64, bool or nil
}

func matchesAll(row map[string]any, terms []filterTerm) bool {
	for _, t := range terms {
		if filterEqual(row[t.field], t.value) == t.negate {
			return false
		}
	}
	return true
}

func filterEqual(have, want any) bool {
	switch w := want.(type) {
	case nil:
		return have == nil || have == ""
	case string:
		h, ok := have.(string)
		return ok && h == w
	case bool:
		h, ok := have.(bool)
		return ok && h == w
	case float64:
		h, ok := toFloat(have)
		return ok && h == w
	}
	return false
}

func parseFilter(filter string) ([]filterTerm, error) {
	l := &filterLexer{src: filter}
	var terms []filterTerm
	if l.skipSpace(); l.done() {
		return nil, nil
	}
	for {
		field, ok := l.ident()
		if !ok {
			return nil, fmt.Errorf("expected a field at %d", l.pos)
		}
		var t filterTerm
		t.field = field
		switch l.skipSpace(); {
		case l.take("!="):
			t.negate = true
		case l.take("="):
		default:
			return nil, fmt.Errorf("expected = or != at %d", l.pos)
		}
		l.skipSpace()
		v, err := l.value()
		if err != nil {
			return nil, err
		}
		t.value = v
		terms = append(terms, t)
		if l.skipSpace(); l.done() {
			return terms, nil
		}
		if !l.take("&&") {
			return nil, fmt.Errorf("expected && at %d", l.pos)
		}
		l.skipSpace()
	}
}

type filterLexer struct {
	src string
	pos int
}

func (l *filterLexer) done() bool { return l.pos >= len(l.src) }

func (l *filterLexer) skipSpace() {
	for !l.done() && (l.src[l.pos] == ' ' || l.src[l.pos] == '\t' || l.src[l.pos] == '\n' || l.src[l.pos] == '\r') {
		l.pos++
	}
}

func (l *filterLexer) take(s string) bool {
	if strings.HasPrefix(l.src[l.pos:], s) {
		l.pos += len(s)
		return true
	}
	return false
}

func isIdentByte(c byte, first bool) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (!first && c >= '0' && c <= '9')
}

func (l *filterLexer) ident() (string, bool) {
	start := l.pos
	for !l.done() && isIdentByte(l.src[l.pos], l.pos == start) {
		l.pos++
	}
	return l.src[start:l.pos], l.pos > start
}

func (l *filterLexer) value() (any, error) {
	if l.done() {
		return nil, fmt.Errorf("expected a value at %d", l.pos)
	}
	if q := l.src[l.pos]; q == '\'' || q == '"' {
		var b strings.Builder
		for l.pos++; !l.done(); l.pos++ {
			c := l.src[l.pos]
			if c == '\\' && l.pos+1 < len(l.src) {
				l.pos++
				b.WriteByte(l.src[l.pos])
				continue
			}
			if c == q {
				l.pos++
				return b.String(), nil
			}
			b.WriteByte(c)
		}
		return nil, fmt.Errorf("unterminated string")
	}
	start := l.pos
	for !l.done() && (isIdentByte(l.src[l.pos], false) || l.src[l.pos] == '.' || l.src[l.pos] == '-') {
		l.pos++
	}
	switch word := l.src[start:l.pos]; word {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	default:
		if n, err := strconv.ParseFloat(word, 64); err == nil {
			return n, nil
		}
		return nil, fmt.Errorf("expected a value at %d", start)
	}
}
