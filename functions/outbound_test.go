package functions

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dop251/goja"

	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/outbound"
)

// vmCtor is the shape every VM constructor in this package shares.
type vmCtor func(string) (*goja.Runtime, *time.Timer)

// typeOf runs `typeof $http` in a VM built by ctor and reports what it saw.
func typeOf(t *testing.T, ctor vmCtor) string {
	t.Helper()
	vm, timer := ctor("probe")
	defer timer.Stop()
	v, err := vm.RunString("typeof $http")
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}
	return v.String()
}

// THE REGRESSION TEST FOR THE INVARIANT: deciders and projections cannot
// reach the network, and no future edit may quietly give them the ability.
//
// It runs with an outbound client INSTALLED. With the client absent the whole
// thing would pass vacuously — every tier would report "undefined" and the
// test would prove only that the feature was switched off. That failure mode
// is not hypothetical here: this project has now been bitten three times by a
// fixture that could not fail (a smoke test that never reached the code it
// tested, a 4xx assertion against an empty log, a probe that only passed on a
// hand-built instance).
//
// The paired assertion on newOutboundVM is what keeps it honest: delete the
// binding entirely and the absence checks still pass, but that one fails.
func TestOutboundIsUnreachableFromDecidersAndProjections(t *testing.T) {
	rt := NewGojaRuntime(nil)
	rt.SetOutbound(DryRunOutbound()) // enabled, but never touches the network

	cases := []struct {
		tier string
		ctor vmCtor
		want string
	}{
		{"decider", rt.newDeciderVM, "undefined"},
		{"projection", rt.newVM, "undefined"},
		{"projection (isolated fixture run)", rt.newVMIsolated, "undefined"},

		// Not decoration: without this the test above passes on a build where
		// $http was never installed anywhere.
		{"effect/reactor", rt.newOutboundVM, "object"},
	}

	for _, tc := range cases {
		if got := typeOf(t, tc.ctor); got != tc.want {
			t.Errorf("%s: typeof $http = %q, want %q", tc.tier, got, tc.want)
		}
	}
}

// With the flag off there is no client, so no tier has the binding — not even
// the ones permitted to use it. Core's default posture is unchanged.
func TestOutboundAbsentWithoutAClient(t *testing.T) {
	rt := NewGojaRuntime(nil)
	for _, tc := range []struct {
		tier string
		ctor vmCtor
	}{
		{"decider", rt.newDeciderVM},
		{"projection", rt.newVM},
		{"effect/reactor", rt.newOutboundVM},
	} {
		if got := typeOf(t, tc.ctor); got != "undefined" {
			t.Errorf("%s: typeof $http = %q with no client installed, want undefined", tc.tier, got)
		}
	}
}

// An http-triggered function is effect-tier, but it does NOT get $http.
//
// It is the only function path driven by an inbound request. The consumer
// engine applies its consumers serially, so outbound concurrency from the
// event and cron paths is bounded by how many consumers exist; N simultaneous
// callers of /api/fn/x make N VMs, each able to block on a third party, which
// is the starvation the in-flight cap exists to prevent — and it is reachable
// unauthenticated under --cqrsAllowAnonymous.
func TestHTTPFunctionsDoNotGetOutbound(t *testing.T) {
	rt := NewGojaRuntime(nil)
	rt.SetOutbound(DryRunOutbound())

	src := `
function handle(request) { return { saw: typeof $http }; }
`
	result, err := rt.runHTTP("probe", compile(t, src), map[string]any{"method": "GET"})
	if err != nil {
		t.Fatalf("runHTTP: %v", err)
	}
	got, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("handle returned %T", result)
	}
	if got["saw"] != "undefined" {
		t.Fatalf("an http function saw $http as %v; request-driven outbound concurrency is unbounded", got["saw"])
	}
}

// The same property through the real decider path rather than the VM
// constructor, so the guarantee does not depend on which constructor a future
// edit happens to call.
func TestDeciderCannotSeeOutboundThroughTheRealPath(t *testing.T) {
	rt := NewGojaRuntime(nil)
	rt.SetOutbound(DryRunOutbound())

	src := `
function initialState() { return { saw: typeof $http }; }
function decide(command, state) { return []; }
function evolve(state, event) { return state; }
`
	spec := mkDeciderSpec(t, rt, "probe", src, nil, nil)
	got, err := rt.runInitial(spec)
	if err != nil {
		t.Fatalf("runInitial: %v", err)
	}
	state, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("initialState returned %T", got)
	}
	if state["saw"] != "undefined" {
		t.Fatalf("a decider saw $http as %v", state["saw"])
	}
}

// The effect tier can actually call out — the wiring works, not just the
// visibility check above.
func TestEffectCanCallOut(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Method
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	client, err := outbound.New(outbound.Config{
		AllowedHosts: []string{"127.0.0.1"},
		AllowPrivate: true,
		Timeout:      2 * time.Second,
		MaxInFlight:  4,
		MaxBodyBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}

	rt := NewGojaRuntime(nil)
	rt.SetOutbound(client)

	src := `
const res = $http.get(url);
if (res.status !== 200) { throw new Error("status " + res.status); }
if (JSON.parse(res.body).ok !== true) { throw new Error("body " + res.body); }
`
	if err := rt.runScript("probe", compile(t, src), map[string]any{"url": srv.URL}); err != nil {
		t.Fatalf("effect call failed: %v", err)
	}
	if got != "GET" {
		t.Errorf("server saw %q, want GET", got)
	}
}

// A refusal reaches the function as a catchable JS exception, so an author
// can handle it — and an uncaught one dead-letters through the path effects
// already use.
func TestRefusalIsACatchableJSError(t *testing.T) {
	client, err := outbound.New(outbound.Config{
		AllowedHosts: []string{"allowed.example.com"},
		Timeout:      time.Second,
		MaxInFlight:  1,
		MaxBodyBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	rt := NewGojaRuntime(nil)
	rt.SetOutbound(client)

	src := `
let caught = "";
try { $http.get("https://evil.example.com/x"); }
catch (e) { caught = String(e); }
if (!caught) { throw new Error("the refusal was not catchable"); }
if (caught.indexOf("allow-list") === -1) { throw new Error("unhelpful message: " + caught); }
`
	if err := rt.runScript("probe", compile(t, src), nil); err != nil {
		t.Fatalf("%v", err)
	}
}

// A reactor is effect-tier for this purpose and gets the binding too — that
// is the tier where "call out, then dispatch a command" lives.
func TestReactorHasTheBinding(t *testing.T) {
	rt := NewGojaRuntime(nil)
	rt.SetOutbound(DryRunOutbound())

	spec := &ReactorSpec{
		Reactor:    "probe",
		EventTypes: []string{"Thing"},
		Prog:       compile(t, `function reactTo(event) { return [{ saw: typeof $http }]; }`),
		runtime:    rt,
	}
	got, err := rt.runReactor(spec, events.Event{Type: "Thing", Position: 1, Data: []byte(`{}`)})
	if err != nil {
		t.Fatalf("runReactor: %v", err)
	}
	list, ok := got.([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("reactTo returned %T (%v)", got, got)
	}
	row, ok := list[0].(map[string]any)
	if !ok || row["saw"] != "object" {
		t.Fatalf("a reactor saw $http as %v", row)
	}
}

// A dry run must not make the call happen. It reports what would have been
// sent instead — leaving $http undefined would fail with an error about the
// binding rather than an answer about the function.
func TestDryRunRefusesAndNamesTheCall(t *testing.T) {
	rt := NewGojaRuntime(nil)
	rt.SetOutbound(DryRunOutbound())

	src := `
let msg = "";
try { $http.post("https://api.example.com/pay", "{}"); }
catch (e) { msg = String(e); }
if (msg.indexOf("dry run") === -1) { throw new Error("not reported as a dry run: " + msg); }
if (msg.indexOf("POST https://api.example.com/pay") === -1) { throw new Error("call not named: " + msg); }
`
	if err := rt.runScript("probe", compile(t, src), nil); err != nil {
		t.Fatalf("%v", err)
	}
}

func TestRequestFromJSRejectsWhatItCannotUnderstand(t *testing.T) {
	cases := []struct {
		name string
		opts map[string]any
	}{
		{"nil", nil},
		{"no url", map[string]any{"method": "GET"}},
		{"empty url", map[string]any{"url": ""}},
		{"url not a string", map[string]any{"url": 42}},
		{"method not a string", map[string]any{"url": "https://x.example", "method": 7}},
		{"body not a string", map[string]any{"url": "https://x.example", "body": map[string]any{}}},
		{"headers not an object", map[string]any{"url": "https://x.example", "headers": "a: b"}},
		{"header value not a string", map[string]any{"url": "https://x.example", "headers": map[string]any{"A": 1}}},
	}
	for _, tc := range cases {
		if _, err := requestFromJS(tc.opts); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}

	req, err := requestFromJS(map[string]any{
		"url": "https://x.example/a", "method": "PUT", "body": "hi",
		"headers": map[string]any{"X-A": "1"},
	})
	if err != nil {
		t.Fatalf("a well-formed request was rejected: %v", err)
	}
	if req.Method != "PUT" || req.Body != "hi" || req.Headers["X-A"] != "1" {
		t.Fatalf("converted badly: %+v", req)
	}
}
