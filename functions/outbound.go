package functions

import (
	"context"
	"fmt"
	"time"

	"github.com/dop251/goja"

	"github.com/jamestryand/pocketcqrs/outbound"
)

// Bounds for the `$http` binding. They are constants rather than flags
// because they are safety limits, not tuning knobs: an operator who needs a
// 30-second third-party call inside a 5-second VM has a design problem the
// core primitive should not accommodate.
const (
	// OutboundTimeout bounds ONE $http call end to end. It sits strictly
	// under FunctionTimeout so a single slow call surfaces as a catchable JS
	// error the author can handle, rather than as a VM interrupt, which kills
	// the function mid-statement and reports nothing useful about why.
	//
	// IT IS NOT A PER-FUNCTION BOUND. FunctionTimeout is armed once per VM,
	// at creation; this deadline is per call. A function making two
	// sequential slow calls spends 6s against a 5s VM budget and IS
	// interrupted. That is a real limit on how many calls one function may
	// chain, not a bug — but do not read the assertion below as promising
	// more than it proves.
	//
	// And note WHEN the interrupt lands, which is not when it is armed:
	// vm.Interrupt cannot preempt a blocking Go binding, so it is delivered
	// only once the VM regains control after the in-flight call returns.
	// Demonstrated, not assumed — a deliberately misconfigured 8s deadline
	// under a 5s budget was interrupted at 8s, not 5s. So a function's real
	// wall-clock ceiling is FunctionTimeout + OutboundTimeout. Still bounded,
	// which is the point, but bounded by the sum rather than by the budget.
	OutboundTimeout = 3 * time.Second

	// OutboundMaxInFlight caps concurrent outbound calls process-wide, so one
	// chatty domain cannot exhaust the core.
	OutboundMaxInFlight = 16

	// OutboundMaxBodyBytes caps a response body. goja holds it as a JS
	// string, so an unbounded body is an unbounded allocation.
	OutboundMaxBodyBytes = 1 << 20 // 1 MiB
)

// Compile-time assertion that ONE call's deadline is under the VM budget. If
// OutboundTimeout ever reaches FunctionTimeout this expression goes negative
// and converting it to uint fails to compile. The relationship is
// load-bearing, so the compiler checks it rather than a comment somebody has
// to notice.
//
// It proves arithmetic between two constants and nothing else. In particular
// it does NOT establish that a function cannot be interrupted mid-call: see
// OutboundTimeout's note about sequential calls.
const _ = uint(FunctionTimeout - OutboundTimeout - 1)

// OutboundDoer is the one call the `$http` binding needs. It is an interface
// so a dry run can install a refusing stub that never reaches the network.
type OutboundDoer interface {
	Do(ctx context.Context, req outbound.Request) (*outbound.Response, error)
}

// SetOutbound installs the bounded HTTP client backing `$http`.
//
// With no client installed, `$http` is simply absent — which is the default
// posture, and what every path sees unless --cqrsAllowOutboundHTTP is set.
//
// NOTE FOR HOT RELOAD: reload builds a FRESH GojaRuntime and copies the
// reader, store and registry onto it. This must be copied too, or `$http`
// would work until the first reload and then silently vanish.
func (r *GojaRuntime) SetOutbound(d OutboundDoer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outbound = d
}

// Outbound returns the installed client, or nil.
func (r *GojaRuntime) Outbound() OutboundDoer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.outbound
}

// newOutboundVM builds a VM for the paths permitted to reach the network:
// event- and cron-triggered effect functions, and reactors.
//
// NOT http-triggered functions, though those are effect-tier too. They are
// the only path driven by an inbound REQUEST, and request-driven concurrency
// is unbounded in a way consumer-driven concurrency is not — see the note in
// http.go. The effect tier is therefore not quite the unit of this grant,
// which is why this is named for the capability rather than for a tier.
//
// THE INSTALLATION IS ADDITIVE, AND THAT IS THE WHOLE POINT. Every other
// binding in this package is subtractive — newVMWithReader grants console and
// pb to everyone, and newDeciderVM then takes capability away. Outbound
// access cannot work that way: putting $http in the shared constructor would
// hand it to deciders and projections by default, leaving the purity
// invariant resting on somebody remembering to remove it in each of the
// places that build those VMs. Granting it here, and never naming it in the
// shared constructor, makes "deciders and projections cannot call out" true
// by construction rather than by vigilance.
//
// Concretely: newVM is now what every path WITHOUT outbound access uses —
// deciders, projections and http functions. Keep the grant here.
func (r *GojaRuntime) newOutboundVM(name string) (*goja.Runtime, *time.Timer) {
	vm, timer := r.newVM(name)
	if d := r.Outbound(); d != nil {
		vm.Set("$http", httpBinding(d))
	}
	return vm, timer
}

// httpBinding is the JS surface. It mirrors `pb`'s shape — a plain object of
// synchronous Go funcs — because goja has no event loop installed and an
// async surface would be a lie about what the call does to the VM.
//
// A returned Go error becomes a thrown JS exception, so a refusal or a
// timeout is catchable; an uncaught one dead-letters through the path
// effects and reactors already use.
func httpBinding(d OutboundDoer) map[string]any {
	fetch := func(opts map[string]any) (map[string]any, error) {
		req, err := requestFromJS(opts)
		if err != nil {
			return nil, err
		}
		// context.Background is correct: the client applies its own deadline,
		// and there is no caller context to inherit inside a VM.
		resp, err := d.Do(context.Background(), req)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"status":  resp.Status,
			"headers": resp.Headers,
			"body":    resp.Body,
		}, nil
	}

	return map[string]any{
		"fetch": fetch,
		"get": func(url string) (map[string]any, error) {
			return fetch(map[string]any{"url": url})
		},
		"post": func(url string, body string) (map[string]any, error) {
			return fetch(map[string]any{"url": url, "method": "POST", "body": body})
		},
	}
}

// requestFromJS converts the options object. It refuses what it cannot
// understand rather than quietly defaulting: a misspelled "URL" silently
// becoming a call to "" is the kind of quiet wrong-doing this codebase keeps
// paying for.
func requestFromJS(opts map[string]any) (outbound.Request, error) {
	var req outbound.Request
	if opts == nil {
		return req, fmt.Errorf("$http: an options object with a url is required")
	}

	url, ok := opts["url"].(string)
	if !ok || url == "" {
		return req, fmt.Errorf("$http: url is required and must be a string")
	}
	req.URL = url

	if m, present := opts["method"]; present {
		s, ok := m.(string)
		if !ok {
			return req, fmt.Errorf("$http: method must be a string, got %T", m)
		}
		req.Method = s
	}
	if b, present := opts["body"]; present {
		switch v := b.(type) {
		case string:
			req.Body = v
		case nil:
		default:
			return req, fmt.Errorf("$http: body must be a string; serialize objects with JSON.stringify")
		}
	}
	if h, present := opts["headers"]; present && h != nil {
		hm, ok := h.(map[string]any)
		if !ok {
			return req, fmt.Errorf("$http: headers must be an object, got %T", h)
		}
		req.Headers = make(map[string]string, len(hm))
		for k, v := range hm {
			s, ok := v.(string)
			if !ok {
				return req, fmt.Errorf("$http: header %q must be a string, got %T", k, v)
			}
			req.Headers[k] = s
		}
	}
	return req, nil
}

// dryRunOutbound refuses every call and names what it would have been.
//
// A dry run answers "what would this do?" and must not make it happen — the
// same reason mode=projection runs against an isolated reader rather than
// the live database. Reporting the intended call is strictly better than
// leaving $http undefined, which would fail the dry run with an error about
// the binding instead of an answer about the function.
type dryRunOutbound struct{}

func (dryRunOutbound) Do(_ context.Context, req outbound.Request) (*outbound.Response, error) {
	method := req.Method
	if method == "" {
		method = "GET"
	}
	return nil, fmt.Errorf(
		"$http is not called during a dry run; this would have sent %s %s", method, req.URL)
}

// DryRunOutbound returns the non-calling stub used by dry-run runtimes.
func DryRunOutbound() OutboundDoer { return dryRunOutbound{} }
