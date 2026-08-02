// Package functions provides the PocketCQRS functions-as-a-service layer:
// user-defined functions triggered by domain events (later also HTTP/cron).
//
// Runtime is deliberately language/runtime-agnostic so isolated runtimes
// (wasm, separate processes) can slot in once untrusted code is in scope.
// The v1 GojaRuntime runs trusted, owner-authored JavaScript in-process.
package functions

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/dop251/goja"

	"pocketcqrs/events"
)

// Runtime hosts user-defined functions.
type Runtime interface {
	// RegisterEventFunction compiles source and runs it for each committed
	// event of eventType. The function is called as handle(event).
	RegisterEventFunction(eventType, name, source string) error
}

// FunctionTimeout bounds a single function execution.
const FunctionTimeout = 5 * time.Second

// GojaRuntime executes JavaScript functions in goja VMs (trusted code only).
type GojaRuntime struct {
	logger func(msg string, args ...any)

	mu  sync.RWMutex
	fns map[string][]eventFunction
}

type eventFunction struct {
	name string
	prog *goja.Program
}

// NewGojaRuntime creates a GojaRuntime. logger may be nil (defaults to no-op).
func NewGojaRuntime(logger func(string, ...any)) *GojaRuntime {
	if logger == nil {
		logger = func(string, ...any) {}
	}
	return &GojaRuntime{logger: logger, fns: map[string][]eventFunction{}}
}

// RegisterEventFunction implements Runtime.
func (r *GojaRuntime) RegisterEventFunction(eventType, name, source string) error {
	prog, err := goja.Compile(name, source, false)
	if err != nil {
		return fmt.Errorf("functions: compile %s: %w", name, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fns[eventType] = append(r.fns[eventType], eventFunction{name: name, prog: prog})
	return nil
}

// Subscribe wires the runtime to the store's in-process event feed.
func (r *GojaRuntime) Subscribe(store *events.Store) {
	store.Subscribe(func(ev events.Event) {
		r.mu.RLock()
		fns := r.fns[ev.Type]
		r.mu.RUnlock()
		for _, fn := range fns {
			fn := fn
			go r.run(fn, ev)
		}
	})
}

func (r *GojaRuntime) run(fn eventFunction, ev events.Event) {
	defer func() {
		if rec := recover(); rec != nil {
			r.logger("function panicked", "function", fn.name, "error", rec)
		}
	}()

	vm := goja.New()
	timer := time.AfterFunc(FunctionTimeout, func() {
		vm.Interrupt("function execution timeout")
	})
	defer timer.Stop()

	vm.Set("console", map[string]any{
		"log": func(args ...any) { r.logger("fn "+fn.name, "args", fmt.Sprint(args...)) },
	})

	var data any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		r.logger("function event data decode failed", "function", fn.name, "error", err)
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

	if _, err := vm.RunProgram(fn.prog); err != nil {
		r.logger("function failed", "function", fn.name, "error", err)
	}
}

// ---------------------------------------------------------------------
// Built-in example functions
// ---------------------------------------------------------------------

//go:embed js/task_audit.js
var taskAuditJS string

// RegisterBuiltins registers the built-in example functions.
func RegisterBuiltins(r Runtime) error {
	for _, eventType := range []string{"TaskCreated", "TaskCompleted"} {
		if err := r.RegisterEventFunction(eventType, "task_audit.js", taskAuditJS); err != nil {
			return err
		}
	}
	return nil
}
