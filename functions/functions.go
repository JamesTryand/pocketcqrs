// Package functions provides the PocketCQRS functions-as-a-service layer:
// user-defined functions triggered by domain events or HTTP (cron later).
//
// Runtime is deliberately language/runtime-agnostic so isolated runtimes
// (wasm, separate processes) can slot in once untrusted code is in scope.
// The v1 GojaRuntime runs trusted, owner-authored JavaScript in-process.
//
// Event delivery is durable and at-least-once via the consumers engine
// (checkpointed, replayed after restart). A function that fails on an event
// is logged and skipped for that event — poison messages do not block the
// log (dead-lettering is future work).
package functions

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/pocketbase/pocketbase/tools/cron"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

// Runtime hosts user-defined functions.
type Runtime interface {
	// RegisterEventFunction compiles source as name and runs it once per
	// committed event whose type is in eventTypes.
	RegisterEventFunction(eventTypes []string, name, source string) error
}

// FunctionTimeout bounds a single function execution.
const FunctionTimeout = 5 * time.Second

// GojaRuntime executes JavaScript functions in goja VMs (trusted code only).
type GojaRuntime struct {
	logger func(msg string, args ...any)
	// warn is the level for faults that can never clear on redelivery, as
	// opposed to logger's ordinary reporting. Nil falls back to logger.
	warn     func(msg string, args ...any)
	reader   Reader
	store    *events.Store
	registry *decider.Registry
	outbound OutboundDoer

	mu      sync.RWMutex
	fns     []*eventFunction
	cronFns []*cronFunction
}

// eventFunction is a compiled event function plus its triggers; it is also
// the consumers.Consumer for checkpointed delivery.
type eventFunction struct {
	name       string
	eventTypes []string
	prog       *goja.Program
	runtime    *GojaRuntime
}

// cronFunction is a compiled function fired on a cron schedule.
type cronFunction struct {
	name     string
	schedule string
	prog     *goja.Program
}

// CronJob is a registered cron function ready for app.Cron().
type CronJob struct {
	Name     string
	Schedule string
	Fire     func()
}

// NewGojaRuntime creates a GojaRuntime. logger may be nil (defaults to no-op).
func NewGojaRuntime(logger func(string, ...any)) *GojaRuntime {
	if logger == nil {
		logger = func(string, ...any) {}
	}
	return &GojaRuntime{logger: logger}
}

// SetWarn installs the warning-level logger used for permanent faults (a
// reaction dropped because its target aggregate is gone). It is a capability
// like SetReader/SetStore/SetOutbound, which means a hot reload MUST carry it
// across — see carryCapabilities. Without it the runtime falls back to
// logger, so a missed copy degrades the level silently rather than crashing,
// which is exactly the failure mode this whole change exists to remove.
func (r *GojaRuntime) SetWarn(warn func(string, ...any)) { r.warn = warn }

// Warn exposes the warning logger so the reload's capability-carrying can be
// asserted rather than reviewed. See SetWarn.
func (r *GojaRuntime) Warn() func(string, ...any) { return r.warn }

// SetReader installs the read-only query-side binding exposed as `pb`.
func (r *GojaRuntime) SetReader(reader Reader) { r.reader = reader }

// SetStore installs the event store used for dead-lettering failed
// deliveries. Without it, failures are only logged.
func (r *GojaRuntime) SetStore(store *events.Store) { r.store = store }

// SetRegistry installs the decider registry that JS reactors dispatch
// through. Without it a reactor fails loudly rather than quietly doing
// nothing — a dry run compiles into a scratch runtime that has none, and
// that must not look like a reactor whose triggers never matched.
func (r *GojaRuntime) SetRegistry(registry *decider.Registry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registry = registry
}

// Registry returns the installed decider registry, or nil.
func (r *GojaRuntime) Registry() *decider.Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.registry
}

// RegisterEventFunction implements Runtime.
func (r *GojaRuntime) RegisterEventFunction(eventTypes []string, name, source string) error {
	prog, err := goja.Compile(name, source, false)
	if err != nil {
		return fmt.Errorf("functions: compile %s: %w", name, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fns = append(r.fns, &eventFunction{name: name, eventTypes: eventTypes, prog: prog, runtime: r})
	return nil
}

// RegisterCronFunction compiles source as name and schedules it for
// cron-triggered execution (effect tier: script body runs per tick).
func (r *GojaRuntime) RegisterCronFunction(schedule, name, source string) error {
	// Validate the schedule HERE, where loading happens, not later where the
	// job is added to the cron service. A reload loads into a fresh runtime
	// first precisely so a bad file cannot touch live structures — but the
	// cron service is fed after the effect tier has already been swapped, so
	// an unparseable schedule used to abort the reload half-applied. It also
	// meant such a file could be written and then block EVERY later reload.
	if _, err := cron.NewSchedule(schedule); err != nil {
		return fmt.Errorf("functions: %s: invalid cron schedule %q: %w", name, schedule, err)
	}
	prog, err := goja.Compile(name, source, false)
	if err != nil {
		return fmt.Errorf("functions: compile %s: %w", name, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cronFns = append(r.cronFns, &cronFunction{name: name, schedule: schedule, prog: prog})
	return nil
}

// CronJobs returns the registered cron functions ready for app.Cron().Add.
func (r *GojaRuntime) CronJobs() []CronJob {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]CronJob, 0, len(r.cronFns))
	for _, fn := range r.cronFns {
		fn := fn
		out = append(out, CronJob{
			Name:     fn.name,
			Schedule: fn.schedule,
			Fire:     func() { r.runCron(fn) },
		})
	}
	return out
}

// Consumers returns one checkpointed consumer per registered function, for
// registering with the consumers engine.
func (r *GojaRuntime) Consumers() []consumers.Consumer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]consumers.Consumer, 0, len(r.fns))
	for _, fn := range r.fns {
		out = append(out, fn)
	}
	return out
}

// EventFunctionInfo is the introspection view of one event function.
type EventFunctionInfo struct {
	Name       string
	EventTypes []string
}

// EventFunctions returns the registered event functions with their
// declared trigger types (for the catalog).
func (r *GojaRuntime) EventFunctions() []EventFunctionInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]EventFunctionInfo, 0, len(r.fns))
	for _, fn := range r.fns {
		out = append(out, EventFunctionInfo{Name: fn.name, EventTypes: fn.eventTypes})
	}
	return out
}

// Name implements consumers.Consumer.
func (fn *eventFunction) Name() string { return "fn:" + fn.name }

// Apply implements consumers.Consumer: delivers the event to the function
// if it subscribed to the type. A failing function is logged and
// dead-lettered (inspection + retry via the deadletter CLI); the failure
// never blocks the log.
func (fn *eventFunction) Apply(ctx context.Context, ev events.Event) error {
	if !contains(fn.eventTypes, ev.Type) {
		return nil
	}
	if err := fn.runtime.runEvent(fn.name, fn.prog, ev); err != nil {
		fn.runtime.logger("function delivery failed, dead-lettered",
			"function", fn.name, "position", ev.Position, "error", err)
		fn.runtime.deadLetter(ctx, fn.Name(), ev, err)
	}
	return nil
}

// deadLetter records a failed delivery if a store is installed.
func (r *GojaRuntime) deadLetter(ctx context.Context, consumer string, ev events.Event, cause error) {
	if r.store == nil {
		return
	}
	if err := r.store.AddDeadLetter(ctx, consumer, ev, cause); err != nil {
		r.logger("failed to write dead letter", "consumer", consumer, "error", err)
	}
}

// RetryEventFunction re-delivers an event to the named function (the most
// recently registered with that name, i.e. the current code). Used by the
// deadletter CLI; the error is returned for the caller to adjudicate.
func (r *GojaRuntime) RetryEventFunction(name string, ev events.Event) error {
	r.mu.RLock()
	var fn *eventFunction
	for _, f := range r.fns {
		if f.name == name {
			fn = f // keep the last (most recently loaded)
		}
	}
	r.mu.RUnlock()
	if fn == nil {
		return fmt.Errorf("functions: no function named %s is registered", name)
	}
	return r.runEvent(fn.name, fn.prog, ev)
}

// newVM creates a VM with the standard bindings (console, pb read access)
// and the execution timeout armed.
//
// It is what every path WITHOUT outbound access uses: deciders, projections
// and http-triggered functions. The paths that may call out use
// newOutboundVM (outbound.go), which layers `$http` on top. Do not move that
// binding down into here — it would grant outbound access to deciders and
// projections by default and leave the purity invariant depending on
// somebody remembering to subtract it again.
func (r *GojaRuntime) newVM(name string) (*goja.Runtime, *time.Timer) {
	return r.newVMWithReader(name, r.reader)
}

// newVMIsolated creates a VM whose `pb` binding reads NOTHING.
//
// It exists for fixture runs: "given exactly these events, what happens?" is
// a question a projection reading live collections cannot answer honestly.
// A read-modify-write projection querying real rows mid-simulation would
// produce a result that depends on the live database, which is how a
// fixture assertion passes vacuously.
func (r *GojaRuntime) newVMIsolated(name string) (*goja.Runtime, *time.Timer) {
	return r.newVMWithReader(name, nil)
}

func (r *GojaRuntime) newVMWithReader(name string, reader Reader) (*goja.Runtime, *time.Timer) {
	vm := goja.New()
	timer := time.AfterFunc(FunctionTimeout, func() {
		vm.Interrupt("function execution timeout")
	})

	vm.Set("console", map[string]any{
		"log": func(args ...any) { r.logger("fn "+name, "args", fmt.Sprint(args...)) },
	})

	// read-only query-side access; nil Reader exposes an inert stub
	pb := map[string]any{
		"findRecord": func(collection, id string) (map[string]any, error) { return nil, nil },
		"query":      func(collection, filter string, limit int) ([]map[string]any, error) { return nil, nil },
	}
	if reader != nil {
		pb["findRecord"] = reader.FindRecord
		pb["query"] = reader.Query
	}
	vm.Set("pb", pb)

	return vm, timer
}

func (r *GojaRuntime) runEvent(name string, prog *goja.Program, ev events.Event) error {
	var data any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		return fmt.Errorf("function event data decode failed: %w", err)
	}
	return r.runScript(name, prog, map[string]any{
		"event": map[string]any{
			"position":    ev.Position,
			"id":          ev.ID,
			"aggregate":   ev.Aggregate,
			"aggregateId": ev.AggregateID,
			"sequence":    ev.Sequence,
			"type":        ev.Type,
			"data":        data,
			"version":     ev.Version,
			"created":     ev.Created,
		},
	})
}

// runCron executes a cron function's script body with a `job` binding.
// Cron failures are logged only (there is no event delivery to dead-letter).
func (r *GojaRuntime) runCron(fn *cronFunction) {
	err := r.runScript(fn.name, fn.prog, map[string]any{
		"job": map[string]any{
			"name":    fn.name,
			"firedAt": time.Now().UTC().Format("2006-01-02 15:04:05.000Z"),
		},
	})
	if err != nil {
		r.logger("cron function failed", "function", fn.name, "error", err)
	}
}

// runScript runs prog's body once with the given extra globals. Panics and
// script errors are recovered and returned, never thrown into the host.
func (r *GojaRuntime) runScript(name string, prog *goja.Program, globals map[string]any) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("function %s panicked: %v", name, rec)
		}
	}()

	// effect tier: event- and cron-triggered functions both land here
	vm, timer := r.newOutboundVM(name)
	defer timer.Stop()

	for k, v := range globals {
		vm.Set(k, v)
	}

	if _, err := vm.RunProgram(prog); err != nil {
		return fmt.Errorf("function %s failed: %w", name, err)
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
