package functions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dop251/goja"
	"github.com/pocketbase/pocketbase/core"

	"github.com/JamesTryand/pocketcqrs/events"
	"github.com/JamesTryand/pocketcqrs/writeguard"
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

	ops, err := normalizeOps(result)
	if err != nil {
		return fmt.Errorf("projection %s: %w", p.spec.Name, err)
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
	defer func() {
		if rec := recover(); rec != nil {
			result = nil
			err = fmt.Errorf("%v", rec)
		}
	}()

	vm, timer := r.newVM(spec.Name)
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
}

// normalizeOps converts the project() return value into row ops:
// undefined/null -> none; {upsert:{key,fields}} / {delete:key} -> one;
// an array of those -> many. Each op may carry a "collection" attribute.
func normalizeOps(result any) ([]rowOp, error) {
	if result == nil {
		return nil, nil
	}
	if list, ok := result.([]any); ok {
		var ops []rowOp
		for _, item := range list {
			op, ok, err := normalizeOp(item)
			if err != nil {
				return nil, err
			}
			if ok {
				ops = append(ops, op)
			}
		}
		return ops, nil
	}
	op, ok, err := normalizeOp(result)
	if err != nil || !ok {
		return nil, err
	}
	return []rowOp{op}, nil
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
	return rowOp{}, false, nil
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
