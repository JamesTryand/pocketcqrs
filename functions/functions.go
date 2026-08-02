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

	"pocketcqrs/consumers"
	"pocketcqrs/events"
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
	reader Reader

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

// SetReader installs the read-only query-side binding exposed as `pb`.
func (r *GojaRuntime) SetReader(reader Reader) { r.reader = reader }

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

// Name implements consumers.Consumer.
func (fn *eventFunction) Name() string { return "fn:" + fn.name }

// Apply implements consumers.Consumer: delivers the event to the function
// if it subscribed to the type. Execution failures are logged, not returned,
// so a failing function does not block the log.
func (fn *eventFunction) Apply(ctx context.Context, ev events.Event) error {
	if !contains(fn.eventTypes, ev.Type) {
		return nil
	}
	fn.runtime.runEvent(fn.name, fn.prog, ev)
	return nil
}

// newVM creates a VM with the standard bindings (console, pb read access)
// and the execution timeout armed.
func (r *GojaRuntime) newVM(name string) (*goja.Runtime, *time.Timer) {
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
	if r.reader != nil {
		pb["findRecord"] = r.reader.FindRecord
		pb["query"] = r.reader.Query
	}
	vm.Set("pb", pb)

	return vm, timer
}

func (r *GojaRuntime) runEvent(name string, prog *goja.Program, ev events.Event) {
	var data any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		r.logger("function event data decode failed", "function", name, "error", err)
		return
	}
	r.runScript(name, prog, map[string]any{
		"event": map[string]any{
			"position":    ev.Position,
			"id":          ev.ID,
			"aggregate":   ev.Aggregate,
			"aggregateId": ev.AggregateID,
			"sequence":    ev.Sequence,
			"type":        ev.Type,
			"data":        data,
			"created":     ev.Created,
		},
	})
}

// runCron executes a cron function's script body with a `job` binding.
func (r *GojaRuntime) runCron(fn *cronFunction) {
	r.runScript(fn.name, fn.prog, map[string]any{
		"job": map[string]any{
			"name":    fn.name,
			"firedAt": time.Now().UTC().Format("2006-01-02 15:04:05.000Z"),
		},
	})
}

// runScript runs prog's body once with the given extra globals, logging
// panics and errors instead of propagating them (effect-tier semantics).
func (r *GojaRuntime) runScript(name string, prog *goja.Program, globals map[string]any) {
	defer func() {
		if rec := recover(); rec != nil {
			r.logger("function panicked", "function", name, "error", rec)
		}
	}()

	vm, timer := r.newVM(name)
	defer timer.Stop()

	for k, v := range globals {
		vm.Set(k, v)
	}

	if _, err := vm.RunProgram(prog); err != nil {
		r.logger("function failed", "function", name, "error", err)
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
