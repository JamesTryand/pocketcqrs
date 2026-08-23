# Go guide

The platform itself is Go: the event store, the decider registry, the
consumers engine, the gateway. Write your domains in Go when you want
compile-time safety; use [JS functions](js-guide.md) for runtime-defined
behavior.

The domains in `aggregates/`, `projections/orders.go` and
`reactors/fulfillment.go` are **examples**, not part of the platform — they
register only under `--tutorial`, and this guide uses them as worked
illustrations of what your own Go domain would look like.

## Layout

| package | role |
| --- | --- |
| `events` | append-only event store (`events.db`): streams, global positions, checkpoints, dead letters, meta kv, read-path upcasting |
| `decider` | the Decider pattern + registry (fold → decide → append) |
| `aggregates` | the **example** domain deciders (task, order) — registered only under `--tutorial` |
| `consumers` | checkpointed poll engine shared by projections, reactors, effect functions |
| `projections` | the `Projection` interface and write-guard helper, plus the example `tasks`/`orders` projections (`--tutorial`) |
| `reactors` | durable event→command mapping (sagas): the generic tier, plus the example fulfillment saga (`--tutorial`) |
| `gateway` | the command HTTP route |
| `writeguard` | rejects out-of-band writes on projection-owned collections |
| `outbound` | the hard-bounded HTTP client behind `$http`; usable from Go too |
| `functions` | the JS runtime (goja): effects, projections, deciders, schema reconcile, dry-run |
| `migrations` | PocketBase Go migrations — collections-as-DDL for Go read models (this repo's are the examples', registered under `--tutorial`) |

## Converting a domain from JS to Go

Every domain that starts as JS functions can move to Go later — this is a
deliberate, supported progression: prototype fast with hot-reloadable JS,
then graduate the proven domain to a compiled Go aggregate once its rules
have settled. Whether a given tier has a direct Go counterpart determines
what "converting" actually involves:

| JS side | Go equivalent | What converting means |
| --- | --- | --- |
| Decider (tier 3) | `decider.Decider[S]` | Structural peer — port `initialState`/`decide`/`evolve` to a typed state struct, then `decider.Register(registry, "yourAggregate", YourDecider())` at bootstrap |
| Projection (tier 2) | `projections.Projection` | Structural peer — port `project()` to `Apply`; the `//@schema` collection becomes an ordinary PocketBase migration instead |
| Reactor (tier 4) | `reactors.Reactor` | Structural peer — port `reactTo()` to `React()`; both dispatch through the same `reactors.Dispatch` |
| Effect (tier 1), `//@trigger event` | no named type | Implement `consumers.Consumer` directly and register it with the engine — there's nothing to port *to*, just a Go type doing the same job |
| Effect (tier 1), `//@trigger http` | no named type | Add a route directly via `core.ServeEvent.Router`, same as this project's own routes (`gateway/gateway.go`, `ops.go`) |
| Effect (tier 1), `//@trigger cron` | no named type | Call `app.Cron().Add(...)` directly in your bootstrap code |

Decider/projection/reactor are true peers because both languages end up on
the same registry/engine underneath — a JS decider and a Go decider are
indistinguishable to the rest of the system. The effect tier's three
triggers have no Go counterpart to port *to*: they are named JS concepts
only because a `.js` file has no other way to reach `Consumer` registration,
the router, or the cron scheduler. Go code already has direct access to all
three, so there's nothing to convert — just write the Go call directly.

**Register your own domain at bootstrap, not in the example wiring.** The
platform ships no aggregates or projections of its own:
`aggregates.RegisterAll` and `allProjections` are the *examples'*
registration and both are gated behind `--tutorial` (`main.go`,
`projection_cmd.go`). Adding your decider or projection inside either means
it only loads when the tutorial flag is set. Register alongside them
instead.

**A word of caution on scope**: converting a decider is a rewrite, not a
migration. There is, deliberately, no automatic JS→Go transpiler — the
generated JS skeleton was always a starting point whose actual rules the
author wrote by hand, and that authored logic is exactly what has to be
re-expressed in Go, not mechanically translated. Use the live JS file as
your reference while writing the Go version, dry-run and test the Go
version independently, and only then remove the JS file — the registry
refuses a name collision between a JS and a Go decider, so a partial
migration is caught immediately rather than silently.

**`pocketcqrs schema import --lang go`** scaffolds the Go starting point
directly from an EventModeling document, alongside the existing (default)
JS output — same model, same validation, second output language. It writes
one file per decider/projection/reactor (all in one package, named after
the aggregate) and prints suggested `decider.Register`/`engine.Register`
lines for `main.go` — printed, not applied, for the same reason
`aggregates.RegisterAll` isn't patched automatically: a one-line edit isn't
worth code-mod machinery, and there's no "graduated" bookkeeping to keep in
sync — the registry's own JS/Go name-collision refusal already makes a
partial migration visible. This is scaffolding for a document you're
importing fresh, not a converter for an existing hand-written JS file: the
same "starting point, not a translation" caveat above applies to its output
exactly as it does to the JS scaffolder's.

Two folds worth knowing about before you rely on the generated Go: a
`//@schema date` field becomes `time.Time`, which `encoding/json` unmarshals
strictly as RFC3339 — a caller sending PocketBase's own space-separated
date format will get a rejected command where the JS decider's untyped
field would have passed it through. And the generated reactor's dispatched
command payload is `ev.Data` untouched (matching the JS scaffolder's
`Object.assign({}, event.data)`, field-for-field) — this only differs if an
event's stored `Data` is nil rather than `{}`, which nothing in this
codebase's own event construction currently does.

See [js-guide.md](js-guide.md) for the JS-side detail on each tier.

## A Go decider

```go
package aggregates

import (
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

type taskState struct {
	Exists, Completed bool
	Title             string
}

func Task() *decider.Decider[taskState] {
	return &decider.Decider[taskState]{
		InitialState: func() taskState { return taskState{} },
		Decide: func(cmd decider.Command, s taskState) ([]events.NewEvent, error) {
			switch cmd.Name {
			case "CreateTask":
				if s.Exists {
					return nil, errors.New("task already exists")
				}
				return []events.NewEvent{{Type: TaskCreated, Data: ...}}, nil
			// ...
			}
		},
		Evolve: func(s taskState, ev events.Event) (taskState, error) {
			switch ev.Type {
			case TaskCreated:
				s.Exists = true
			}
			return s, nil
		},
	}
}
```

Register with `decider.Register(registry, "task", Task())` on the registry
`main.go` builds at bootstrap. (`aggregates.RegisterAll` does exactly this
for the two example aggregates, but is only called under `--tutorial` — so
register your own next to that call, not inside it.) The registry
handles concurrency: it loads the stream, folds `Evolve`, calls `Decide`,
and appends at the expected sequence — a stale fold is retried by the
caller on `events.ErrConcurrency` (gateway → 409).

Deciders are pure: they return events, never touch the app or the store.
Side effects belong to downstream consumers.

### The calling actor: `Command.Actor` / `Command.Now`

`cmd.Actor` is the authenticated caller's PocketBase auth-record id (empty
for an anonymous or meta-less call); `cmd.Now` is the same stamped timestamp
the registry records into the produced events' metadata — both populated by
`Register[S]` from the `meta` map `HandleWithMeta`/the gateway supply,
mirroring exactly what a JS decider's `command.actor`/`command.now` binding
already receives ([js-guide.md](js-guide.md#deciders-tier-3)):

```go
case "GrantRotaPermission":
	if cmd.Actor == "" || !s.PermissionHolders[cmd.Actor] {
		return nil, errors.New("actor lacks permission")
	}
	// ...
```

Use `cmd.Actor` for authorization the same way a JS decider does — checking
who's calling against folded state — and `cmd.Now` instead of `time.Now()`
for anything time-stamped, for the same determinism reason JS deciders are
barred from `Date` entirely: a Go decider *can* call `time.Now()` (nothing
stops it), but doing so breaks the same replay-reproducibility guarantee, so
treat the ban as a convention worth keeping even though the compiler won't
enforce it.

`cmd.Provenance` is a separate, narrower signal: empty for everything the
gateway and local reactors produce today, and set only by a trusted local
write path that has verified a command's causal chain crossed a trust
boundary (e.g. a federated peer deployment, once that exists — see
platform/pocketbase-cqrs-faas NEEDS.md's federation trust model item). Do
**not** infer trust from `cmd.Actor`'s `"reactor:<name>"` prefix — that
string only says which reactor produced the command, unconditionally, and is
identical whether the reactor reacted to a purely local event or one that
originated at an independently-administered peer. A decider that wants to
grant reactor automation elevated trust should check `cmd.Provenance`
instead: it is empty unless something explicitly vouched for the command's
origin.

A third shape, `"extcall:<name>"`, marks a command dispatched over the HTTP
gateway by a recognized **external-caller** identity — a record in the auth
collection named by `gateway.Config.ExternalCallerCollection` (e.g.
`pocketcqrs-extensions`' `extcaller`), not a plain user id or a local
reactor. Like `"reactor:<name>"`, this is derived server-side from the
authenticated caller's own collection membership, never from anything the
request supplies — a caller cannot claim this label without holding the
credential for a record already in that collection. Treat it the same way
as `"reactor:"`: a naming convention for observability
(`events/stats.go`'s `ReactorFlows` recognizes both prefixes), not a trust
marker — check `cmd.Provenance` for that, exactly as above.

## A Go projection

```go
type tasksProjection struct{ app core.App }

func (tasksProjection) Name() string          { return "tasks" }        // durable checkpoint key
func (tasksProjection) Collections() []string { return []string{"tasks"} } // write-guarded

func (p tasksProjection) Apply(ctx context.Context, ev events.Event) error {
	ctx = writeguard.MarkInternal(ctx) // projection writes pass the write-guard
	// fold ev into the collection; MUST be idempotent (at-least-once delivery)
}
```

Register it in the list that drives engine registration, the write-guard set
and `projection rebuild` — one list for all three, so a projection cannot be
consuming events while its collections sit unguarded. `allProjections`
(`projection_cmd.go`) is that list, but it returns nothing unless
`--tutorial` is set, because every Go projection in this repo is example
content; add yours unconditionally rather than inside that branch. The target
collection is created by a **PocketBase Go migration** in `migrations/`
(collections-as-DDL), not by the projection.

Rebuild offline: `pocketcqrs projection rebuild <name>` — wipe, reset
checkpoint, replay.

## A reactor (saga)

```go
type fulfillment struct{}

func (fulfillment) Name() string { return "fulfillment" } // checkpointed as "reactor:fulfillment"

func (fulfillment) React(ev events.Event) []reactors.Reaction {
	if ev.Aggregate != aggregates.OrderAggregate || ev.Type != aggregates.OrderConfirmed {
		return nil
	}
	payload, _ := json.Marshal(map[string]string{"title": "fulfil order " + ev.AggregateID})
	return []reactors.Reaction{{
		Aggregate: aggregates.TaskAggregate,
		ID:        "fulfill-" + ev.AggregateID, // deterministic id = idempotency
		Command:   decider.Command{Name: aggregates.CmdCreateTask, Payload: payload},
	}}
}
```

(`reactors.Reactor` is the interface: `Name()` + `React(ev) []Reaction`;
`reactors.AsConsumer(reactor, registry, logger)` adapts it to the engine.)

Reactors are durable consumers mapping committed events to follow-up
commands, dispatched **through the registry** — reactions become events like
everything else, with `causationId`/`correlationId` metadata and
`actor=reactor:<name>`. Domain rejections are logged and skipped (use
deterministic target ids so retries are no-ops); concurrency conflicts are
retried.

## Calling out from Go

The `outbound` package is the same bounded client that backs the JS `$http`
binding, exported so Go reactors, consumers and out-of-process components
share one implementation of the guardrails rather than each growing their own.

```go
client, err := outbound.New(outbound.Config{
    AllowedHosts: []string{"api.example.com"}, // exact hosts, no wildcards
    Timeout:      3 * time.Second,
    MaxInFlight:  16,
    MaxBodyBytes: 1 << 20,
})
resp, err := client.Do(ctx, outbound.Request{Method: "POST", URL: "...", Body: "..."})
```

It enforces the allow-list before any I/O, re-checks the **resolved IP** at
dial time (link-local always refused, private ranges only with
`AllowPrivate`), never follows redirects, makes exactly one attempt, and caps
both concurrency and response size. Refusals are distinguishable:
`errors.Is(err, outbound.ErrHostNotAllowed)` and friends.

**Be honest about what this is.** For Go callers it is a *convention*, not an
enforcement — Go code can always reach `net/http` directly, and nothing stops
it. The enforcement is real only for the JS tiers, where the binding is the
only door. Use it anyway: a Go consumer that calls out without a timeout or a
cap is the blast radius the primitive exists to bound, and the fact that the
compiler won't stop you is not a reason to reimplement the same six rules
slightly differently.

Same rule as JS about **where** it may run: downstream of committed events
only. A `Decide` or `Evolve` that reaches the network destroys replay
reproducibility, which is the guarantee everything else rests on.

## Bootstrap order (main.go)

1. `RunAppMigrations` — collections-as-DDL before anything touches them
2. open `events.db` → registry with Go deciders → consumers engine
3. `functions.LoadDir` → validate JS deciders → register validated ones
4. `store.SetUpcaster(BuildUpcaster(validated))` — store-level upcasting
5. `ReconcileSchemas` — JS projection schemas (additive, two-pass)
6. engine: Go projections → JS projections → reactors → effect functions
7. write-guard over all projection-owned collections; cron jobs
8. gateway + `/api/fn` + `/api/cqrs/admin/reload` routes on serve

## Testing

- Deciders: pure unit tests over `Decide`/`Evolve` (see `aggregates/*_test.go`).
- Registry/store: real `events.Open` on a temp file.
- Projections/reconcile: `github.com/pocketbase/pocketbase/tests` —
  `tests.NewTestApp()` gives a full app with temp data dirs.
- Smoke: build `pocketcqrs.exe`, serve against a scratch `--dir`, drive HTTP.
