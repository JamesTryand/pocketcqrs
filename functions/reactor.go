package functions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dop251/goja"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/reactors"
)

// ReactorSpec is a compiled JS reactor: a durable consumer that maps
// committed events to COMMANDS.
//
// The file defines react(event) and RETURNS dispatch descriptors —
// {aggregate, id, command, payload} — rather than calling a host binding
// that dispatches for it. That mirrors project() returning row ops and
// decide() returning events: the function stays pure, so a dry run can
// report what it WOULD send without stubbing anything out, and the one rule
// that matters is enforced in exactly one place. The rule is that a reaction
// enters as a COMMAND through the decider registry — appending directly
// would make the resulting event an unconditional pass-through rather than
// a decision, which is precisely what the write side exists to prevent.
type ReactorSpec struct {
	// Reactor is the logical name (the file's basename, no extension).
	Reactor string
	// EventTypes are the //@trigger react types.
	EventTypes []string
	// Dispatches are the declared "<aggregate>/<Command>" pairs, if any.
	// Declared because a command LEAVES NO TRACE: the event it causes is
	// recoverable from the log, but only once the reactor has actually
	// fired, so an automation that has never run is invisible without this.
	Dispatches []string
	Prog       *goja.Program
	runtime    *GojaRuntime
}

// Name implements consumers.Consumer — and is the durable checkpoint key.
//
// "fn-react:", NOT "reactor:", which Go reactors already use
// (reactors.consumer.Name). A JS reactor sharing that prefix with a
// same-named Go reactor would share its CHECKPOINT, silently skipping events
// for both. The metadata actor stamped on dispatched commands is a separate
// decision that goes the other way — see reactors.Dispatch.
func (s *ReactorSpec) Name() string { return "fn-react:" + s.Reactor }

// Apply implements consumers.Consumer: runs react() for a matching event and
// dispatches whatever it returned.
//
// A reactor that throws is dead-lettered, like an effect function and unlike
// a projection: it causes writes, so it is effect-tier at most, and a poison
// reaction must not block the log.
func (s *ReactorSpec) Apply(ctx context.Context, ev events.Event) error {
	if !contains(s.EventTypes, ev.Type) {
		return nil
	}
	rt := s.runtime

	registry := rt.Registry()
	if registry == nil {
		// Loud, not silent. Without a registry there is nothing to dispatch
		// THROUGH, and quietly doing nothing would look like a reactor whose
		// triggers never matched.
		return fmt.Errorf("functions: reactor %s: no decider registry installed (call SetRegistry)", s.Reactor)
	}

	result, err := rt.runReactor(s, ev)
	if err != nil {
		rt.logger("reactor failed, dead-lettered",
			"reactor", s.Reactor, "position", ev.Position, "error", err)
		rt.deadLetter(ctx, s.Name(), ev, err)
		return nil
	}
	reactions, ignored, err := normalizeDispatches(result)
	if err != nil {
		rt.logger("reactor returned an unusable dispatch, dead-lettered",
			"reactor", s.Reactor, "position", ev.Position, "error", err)
		rt.deadLetter(ctx, s.Name(), ev, err)
		return nil
	}
	// Count what is discarded and say so. A reactor returning plain objects
	// would otherwise dispatch NOTHING, forever, with the log showing only a
	// consumer that keeps up perfectly — the same shape of quiet wrong-doing
	// as a projection returning non-ops.
	if ignored > 0 {
		rt.logger("reactor returned values that are not dispatches; discarded",
			"reactor", s.Reactor, "position", ev.Position, "count", ignored)
	}
	return reactors.Dispatch(ctx, registry, s.Reactor, ev, reactions, rt.logger)
}

// buildReactorSpec compiles a reactor file.
func buildReactorSpec(rt *GojaRuntime, filename string, src string, t triggers) (*ReactorSpec, error) {
	name := strings.TrimSuffix(filename, ".js")
	prog, err := goja.Compile(filename, src, false)
	if err != nil {
		return nil, fmt.Errorf("functions: compile %s: %w", filename, err)
	}
	return &ReactorSpec{
		Reactor:    name,
		EventTypes: t.react,
		Dispatches: t.dispatches,
		Prog:       prog,
		runtime:    rt,
	}, nil
}

// LoadReactorSource loads a JS reactor from source held in memory (the
// editor's candidate code, and the dry run).
func LoadReactorSource(rt *GojaRuntime, name, src string) (*ReactorSpec, error) {
	t, err := parseTriggers(src)
	if err != nil {
		return nil, fmt.Errorf("functions: %s: %w", name, err)
	}
	if len(t.react) == 0 {
		return nil, fmt.Errorf("functions: %s: missing //@trigger react directive", name)
	}
	return buildReactorSpec(rt, name, src, t)
}

// runReactor executes react(event) in a fresh VM and returns what it gave
// back, un-normalized.
//
// Math.random is seeded from the event position, exactly as a projection's
// is. Delivery is at-least-once, so a replay must produce the SAME
// reactions — an unseeded random would make a retry dispatch something
// different from the attempt it is retrying.
func (r *GojaRuntime) runReactor(spec *ReactorSpec, ev events.Event) (result any, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			result = nil
			err = fmt.Errorf("%v", rec)
		}
	}()

	vm, timer := r.newVM(spec.Reactor)
	defer timer.Stop()
	seedRandom(vm, ev.Position)

	var data any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		return nil, fmt.Errorf("reactor %s: event data decode: %w", spec.Reactor, err)
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
	react, ok := goja.AssertFunction(vm.Get("react"))
	if !ok {
		return nil, fmt.Errorf("reactor %s does not define react(event)", spec.Reactor)
	}
	v, err := react(goja.Undefined(), vm.ToValue(event))
	if err != nil {
		return nil, err
	}
	if goja.IsUndefined(v) || goja.IsNull(v) {
		return nil, nil
	}
	return v.Export(), nil
}

// normalizeDispatches turns what react() returned into reactions, counting
// anything that is not a dispatch descriptor rather than dropping it
// silently. A descriptor is {aggregate, id, command, payload?}.
//
// An error means the shape was recognisably a dispatch but unusable (a
// missing aggregate, say) — worth failing loudly, because the author clearly
// meant to send something. A non-descriptor is merely counted: returning a
// stray value is a mistake the count names without failing the delivery.
func normalizeDispatches(result any) ([]reactors.Reaction, int, error) {
	if result == nil {
		return nil, 0, nil
	}
	var raw []any
	switch v := result.(type) {
	case []any:
		raw = v
	case map[string]any:
		raw = []any{v}
	default:
		return nil, 1, nil
	}

	var out []reactors.Reaction
	ignored := 0
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			ignored++
			continue
		}
		aggregate, _ := m["aggregate"].(string)
		id, _ := m["id"].(string)
		command, _ := m["command"].(string)
		if aggregate == "" && id == "" && command == "" {
			ignored++
			continue
		}
		if aggregate == "" || id == "" || command == "" {
			return nil, ignored, fmt.Errorf(
				"dispatch needs aggregate, id and command (got aggregate=%q id=%q command=%q)",
				aggregate, id, command)
		}
		payload := json.RawMessage(`{}`)
		if p, present := m["payload"]; present && p != nil {
			encoded, err := json.Marshal(p)
			if err != nil {
				return nil, ignored, fmt.Errorf("dispatch payload for %s/%s is not encodable: %w", aggregate, command, err)
			}
			payload = encoded
		}
		out = append(out, reactors.Reaction{
			Aggregate: aggregate,
			ID:        id,
			Command:   decider.Command{Name: command, Payload: payload},
		})
	}
	return out, ignored, nil
}

// ReactorDryRun reports what a candidate reactor WOULD dispatch.
type ReactorDryRun struct {
	Reactor string
	// Events is how many matching events were replayed.
	Events int
	// Dispatches is what react() returned across those events. Named for
	// its own shape: `events` already means a count in other modes, and one
	// field meaning two things by mode is a trap for every client.
	Dispatches []DispatchPreview
	// IgnoredValues counts returned values that were not dispatches.
	IgnoredValues int
}

// DispatchPreview is one command a reactor would send.
type DispatchPreview struct {
	CausedBy  int64           `json:"causedBy"`
	Aggregate string          `json:"aggregate"`
	ID        string          `json:"id"`
	Command   string          `json:"command"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// DryRunReactor replays the log through a candidate reactor and reports the
// commands it would dispatch. NOTHING is dispatched: no registry is touched,
// so no decider runs and no event is appended.
func DryRunReactor(store *events.Store, spec *ReactorSpec) (*ReactorDryRun, error) {
	ctx := context.Background()
	out := &ReactorDryRun{Reactor: spec.Reactor}

	var pos int64
	for {
		batch, err := store.Poll(ctx, pos, 100)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		for _, ev := range batch {
			pos = ev.Position
			if !contains(spec.EventTypes, ev.Type) {
				continue
			}
			out.Events++
			result, err := spec.runtime.runReactor(spec, ev)
			if err != nil {
				return nil, fmt.Errorf("dryrun: reactor %s failed at event %d: %w", spec.Reactor, ev.Position, err)
			}
			reactions, ignored, err := normalizeDispatches(result)
			if err != nil {
				return nil, fmt.Errorf("dryrun: reactor %s at event %d: %w", spec.Reactor, ev.Position, err)
			}
			out.IgnoredValues += ignored
			for _, r := range reactions {
				out.Dispatches = append(out.Dispatches, DispatchPreview{
					CausedBy:  ev.Position,
					Aggregate: r.Aggregate,
					ID:        r.ID,
					Command:   r.Command.Name,
					Payload:   r.Command.Payload,
				})
			}
		}
	}
	return out, nil
}

// compile-time proof that a spec really is a consumer
var _ interface {
	Name() string
	Apply(context.Context, events.Event) error
} = (*ReactorSpec)(nil)
