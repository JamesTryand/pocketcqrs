// Package functions provides the PocketCQRS functions-as-a-service layer:
// user-defined functions triggered by domain events (later also HTTP/cron).
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
	_ "embed"
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

	mu  sync.RWMutex
	fns []*eventFunction
}

// eventFunction is a compiled function plus its trigger; it is also the
// consumers.Consumer for checkpointed delivery.
type eventFunction struct {
	name       string
	eventTypes []string
	prog       *goja.Program
	runtime    *GojaRuntime
}

// NewGojaRuntime creates a GojaRuntime. logger may be nil (defaults to no-op).
func NewGojaRuntime(logger func(string, ...any)) *GojaRuntime {
	if logger == nil {
		logger = func(string, ...any) {}
	}
	return &GojaRuntime{logger: logger, fns: nil}
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
	fn.runtime.run(fn.name, fn.prog, ev)
	return nil
}

func (r *GojaRuntime) run(name string, prog *goja.Program, ev events.Event) {
	defer func() {
		if rec := recover(); rec != nil {
			r.logger("function panicked", "function", name, "error", rec)
		}
	}()

	vm := goja.New()
	timer := time.AfterFunc(FunctionTimeout, func() {
		vm.Interrupt("function execution timeout")
	})
	defer timer.Stop()

	vm.Set("console", map[string]any{
		"log": func(args ...any) { r.logger("fn "+name, "args", fmt.Sprint(args...)) },
	})

	var data any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		r.logger("function event data decode failed", "function", name, "error", err)
		return
	}
	vm.Set("event", map[string]any{
		"position":    ev.Position,
		"id":          ev.ID,
		"aggregate":   ev.Aggregate,
		"aggregateId": ev.AggregateID,
		"sequence":    ev.Sequence,
		"type":        ev.Type,
		"data":        data,
		"created":     ev.Created,
	})

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

// ---------------------------------------------------------------------
// Built-in example functions
// ---------------------------------------------------------------------

//go:embed js/task_audit.js
var taskAuditJS string

// RegisterBuiltins registers the built-in example functions.
func RegisterBuiltins(r Runtime) error {
	return r.RegisterEventFunction([]string{"TaskCreated", "TaskCompleted"}, "task_audit.js", taskAuditJS)
}
