package functions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dop251/goja"
	"github.com/pocketbase/pocketbase/core"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/writeguard"
)

// ProjectionSpec is a JS projection: its trigger, output schemas and code.
// A projection may declare several //@schema blocks (multi-collection);
// its row ops then route by their "collection" attribute.
type ProjectionSpec struct {
	Name       string
	EventTypes []string
	Schemas    []*SchemaSpec
	Prog       *goja.Program
	runtime    *GojaRuntime
	app        core.App
}

// Collections lists the guarded collections this projection owns.
func (spec *ProjectionSpec) Collections() []string {
	out := make([]string, 0, len(spec.Schemas))
	for _, s := range spec.Schemas {
		out = append(out, s.Collection)
	}
	return out
}

// JSProjection is a checkpointed consumer running a user-defined
// project(event) function and applying its returned row ops.
// It satisfies projections.Projection (Name/Collections/Apply).
type JSProjection struct{ spec *ProjectionSpec }

// Consumer wraps the spec as the consumers.Engine-facing projection.
func (spec *ProjectionSpec) Consumer() *JSProjection { return &JSProjection{spec: spec} }

// Name is the durable checkpoint key.
func (p *JSProjection) Name() string { return p.spec.Name }

// Spec returns the underlying projection spec (introspection, e.g. the
// catalog: declared event types and schemas).
func (p *JSProjection) Spec() *ProjectionSpec { return p.spec }

// Collections lists the guarded collections this projection owns.
func (p *JSProjection) Collections() []string { return p.spec.Collections() }

// Apply runs project(event) and applies the returned ops. VM errors and op
// failures are returned: a failing projection blocks at the failing event
// (correctness over progress — rebuild after fixing).
func (p *JSProjection) Apply(ctx context.Context, ev events.Event) error {
	if !contains(p.spec.EventTypes, ev.Type) {
		return nil
	}

	result, err := p.spec.runtime.runProjection(p.spec, ev)
	if err != nil {
		return err
	}

	ops, ignored, err := normalizeOps(result)
	if err != nil {
		return fmt.Errorf("projection %s: %w", p.spec.Name, err)
	}
	// An object that is neither an upsert nor a delete is not an op, and was
	// silently discarded: a projection returning plain rows wrote nothing,
	// forever, without a word. It is not an error — returning nothing is a
	// legitimate outcome and projections BLOCK on error — but it must be
	// audible, because the symptom (an empty collection) points nowhere near
	// the cause. The dry run says the same thing before it ever ships.
	if ignored > 0 {
		p.spec.runtime.logger("projection returned values that are not row ops, ignored",
			"projection", p.spec.Name, "event", ev.Type, "position", ev.Position, "ignored", ignored,
			"hint", "project(event) must return {upsert:{key,fields}}, {delete:key}, {increment:{key,field,delta}} or {groupByBump:{key,field,groupField,groupValue,subfield,delta}}")
	}

	ctx = writeguard.MarkInternal(ctx)
	for _, op := range ops {
		s, err := p.spec.resolveSchema(op)
		if err != nil {
			return fmt.Errorf("projection %s: %w", p.spec.Name, err)
		}
		if err := p.spec.applyOp(ctx, s, op); err != nil {
			return fmt.Errorf("projection %s: %w", p.spec.Name, err)
		}
	}
	return nil
}

// runProjection executes project(event) in a tier-2 (deterministic) VM:
// Math.random is replaced by a PRNG seeded from the event position, so
// replays produce identical sequences. Use event.created for time.
func (r *GojaRuntime) runProjection(spec *ProjectionSpec, ev events.Event) (result any, err error) {
	return r.runProjectionWith(spec, ev, false)
}

// runProjectionWith runs project(event); isolated cuts the `pb` read binding
// off from live collections, for fixture runs where reading real rows would
// make the answer depend on state the fixture does not describe.
func (r *GojaRuntime) runProjectionWith(spec *ProjectionSpec, ev events.Event, isolated bool) (result any, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			result = nil
			err = fmt.Errorf("%v", rec)
		}
	}()

	newVM := r.newVM
	if isolated {
		newVM = r.newVMIsolated
	}
	vm, timer := newVM(spec.Name)
	defer timer.Stop()
	seedRandom(vm, ev.Position)

	var data any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		return nil, fmt.Errorf("projection %s: event data decode: %w", spec.Name, err)
	}
	event := map[string]any{
		"position":    ev.Position,
		"id":          ev.ID,
		"aggregate":   ev.Aggregate,
		"aggregateId": ev.AggregateID,
		"sequence":    ev.Sequence,
		"type":        ev.Type,
		"data":        data,
		"version":     ev.Version,
		"created":     ev.Created,
	}
	vm.Set("event", event)

	if _, err := vm.RunProgram(spec.Prog); err != nil {
		return nil, err
	}
	project, ok := goja.AssertFunction(vm.Get("project"))
	if !ok {
		return nil, fmt.Errorf("projection %s does not define project(event)", spec.Name)
	}
	v, err := project(goja.Undefined(), vm.ToValue(event))
	if err != nil {
		return nil, err
	}
	if goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, nil
	}
	return v.Export(), nil
}

// seedRandom replaces Math.random with a deterministic PRNG (splitmix64)
// seeded by seed, so projection replays see identical sequences.
func seedRandom(vm *goja.Runtime, seed int64) {
	state := uint64(seed)*0x9E3779B97F4A7C15 + 0x9E3779B97F4A7C15
	random := func() float64 {
		state += 0x9E3779B97F4A7C15
		z := state
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		z = z ^ (z >> 31)
		return float64(z>>11) / float64(uint64(1)<<53)
	}
	if math := vm.Get("Math"); math != nil {
		_ = math.ToObject(vm).Set("random", random)
	}
}

// ---------------------------------------------------------------------
// row ops
// ---------------------------------------------------------------------

// rowOp is one declarative row operation returned by project(event).
// collection names the target schema; empty routes to the projection's
// only schema (ambiguous and rejected when several are declared).
type rowOp struct {
	collection string
	delete     bool
	key        any
	fields     map[string]any
	// increment marks a count/sum-derivation op (Finding 3): add delta to
	// incField rather than set fields verbatim. It exists as its own op
	// shape, not a fields entry, because a derived count/sum must ACCUMULATE
	// across events, and neither this project's own read binding (`pb`, a
	// query-side stub over an isolated fixture — see M7 in dryrun.go) nor a
	// plain upsert's field-merge can do that: an upsert always sets the
	// value the caller computed, so a projection that wanted to accumulate
	// via upsert would need to read the running total back first, which a
	// fixture verify run must not do (see runProjectionWith's isolation
	// doc). Increment sidesteps the read entirely — the host adds the
	// delta, live or in a fixture simulation alike (dryrun.go's
	// runProjectionOver), so a generated toggle/count/sum projection
	// verifies against a scenario exactly like a plain merge does.
	increment bool
	incField  string
	delta     float64

	// groupBy marks a groupBy-derivation op (schema 2.3.0's nested/grouped
	// rollup, Finding 4): find-or-create the entry in groupByField's own
	// JSON array (on the row named by key) whose groupKeyField equals
	// groupKeyValue, then add delta to that entry's subfield. Reuses key
	// and delta from increment above — same accumulation, one JSON level
	// deeper — for exactly the same reason increment exists at all: a
	// fixture verify run must not read its own row back (see increment's
	// own doc comment), so the host does the find-or-create-then-bump
	// itself, live (applyOp below) or in a fixture simulation alike
	// (dryrun.go's runProjectionOver).
	groupBy       bool
	groupByField  string
	groupKeyField string
	groupKeyValue any
	subfield      string
}

// normalizeOps converts the project() return value into row ops:
// undefined/null -> none; {upsert:{key,fields}} / {delete:key} /
// {increment:{key,field,delta}} / {groupByBump:{key,field,groupField,
// groupValue,subfield,delta}} -> one; an array of those -> many. Each op
// may carry a "collection" attribute.
//
// It also reports how many returned values were objects but not ops, so the
// caller can say so — silently dropping them is how a projection ends up
// writing nothing with no explanation anywhere.
func normalizeOps(result any) (ops []rowOp, ignored int, err error) {
	if result == nil {
		return nil, 0, nil
	}
	if list, ok := result.([]any); ok {
		for _, item := range list {
			op, ok, err := normalizeOp(item)
			if err != nil {
				return nil, 0, err
			}
			if ok {
				ops = append(ops, op)
			} else {
				ignored++
			}
		}
		return ops, ignored, nil
	}
	op, ok, err := normalizeOp(result)
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, 1, nil
	}
	return []rowOp{op}, 0, nil
}

func normalizeOp(v any) (rowOp, bool, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return rowOp{}, false, fmt.Errorf("op must be an object, got %T", v)
	}
	var collection string
	if c, ok := m["collection"]; ok {
		if s, ok := c.(string); ok {
			collection = s
		} else {
			return rowOp{}, false, fmt.Errorf("op collection must be a string, got %T", c)
		}
	}
	if del, ok := m["delete"]; ok {
		return rowOp{collection: collection, delete: true, key: del}, true, nil
	}
	if up, ok := m["upsert"]; ok {
		um, ok := up.(map[string]any)
		if !ok {
			return rowOp{}, false, fmt.Errorf("upsert must be {key, fields}, got %T", up)
		}
		fields, _ := um["fields"].(map[string]any)
		return rowOp{collection: collection, key: um["key"], fields: fields}, true, nil
	}
	if inc, ok := m["increment"]; ok {
		im, ok := inc.(map[string]any)
		if !ok {
			return rowOp{}, false, fmt.Errorf("increment must be {key, field, delta}, got %T", inc)
		}
		field, _ := im["field"].(string)
		if field == "" {
			return rowOp{}, false, fmt.Errorf("increment op is missing its field name")
		}
		rawDelta, hasDelta := im["delta"]
		if !hasDelta {
			return rowOp{}, false, fmt.Errorf("increment op %q is missing its delta", field)
		}
		delta, ok := toFloat(rawDelta)
		if !ok {
			return rowOp{}, false, fmt.Errorf("increment op %q delta must be a number, got %T", field, rawDelta)
		}
		return rowOp{collection: collection, increment: true, key: im["key"], incField: field, delta: delta}, true, nil
	}
	if gb, ok := m["groupByBump"]; ok {
		gm, ok := gb.(map[string]any)
		if !ok {
			return rowOp{}, false, fmt.Errorf("groupByBump must be {key, field, groupField, groupValue, subfield, delta}, got %T", gb)
		}
		field, _ := gm["field"].(string)
		if field == "" {
			return rowOp{}, false, fmt.Errorf("groupByBump op is missing its field name")
		}
		groupField, _ := gm["groupField"].(string)
		if groupField == "" {
			return rowOp{}, false, fmt.Errorf("groupByBump op %q is missing its groupField name", field)
		}
		subfield, _ := gm["subfield"].(string)
		if subfield == "" {
			return rowOp{}, false, fmt.Errorf("groupByBump op %q is missing its subfield name", field)
		}
		rawDelta, hasDelta := gm["delta"]
		if !hasDelta {
			return rowOp{}, false, fmt.Errorf("groupByBump op %q.%q is missing its delta", field, subfield)
		}
		delta, ok := toFloat(rawDelta)
		if !ok {
			return rowOp{}, false, fmt.Errorf("groupByBump op %q.%q delta must be a number, got %T", field, subfield, rawDelta)
		}
		return rowOp{collection: collection, groupBy: true, key: gm["key"], groupByField: field,
			groupKeyField: groupField, groupKeyValue: gm["groupValue"], subfield: subfield, delta: delta}, true, nil
	}
	return rowOp{}, false, nil
}

// toFloat coerces a value decoded from JS (goja numbers export as float64,
// but a Go-built fixture or a JSON round-trip may hand back other numeric
// kinds) into a float64. false means v is not a number at all.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// resolveSchema determines which declared schema an op targets: an explicit
// op.collection must name a declared schema; without one the projection
// must declare exactly one schema.
func (spec *ProjectionSpec) resolveSchema(op rowOp) (*SchemaSpec, error) {
	if op.collection != "" {
		for _, s := range spec.Schemas {
			if s.Collection == op.collection {
				return s, nil
			}
		}
		return nil, fmt.Errorf("op targets undeclared collection %q", op.collection)
	}
	if len(spec.Schemas) == 1 {
		return spec.Schemas[0], nil
	}
	return nil, fmt.Errorf("op is ambiguous: projection declares %d schemas, set the op's \"collection\"", len(spec.Schemas))
}

// reservedRowFields may not be set by projection ops.
var reservedRowFields = map[string]bool{"id": true, "created": true, "updated": true}

// groupByRowIndex finds the entry in a groupBy field's own nested list whose
// groupKeyField matches groupKeyValue, or -1. Shared by applyOp (live) and
// dryrun.go's runProjectionOver (isolated simulation) so both read the same
// find-or-create rule. jsonEq-style comparison (via jsonEqual, dryrun.go),
// not ==, because the live path's rows come back from
// Record.UnmarshalJSONField decoding through encoding/json exactly like the
// isolated path's do — but a value straight off op.groupKeyValue may still
// be a different concrete numeric type than one that round-tripped through
// JSON, and jsonEqual normalizes both through the same encoder before
// comparing.
func groupByRowIndex(rows []map[string]any, groupKeyField string, groupKeyValue any) int {
	for i, r := range rows {
		if jsonEqual(r[groupKeyField], groupKeyValue) {
			return i
		}
	}
	return -1
}

// applyOp applies one row op to the schema's collection with the internal
// writeguard marker. Upserts are keyed (idempotent); deletes are no-ops
// when the row is absent.
func (spec *ProjectionSpec) applyOp(ctx context.Context, s *SchemaSpec, op rowOp) error {
	app := spec.app
	keyField := s.Key

	rec, err := app.FindFirstRecordByData(s.Collection, keyField, op.key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	if op.delete {
		if rec == nil {
			return nil
		}
		return app.DeleteWithContext(ctx, rec)
	}

	if rec == nil {
		col, err := app.FindCollectionByNameOrId(s.Collection)
		if err != nil {
			return err
		}
		rec = core.NewRecord(col)
		rec.Set(keyField, op.key)
	}

	if op.increment {
		if reservedRowFields[op.incField] || op.incField == keyField {
			spec.runtime.logger("projection op field ignored (reserved)",
				"projection", spec.Name, "field", op.incField)
			return nil
		}
		rec.Set(op.incField, rec.GetFloat(op.incField)+op.delta)
		return app.SaveWithContext(ctx, rec)
	}

	if op.groupBy {
		if reservedRowFields[op.groupByField] || op.groupByField == keyField {
			spec.runtime.logger("projection op field ignored (reserved)",
				"projection", spec.Name, "field", op.groupByField)
			return nil
		}
		var rows []map[string]any
		_ = rec.UnmarshalJSONField(op.groupByField, &rows)
		idx := groupByRowIndex(rows, op.groupKeyField, op.groupKeyValue)
		if idx < 0 {
			rows = append(rows, map[string]any{op.groupKeyField: op.groupKeyValue})
			idx = len(rows) - 1
		}
		current, _ := toFloat(rows[idx][op.subfield])
		rows[idx][op.subfield] = current + op.delta
		rec.Set(op.groupByField, rows)
		return app.SaveWithContext(ctx, rec)
	}

	for name, value := range op.fields {
		if reservedRowFields[name] || name == keyField {
			spec.runtime.logger("projection op field ignored (reserved)",
				"projection", spec.Name, "field", name)
			continue
		}
		rec.Set(name, value)
	}
	return app.SaveWithContext(ctx, rec)
}
