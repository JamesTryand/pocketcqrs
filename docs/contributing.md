# Contributing

## Ground rules (the architecture in three sentences)

The event log is the only source of truth — state changes are commands,
commands become events, events become read models. Reconciliation is
additive everywhere: events are append-only (properties may be added, never
removed — shape changes are new event types plus upcast transforms), and
collection schemas are create/extend-only (never drop, retype or rename).
Projection-owned collections are read-only for everyone; the only writers
are projections via an internal marker.

Keep those three intact and most design decisions make themselves.

## Build, test, smoke

```sh
go build ./...
go vet ./...
go test ./...
go build -o pocketcqrs.exe .   # for smoke tests
```

- Unit tests live next to their packages (`*_test.go`); projection/schema
  tests use `github.com/pocketbase/pocketbase/tests` (`tests.NewTestApp()`).
- Smoke tests: serve against a throwaway `--dir` with a scratch
  `--functionsDir`, drive it over HTTP, then delete the scratch dir. The
  convention so far: detached serve process, a throwaway superuser
  (`smoketest@example.com`), verify behavior via the public API (not logs).

## Conventions

- **Commits**: milestone-prefixed subjects (`M8.5: ...`), body explains the
  why. The durable plan lives in the issue worktree, not here.
- **Go style**: standard `gofmt`; small packages with one job; comments
  explain invariants and semantics, not mechanics.
- **Migrations**: Go read-model collections are PocketBase Go migrations in
  `migrations/` (collections-as-DDL); JS projection collections come from
  `//@schema` and never from migrations. This repo's migrations belong to
  the examples, so they register from `migrations.RegisterExamples` rather
  than an `init()` — gating inside an `up` would record the migration as
  applied and make `--tutorial` a one-way door.
- **Registration surfaces**: a Go *example* domain touches
  `aggregates.RegisterAll` (decider), `exampleProjections` in `examples.go`
  (projection + write-guard), and usually a migration — all three gated
  behind `--tutorial`. Someone adding a Go domain to *their own* build
  registers it outside that gate; see
  [go-guide](go-guide.md#converting-a-domain-from-js-to-go). JS-defined
  domains need no Go changes.
- **Example content stays opt-in**: anything that only exists to teach goes
  behind `--tutorial`, and `examples.go` is where its wiring lives. The
  platform must boot with nothing registered.
- **Docs**: user-visible behavior changes update `docs/` in the same
  commit — including the affected [domain docs](domains/README.md).

## Trust model for functions (deferred decision)

v1 assumes **trusted, owner-authored** JS (goja in-process). The
`functions.Runtime` interface is deliberately language/runtime-agnostic so
an isolated runtime (wasm/process) can slot in when untrusted tenant code
arrives — that is a post-slice decision gate, not current scope.

`$http` (see the [JS guide](js-guide.md#calling-out-http-event-cron-and-reactor-functions)) is
the one capability that reaches outside the process, and it narrows that
assumption rather than resting on it. Its allow-list is **deployment-wide,
not per-function**, precisely because function code is hot-reloadable and has
no code-review gate: a per-function list would be authored by whoever wrote
the call it authorises. If you add another outward-facing capability, hold
the same line — the boundary belongs to the operator at boot, not to the
function.

Two rules follow for anyone touching `functions/`:

- **Capability grants to the JS tiers are additive.** `newVMWithReader`
  grants what every path may have; `newOutboundVM` layers on what only the
  permitted ones may have. Never put an outward-facing binding in the shared
  constructor and subtract it again — that leaves decider and projection
  purity depending on someone remembering the subtraction.
  `TestOutboundIsUnreachableFromDecidersAndProjections` fails if you do.
- **The unit of an outward grant is a path, not a tier.** `$http` goes to
  event and cron functions and to reactors, but *not* to http-triggered
  functions, even though those are effect-tier too. The consumer engine
  applies consumers serially, so consumer-driven outbound concurrency is
  bounded by the consumer count; request-driven concurrency is bounded by
  nothing. Ask which *drives* a path before granting it anything outward.
- **A hot reload builds a fresh runtime.** `reload.go` copies the reader,
  store, registry and outbound client onto it. A new capability that is not
  copied there works until the first reload and then silently vanishes.

## Relationship to PocketBase

PocketBase is an **unmodified dependency** pinned in `go.mod` (release tag,
not master). Do not patch its internals; if a hook ceiling ever forces the
issue, that is a recorded decision gate (hard fork), not a drive-by change.
Upstream (`pocketbase/pocketbase`) does not accept LLM contributions — do
not open PRs or issues there.
