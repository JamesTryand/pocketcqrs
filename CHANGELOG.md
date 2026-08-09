# Changelog

All notable changes to PocketCQRS. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/); versions match git tags.

## v0.5.0 — a door to the outside, with bounds

Calling a third party is common enough that "install a second component" was
the wrong default for it — but an unbounded call from inside the process is a
blast radius nobody chose. This release adds the narrowest primitive that
makes the common case work: off unless asked for, one deployment-wide list of
permitted destinations, and every failure mode already bounded.

Nothing changes unless you pass a flag. Deciders and projections cannot reach
the network, and now cannot be given the ability by accident either.

### Added

- **Bounded outbound HTTP for event, cron and reactor functions**, behind
  `--cqrsAllowOutboundHTTP` (off by default). Calling a third party is common
  enough that "install a second component" was the wrong default for it, but
  an unbounded call from inside the process is a blast radius nobody chose.
  So the primitive is deliberately narrow, and every bound is enforced rather
  than documented:

  - a **deployment-wide** host allow-list from `--cqrsOutboundHost`
    (repeatable), checked before any I/O and before DNS. Global rather than
    per-function on purpose: function code is hot-reloadable with no
    code-review gate, so a per-function list would be written by whoever
    wrote the call. **An empty list permits nothing.**
  - the **resolved IP** is re-checked at dial time, so a hostile resolver
    cannot aim an allow-listed name at loopback or at `169.254.169.254`.
    Link-local is refused with no override; loopback and private ranges need
    `--cqrsAllowPrivateOutbound` (dev and internal services).
  - redirects are **never followed** — a 3xx would otherwise walk a call
    straight off the allow-list.
  - one attempt, no retry; failures dead-letter through the path effects and
    reactors already use, and the checkpoint still advances.
  - a process-wide in-flight cap (saturated ⇒ wait out your own deadline,
    then fail) and a response body cap that **errors rather than truncating**.

  **Deciders and projections never get it**, whatever the flags say. The
  binding is installed additively, on the permitted VMs only, so the purity
  invariant holds by construction rather than by remembering to subtract it;
  a regression test runs with the client installed and fails if any future
  edit moves the grant into the shared VM constructor.

  **`//@trigger http` functions do not get it either**, though they are
  effect-tier like `event` and `cron`. They are the only path driven by an
  inbound request: the consumer engine applies consumers serially, so
  consumer-driven outbound concurrency is bounded by the consumer count,
  while N simultaneous callers of `/api/fn/x` make N VMs each able to block
  on a third party. A route that needs a third party should record an event
  and let an effect or reactor make the call — durable and retryable, rather
  than tied to a request that may already have timed out.

  A dry run does not call out — it reports what would have been sent. The
  bounded client is also exported as the `outbound` Go package.

  *One limit worth knowing*: the 3s call deadline sits under the 5s function
  budget, so a single slow call fails as a catchable error rather than a VM
  interrupt. The budget is armed once per execution, though, so **two
  sequential slow calls still exhaust it**. And the budget cannot cut short a
  call already in flight — `vm.Interrupt` is delivered only when the VM
  regains control — so a function's worst-case wall clock is **5s + 3s**, not
  5s. Chain calls sparingly.

- `docs/go-guide.md`: **"Converting a domain from JS to Go"** — the JS→Go
  graduation path is a deliberate, supported progression (prototype in
  hot-reloadable JS, compile the proven domain once its rules settle) and no
  document described it. A per-tier table says which tiers are structural
  peers (decider, projection, reactor — both languages land on the same
  registry/engine) and which have no Go counterpart to port *to*: the effect
  tier's three triggers are named JS concepts only because a `.js` file has
  no other way to reach `Consumer` registration, the router or the cron
  scheduler. `docs/js-guide.md` links to it.

  It states the scope honestly: converting a decider is a rewrite, not a
  migration, and there is deliberately no JS→Go transpiler. A partial
  migration is caught rather than silent, because the registry already
  refuses a name collision between a JS and a Go decider.

- `docs/reference/cli.md`, `docs/reference/directives.md` and
  `docs/contributing.md` document the flags, the per-trigger binding surface
  and the rule this establishes: **an outward-facing boundary belongs to the
  operator at boot, not to the function.**

### Changed

- `docs/js-guide.md` said reactor `Math.random` seeding existed "because an
  at-least-once **replay** must decide the same thing twice". Reactors are
  not re-run on replay — `projection rebuild` replays into one named
  projection and there is no global replay. What the seeding actually guards
  is **crash-recovery redelivery**: the consumer engine runs `Apply` and then
  advances the checkpoint, so a crash between the two re-runs the reactor.
  Corrected, and the distinction now spelled out where it matters.

### Fixed

- **`docs/go-guide.md` told you to register your own Go domain in the
  example wiring**, which v0.4.0 had put behind `--tutorial`. It named
  `aggregates.RegisterAll` for deciders and `allProjections` for projections;
  both are gated (`main.go`, `projection_cmd.go:23`), so following the guide
  gave you a decider or projection that silently did not load without the
  tutorial flag. The guide now names the real call
  (`decider.Register(registry, ...)` at bootstrap) and says explicitly that
  the example wiring is example-only. `allProjections` was also cited as
  living in `main.go`; it is a method in `projection_cmd.go`.

## v0.4.0 — the platform ships empty

A framework should not create collections nobody asked for. Until now
`pocketcqrs` registered its own example domains unconditionally, so
installing the binary and running it gave you a `task` aggregate, an `order`
aggregate and three collections you never chose. They are teaching material,
and they are now opt-in.

Nothing was removed: everything still ships, `--tutorial` turns it on, and
the docs walk through it exactly as before.

### Changed

- **BREAKING: the example domains are opt-in.** PocketCQRS now boots empty.
  The `task` and `order` Go aggregates, their projections, the fulfillment
  saga and the `tasks`/`orders`/`order_lines` collection migrations register
  only under the new **`--tutorial`** flag. Installing the binary and running
  it no longer creates collections nobody asked for.

  *Migrating*: add `--tutorial` to any command that depended on them. An
  instance that already created those collections keeps them **and keeps
  their write-guard** — losing a protection silently would be worse than
  never having it — and boot logs a warning naming them. Because an
  unregistered migration is never *recorded* either, the switch works in both
  directions: turn the flag back on and the collections are created as
  before.

  Also: the names `task` and `order` are now free for your own JS deciders,
  which a colliding registration previously refused.

- **BREAKING: the example function files moved to `examples/pb_functions/`.**
  `pb_functions/` is now yours and is empty in git; `--functionsDir` still
  defaults to it. Copy what you want:
  `cp examples/pb_functions/*.js pb_functions/`. Serving straight out of
  `examples/` is deliberately not the documented path — the functions editor
  and the reload endpoint write into `functionsDir`, so pointing it at
  tracked files would dirty the tree on every save.

### Added

- `examples/pb_functions/task_completion_note.js`: a shipped JS reactor example.
  `TaskCompleted` (Go `task` aggregate) dispatches `CreateNote` (JS `note`
  aggregate) — the reactor tier previously had no runnable example in the
  shipped fixture set, only the abstract one in the directive reference.
  Wired into `getting-started.md`'s walkthrough. Documented in
  `docs/domains/task.md` and `docs/domains/note.md`.

### Fixed

- **`writeguard.Register` with an empty collection list denied every record
  write in the app** rather than none. PocketBase treats a tagged hook with
  no tags as matching every collection, so guarding nothing guarded
  everything — superuser creation included. Reachable before this release:
  `reload` re-registers the guard from the JS projections it just loaded, so
  the ordinary maintenance-on/reload/maintenance-off dance on an instance
  with no JS projections left every collection write-denied until restart.

- A latent race in the `TestScaffoldedSliceWorks` smoke test, which waited
  for the projected row to *exist* and then asserted a field the second
  event sets — so it raced the update and could report a merge bug that was
  not there. It now waits for the row's final state.

### Docs

- `docs/tutorial.md` boots with `--tutorial`, which its central lesson
  depends on: the walkthrough exists to show a generated `order.js`
  colliding with a Go `order` aggregate, and with nothing registered there
  is no collision to see. The point is generalised rather than tied to the
  examples — *any* Go aggregate registered at boot claims its name ahead of
  JS.
- `docs/getting-started.md` opens on the two-part opt-in (a flag for the Go
  domains, a copy for the JS files) and says plainly that a plain `serve` is
  an empty platform, which is the intended production shape.
- `examples/pb_functions/README.md` is new: what each example file is, and
  which of them need `--tutorial` (only `note.js` + `notes.js` stand alone).
- `--tutorial` documented in the CLI reference with both directions of the
  switch; `go-guide.md` and `contributing.md` corrected where they described
  example content as built-in.

Gates: `go vet` clean, `gofmt` clean, 164 unit tests / 16 packages, 16 smoke
tests, six browser probes.

```sh
go install github.com/jamestryand/pocketcqrs@v0.4.0
go install github.com/jamestryand/pocketcqrs/pocketcqrs-dashboard@v0.4.0
```

## v0.3.0 — M13 complete, then M14: EventModeling import/export and the reactor tier

Two milestones landed in this range: DASH.6 finished M13, then M14 built on
top of it. The tag annotation on `v0.3.0` covers only M14 — DASH.6 belongs to
this range too and is recorded here.

### Added

- **Domain scaffolder** (`scaffold` package, `POST /api/cqrs/admin/scaffold`,
  the dashboard's `/scaffold` wizard): generates a JS decider and JS
  projection from a description of a slice (aggregate, commands, payload
  fields, read model). Writes nothing — generated source goes through the
  same load check, dry run and save path as hand-written code. Built around
  an intermediate `scaffold.Domain` model shared with the M14 importer, so
  the wizard and the importer stay one generator, not two.
- **Declared-command surface**: `//@commands` (JS) and `Commands()` (Go)
  reach the catalog JSON, the catalog Markdown and the domain-doc skeleton's
  command table — the one part of a slice that leaves no trace in the log,
  so without this an export could not name a slice's commands at all.
- **JS reactor tier** (tier 4): `//@trigger reactor` + `//@dispatches`. A
  durable consumer mapping events to **commands**, dispatched through the
  decider registry rather than appended directly, so a reaction stays a
  decision that can be refused. Shares one dispatch implementation
  (`reactors.Dispatch`) with the Go `reactors` package. Activates in running
  mode (no schema, no maintenance barrier); checkpoints under
  `fn-reactor:<name>`, distinct from Go reactors' `reactor:<name>`, while
  stamping the same `reactor:<name>` metadata actor so catalog flow
  detection works for both. Originally named `//@trigger react` /
  `react(event)`; renamed to `reactor`/`reactTo` before the tag because the
  old name read as the React JS framework, which this project does not use.
- **`pocketcqrs schema import|export`** for eventmodelschema 2.0.0 documents.
  `import` maps a document (single file or split manifest) onto the same
  `scaffold.Domain` model the wizard uses, **runs the document's own
  scenarios** against the generated code rather than just reading them, and
  reports every decision taken on the document's behalf (untagged
  aggregates, synthesized swimlanes, lossy fields). `export` reconstructs a
  document from the running catalog, synthesizing the required-but-absent
  design-time pieces (a swimlane, a screen per screen-bearing slice, default
  status) rather than emitting an invalid one.
- **`//@produces <Command> <Event...>`**: records which command appends
  which events. `//@commands` and `//@handles` each named one side and
  nothing joined a pair, so an export previously had to widen every
  command's `eventIds` to its aggregate's whole event set. Contradictions
  across a file are refused at load; the scaffolder and importer emit it
  automatically, so generated code round-trips faithfully.
- `mode=reactor` dry run (`POST /api/cqrs/admin/dryrun`, `dryrun` CLI has no
  reactor subcommand yet, HTTP/dashboard only): replays matching history
  through `reactTo(event)` with no registry installed, so nothing can be
  dispatched even by accident.
- `mode=projection` accepts a supplied fixture with reads isolated from live
  collections — the two-step a `stateView` scenario needs (extend, then
  compare), rather than a read-modify-write projection answering from the
  real database about rows a fixture never described.

### Changed

- **A refused `decide` dry run is a verdict, not an error**: `DryRunDecide`
  now returns `*DecideDryRun{Produced, Rejected, Message}`, and the HTTP/CLI
  dry run reports a refusal as `200`/`{ok:false, rejected:true, message}`
  (CLI: prints the refusal, exits `0`) instead of a bare error. The status
  reports whether the simulation ran, not the verdict — the same shape
  already used for a failed dead-letter retry.
- `scaffold.Command.Event string` → `Events []Event`: a command may record
  several events (the model records **what can result**, not how the result
  is chosen — that's `decide()`'s job). Event payloads are explicit per
  event and never inherited from the command; `NoFields` says "this event
  genuinely carries nothing" so it can't be confused with "unspecified".
  `Warnings()` reports what a description left undecided alongside
  `Validate()`.
- Domain scenarios are **run**, not just read: importing a document executes
  its given/when/then against the code it just generated. Caught two bugs in
  the importer itself (a fixed fixture stream id; foreign trigger events
  seeded onto the wrong stream) and one real mapping bug (a
  cross-aggregate automation opens a new stream per trigger, so its command
  is a create).

### Housekeeping

- Module path lowercased: `github.com/JamesTryand/pocketcqrs` →
  `github.com/jamestryand/pocketcqrs` (the module proxy escapes the
  uppercase account name and Go tooling handles it badly).
- `testdata/eventmodelschema/` vendors both eventmodelschema documents and
  worked examples, pinned by hash in `PROVENANCE.md` — the round-trip loss
  test needs a real document, and a scratch clone doesn't survive between
  sessions. Tracking eventmodelschema **2.0.0** as of this release.

Gates: `go vet` clean, `gofmt` clean, 164 unit tests / 16 packages, 12 smoke
tests, six browser probes.

```sh
go install github.com/jamestryand/pocketcqrs@v0.3.0
go install github.com/jamestryand/pocketcqrs/pocketcqrs-dashboard@v0.3.0
```

## v0.2.0 — the ops dashboard becomes operational

Everything since v0.1.0. The platform was complete at v0.1.0; this release
is about operating it: **DASH.3** (browse), **DASH.4** (act), **DASH.5**
(edit), plus hardening the gates that caught the worst bugs along the way.

### Added

- **DASH.3 — browsing**: bidirectional event feed (position-based paging,
  never an offset — positions are `AUTOINCREMENT` and burn on conflict
  retries), the catalog explorer drawn in event-modeling notation
  (events/read models/aggregates/consumers as distinct node shapes, reactor
  flows as dashed edges **observed in the log**, not declared).
- **DASH.4 — acting**: a System page (maintenance barrier + hot reload with
  its report), dead-letter retry/dismiss/retry-all, htmx polling for live
  tables with out-of-band updates for figures outside them, and
  `docs/consuming.md` (three deployment patterns with Caddyfiles). A failed
  retry is a `200`, not a `4xx` — a poison event staying poison is the
  ordinary case, and the UI has to tell it from a broken endpoint.
- **DASH.5 — the function editor**: `GET|PUT|DELETE
  /api/cqrs/admin/functions[/{name}]` and `POST /api/cqrs/admin/dryrun`,
  with a CodeMirror editor UI over them — edit, dry run against real
  history, save, activate through the barrier. A write is refused unless
  the source loads (reloads are all-or-nothing, so one bad save would block
  every later reload including the fix); a save keeps the replaced version
  and offers **Load previous version**.
- **Hardened gates**: a `smoke` build-tag suite that builds both binaries
  and drives them over real HTTP (now in CI, with server logs attached to
  failures), the browser probes checked in and made self-sufficient (each
  installs its own fixtures through the admin API rather than depending on
  a hand-prepared instance), and a `gofmt` CI check.

### Fixed

- A projection returning plain objects instead of row ops wrote nothing,
  forever, unlogged; `normalizeOps` now counts what it discards and the dry
  run reports it, so a mistake gets named instead of inferred from a zero.
- A cron+projection file silently dropped its cron trigger — pinned by a
  test as decided behavior (single-purpose means single-purpose), not
  deferred again.
- A function save was a bare overwrite with no undo.

Gates: `go vet` clean, **123 unit tests / 14 packages**, 11 smoke tests, five
checked-in browser probes — verified at the actual `v0.2.0` commit
(`9ccb5d3`). DASH.6 (domain scaffolder, declared-command surface), which
landed two commits later at `dcc6d14`/`34c2296`, is **not** part of this
release — see the `v0.3.0` entry above.

```sh
go install github.com/jamestryand/pocketcqrs@v0.2.0
go install github.com/jamestryand/pocketcqrs/pocketcqrs-dashboard@v0.2.0
```

## v0.1.0 — M1–M11 platform, M12/DASH.0–2 ops dashboard skeleton

The write side, the read side, the FaaS runtime, and domain packs — the
whole platform — plus the first two steps of the ops dashboard.

### Added

- **M1 — the vertical slice**: event store (`events.db`, separate from
  PocketBase's `data.db`), the decider core
  (`InitialState`/`Decide`/`Evolve`), the `task` aggregate, and the command
  gateway (`POST /api/cqrs/{aggregate}/{id}/{command}`).
- **M2 — hardening the gateway and the runtime**: auth required on commands
  by default, actor stamped into event metadata (M2.1); a generalized
  checkpointed-consumer engine for durable, at-least-once consumption
  (M2.2); HTTP-triggered functions and read-only query bindings (M2.3–2.4);
  `projection rebuild` (wipe + checkpoint reset + replay) (M2.5).
- **M3 — projections and sagas**: the `order` aggregate, reactor (saga)
  infrastructure and the fulfillment saga (M3.1); JS projections with
  `//@schema` directives and boot-time additive reconciliation (M3.2).
- **M4 — JS deciders** (tier 3): a neutered VM (no `Date`, no `Math.random`,
  no `pb`), event versioning and `//@transform` upcasters.
- **M5 — cron-triggered functions** (`//@trigger cron`) via PocketBase's
  cron service.
- **M6 — the dead-letter queue** for failed function deliveries, plus the
  `deadletter` CLI.
- **M7 — the dry-run harness CLI**: `extract`/`decider`/`decide`/
  `projection`, running candidate code against real history without
  appending anything.
- **M8 — schema and reload depth**: `--cqrsStrictBoot` (M8.1), relation-typed
  schema fields for JS projections (M8.2), multi-collection JS projections —
  repeated `//@schema` per file (M8.3), store-level upcasting so transforms
  apply once at the read path for every consumer (M8.4), hot reload +
  maintenance mode, the ordering barrier between them (M8.5).
- **M9 — the docs tree**: getting-started, Go/JS guides, references, and the
  domain-doc convention.
- **M10 — the platform catalog**: introspection package, CLI, and JSON
  endpoint.
- **M11 — domain packs**: export/import CLI and the extending-over-time
  guide.
- **M12/DASH.1–2 — the ops dashboard begins**: the operational API (log
  feed, streams, dead letters, admin mode), and the `pocketcqrs-dashboard`
  binary skeleton (a second `embed.FS`-based binary, vendored assets, no
  CDN dependency).

### Housekeeping

- Canonical module path set to `github.com/JamesTryand/pocketcqrs` (later
  lowercased in v0.3.0's range).
- CI: `go vet` + build + test on Ubuntu and Windows.

```sh
go install github.com/jamestryand/pocketcqrs@v0.1.0
go install github.com/jamestryand/pocketcqrs/pocketcqrs-dashboard@v0.1.0
```
