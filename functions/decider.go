package functions

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dop251/goja"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

// DeciderSpec is a JS decider: its aggregate, declared event coverage,
// transforms and code.
type DeciderSpec struct {
	Aggregate string
	Handles   []string
	// Commands is the optional //@commands declaration — see
	// decider.Decider.Commands for why it exists.
	Commands   []string
	Transforms []TransformSpec
	Prog       *goja.Program
	runtime    *GojaRuntime
}

// TransformSpec declares an upcaster transform_<Type>_<From>_to_<To>(data).
type TransformSpec struct {
	Type string
	From int64
	To   int64
}

// UntypedDecider adapts the spec to the decider registry's Untyped contract.
//
// Events handed to Evolve are expected to be upcast already: the store's
// read path applies the transform chains (see BuildUpcaster), and every
// production/validation/dry-run fold reads through the store. Validation
// and dry-run additionally apply spec.upcast themselves so they stay
// correct regardless of how the store's upcaster is currently wired
// (idempotent: a transform only fires on an exact From == version match).
func (spec *DeciderSpec) UntypedDecider() decider.Untyped {
	return decider.Untyped{
		Commands: spec.Commands,
		Initial: func() any {
			state, err := spec.runtime.runInitial(spec)
			if err != nil {
				// initialState must not fail; a zero map keeps the system
				// alive and surfaces the real error on first use
				spec.runtime.logger("initialState failed", "decider", spec.Aggregate, "error", err)
				return map[string]any{}
			}
			return state
		},
		Decide: func(cmd decider.Command, state any, meta map[string]any) ([]events.NewEvent, error) {
			return spec.runtime.runDecide(spec, cmd, state, meta)
		},
		Evolve: func(state any, ev events.Event) (any, error) {
			return spec.runtime.runEvolve(spec, state, ev)
		},
	}
}

// ---------------------------------------------------------------------
// tier-3 VM: structurally neutered for decider execution
// ---------------------------------------------------------------------

// newDeciderVM builds a VM for decider code: console is available, but
// Math.random throws, Date is absent, and there are NO pb bindings —
// deciders decide from command+state only. Time comes through the command
// envelope (command.now), injected by the registry and recorded in the
// produced events' metadata.
func (r *GojaRuntime) newDeciderVM(name string) (*goja.Runtime, *time.Timer) {
	vm, timer := r.newVM(name)

	vm.Set("pb", goja.Undefined())
	vm.Set("Date", goja.Undefined())
	if math := vm.Get("Math"); math != nil {
		_ = math.ToObject(vm).Set("random", vm.ToValue(func() {
			panic(vm.NewTypeError("Math.random is not available in decider functions"))
		}))
	}

	return vm, timer
}

func (r *GojaRuntime) runInitial(spec *DeciderSpec) (any, error) {
	vm, timer := r.newDeciderVM(spec.Aggregate)
	defer timer.Stop()
	if _, err := vm.RunProgram(spec.Prog); err != nil {
		return nil, err
	}
	fn, ok := goja.AssertFunction(vm.Get("initialState"))
	if !ok {
		return nil, fmt.Errorf("decider %s does not define initialState()", spec.Aggregate)
	}
	v, err := fn(goja.Undefined())
	if err != nil {
		return nil, err
	}
	return v.Export(), nil
}

func (r *GojaRuntime) runDecide(spec *DeciderSpec, cmd decider.Command, state any, meta map[string]any) (result []events.NewEvent, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			result = nil
			err = fmt.Errorf("%v", rec)
		}
	}()

	vm, timer := r.newDeciderVM(spec.Aggregate)
	defer timer.Stop()
	if _, err := vm.RunProgram(spec.Prog); err != nil {
		return nil, err
	}

	var payload any
	if len(cmd.Payload) > 0 {
		if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
			return nil, err
		}
	}
	command := map[string]any{
		"name":    cmd.Name,
		"payload": payload,
		"now":     meta["now"],
		"actor":   meta["actor"],
	}

	fn, ok := goja.AssertFunction(vm.Get("decide"))
	if !ok {
		return nil, fmt.Errorf("decider %s does not define decide(command, state)", spec.Aggregate)
	}
	v, err := fn(goja.Undefined(), vm.ToValue(command), vm.ToValue(state))
	if err != nil {
		return nil, err
	}
	if goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, nil
	}

	exported := v.Export()
	list, ok := exported.([]any)
	if !ok {
		return nil, fmt.Errorf("decider %s: decide must return an array of events, got %T", spec.Aggregate, exported)
	}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("decider %s: event must be {type, data}, got %T", spec.Aggregate, item)
		}
		typ, _ := m["type"].(string)
		if typ == "" {
			return nil, fmt.Errorf("decider %s: event is missing its type", spec.Aggregate)
		}
		data, err := json.Marshal(m["data"])
		if err != nil {
			return nil, err
		}
		ne := events.NewEvent{Type: typ, Data: data}
		if ver, ok := m["version"]; ok {
			ne.Version = toInt64(ver)
		}
		result = append(result, ne)
	}
	return result, nil
}

func (r *GojaRuntime) runEvolve(spec *DeciderSpec, state any, ev events.Event) (result any, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			result = state
			err = fmt.Errorf("%v", rec)
		}
	}()

	vm, timer := r.newDeciderVM(spec.Aggregate)
	defer timer.Stop()
	if _, err := vm.RunProgram(spec.Prog); err != nil {
		return nil, err
	}

	var data any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		return nil, err
	}
	event := map[string]any{
		"type":    ev.Type,
		"data":    data,
		"version": ev.Version,
	}

	fn, ok := goja.AssertFunction(vm.Get("evolve"))
	if !ok {
		return nil, fmt.Errorf("decider %s does not define evolve(state, event)", spec.Aggregate)
	}
	v, err := fn(goja.Undefined(), vm.ToValue(state), vm.ToValue(event))
	if err != nil {
		return nil, err
	}
	return v.Export(), nil
}

// ---------------------------------------------------------------------
// transforms (upcasters)
// ---------------------------------------------------------------------

// BuildUpcaster composes the per-decider transform chains into one
// store-level upcaster (events.Store.SetUpcaster): events route by
// aggregate to their decider's chain; events of aggregates without a JS
// decider pass through untouched. Returns nil when there are no specs.
func BuildUpcaster(specs []*DeciderSpec) func(events.Event) (events.Event, error) {
	if len(specs) == 0 {
		return nil
	}
	byAggregate := make(map[string]*DeciderSpec, len(specs))
	for _, spec := range specs {
		byAggregate[spec.Aggregate] = spec
	}
	return func(ev events.Event) (events.Event, error) {
		spec, ok := byAggregate[ev.Aggregate]
		if !ok {
			return ev, nil
		}
		return spec.upcast(ev)
	}
}

// upcast applies the declared transform chain to bring ev to the latest
// version of its type before evolve sees it.
func (spec *DeciderSpec) upcast(ev events.Event) (events.Event, error) {
	for {
		var next *TransformSpec
		for i := range spec.Transforms {
			if spec.Transforms[i].Type == ev.Type && spec.Transforms[i].From == ev.Version {
				next = &spec.Transforms[i]
				break
			}
		}
		if next == nil {
			return ev, nil
		}
		data, err := spec.runtime.runTransform(spec, *next, ev.Data)
		if err != nil {
			return ev, err
		}
		ev.Data = data
		ev.Version = next.To
	}
}

func (spec *DeciderSpec) transformName(t TransformSpec) string {
	return fmt.Sprintf("transform_%s_%d_to_%d", t.Type, t.From, t.To)
}

func (r *GojaRuntime) runTransform(spec *DeciderSpec, t TransformSpec, data json.RawMessage) (json.RawMessage, error) {
	vm, timer := r.newDeciderVM(spec.Aggregate)
	defer timer.Stop()
	if _, err := vm.RunProgram(spec.Prog); err != nil {
		return nil, err
	}

	name := spec.transformName(t)
	fn, ok := goja.AssertFunction(vm.Get(name))
	if !ok {
		return nil, fmt.Errorf("decider %s declares //@transform %s %d %d but does not define %s(data)",
			spec.Aggregate, t.Type, t.From, t.To, name)
	}

	var parsed any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, err
	}
	v, err := fn(goja.Undefined(), vm.ToValue(parsed))
	if err != nil {
		return nil, err
	}
	return json.Marshal(v.Export())
}

// ---------------------------------------------------------------------
// boot-time dry-run validation
// ---------------------------------------------------------------------

// ValidateDeciderSpec dry-runs the decider against existing history before
// it is registered: the program must define the contract functions plus all
// declared transforms, and every existing stream of the aggregate must fold
// through evolve without error, with every event type covered by
// //@handles. (Historical commands are not persisted, so validation covers
// the "prior events" half of the dry-run harness.)
func ValidateDeciderSpec(store *events.Store, spec *DeciderSpec) error {
	ctx := context.Background()

	// contract probe: required functions exist
	if _, err := spec.runtime.runInitial(spec); err != nil {
		return fmt.Errorf("validation: %w", err)
	}
	vm, timer := spec.runtime.newDeciderVM(spec.Aggregate)
	if _, err := vm.RunProgram(spec.Prog); err != nil {
		timer.Stop()
		return fmt.Errorf("validation: %w", err)
	}
	for _, fnName := range []string{"decide", "evolve"} {
		if _, ok := goja.AssertFunction(vm.Get(fnName)); !ok {
			timer.Stop()
			return fmt.Errorf("validation: decider %s does not define %s", spec.Aggregate, fnName)
		}
	}
	for _, t := range spec.Transforms {
		if _, ok := goja.AssertFunction(vm.Get(spec.transformName(t))); !ok {
			timer.Stop()
			return fmt.Errorf("validation: decider %s declares //@transform %s %d %d but does not define %s(data)",
				spec.Aggregate, t.Type, t.From, t.To, spec.transformName(t))
		}
	}
	timer.Stop()

	// fold every existing stream of this aggregate
	streamIDs, err := store.ListStreams(ctx, spec.Aggregate)
	if err != nil {
		return err
	}
	d := spec.UntypedDecider()
	for _, id := range streamIDs {
		stream, err := store.LoadStream(ctx, spec.Aggregate, id)
		if err != nil {
			return err
		}
		state := d.Initial()
		for _, ev := range stream {
			// upcast explicitly: validation must not depend on how the
			// store's upcaster is wired (idempotent if it already fired)
			if ev, err = spec.upcast(ev); err != nil {
				return fmt.Errorf("validation: decider %s: upcasting stream %s failed at %s#%d: %w",
					spec.Aggregate, id, ev.Type, ev.Sequence, err)
			}
			if !contains(spec.Handles, ev.Type) {
				return fmt.Errorf("validation: decider %s: stream %s contains %s which is not declared in //@handles",
					spec.Aggregate, id, ev.Type)
			}
			if state, err = d.Evolve(state, ev); err != nil {
				return fmt.Errorf("validation: decider %s: folding stream %s failed at %s#%d: %w",
					spec.Aggregate, id, ev.Type, ev.Sequence, err)
			}
		}
	}
	return nil
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		s := fmt.Sprint(v)
		var i int64
		_, _ = fmt.Sscan(s, &i)
		return i
	}
}
