# Changelog

All notable changes to PocketCQRS. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/); versions match git tags.

## Unreleased

### Added

- `examples/pb_functions/task_completion_note.js`: a shipped JS reactor example.
  `TaskCompleted` (Go `task` aggregate) dispatches `CreateNote` (JS `note`
  aggregate) — the reactor tier previously had no runnable example in the
  shipped fixture set, only the abstract one in the directive reference.
  Wired into `getting-started.md`'s walkthrough. Documented in
  `docs/domains/task.md` and `docs/domains/note.md`.

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
