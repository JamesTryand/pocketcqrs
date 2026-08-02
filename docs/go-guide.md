# Go guide

The platform itself is Go: the event store, the decider registry, the
consumers engine, the gateway, and the built-in domains (`aggregates/`,
`projections/`, `reactors/`). Write new built-in domains in Go when you want
compile-time safety; use [JS functions](js-guide.md) for runtime-defined
behavior.

## Layout

| package | role |
| --- | --- |
| `events` | append-only event store (`events.db`): streams, global positions, checkpoints, dead letters, meta kv, read-path upcasting |
| `decider` | the Decider pattern + registry (fold → decide → append) |
| `aggregates` | built-in domain deciders (task, order) |
| `consumers` | checkpointed poll engine shared by projections, reactors, effect functions |
| `projections` | Go projections into PocketBase collections |
| `reactors` | durable event→command mapping (sagas) |
| `gateway` | the command HTTP route |
| `writeguard` | rejects out-of-band writes on projection-owned collections |
| `functions` | the JS runtime (goja): effects, projections, deciders, schema reconcile, dry-run |
| `migrations` | PocketBase Go migrations — collections-as-DDL for built-in read models |

## A Go decider

```go
package aggregates

import (
	"pocketcqrs/decider"
	"pocketcqrs/events"
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

Register in `aggregates.RegisterAll` (called at bootstrap). The registry
handles concurrency: it loads the stream, folds `Evolve`, calls `Decide`,
and appends at the expected sequence — a stale fold is retried by the
caller on `events.ErrConcurrency` (gateway → 409).

Deciders are pure: they return events, never touch the app or the store.
Side effects belong to downstream consumers.

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

Register in `allProjections` (main.go) — the same list drives the engine
registration, the write-guard set, and `projection rebuild`. The target
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
