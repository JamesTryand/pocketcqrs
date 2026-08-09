# CLI reference

All commands are subcommands of the `pocketcqrs` binary (`go run . <cmd>`).
PocketBase's own commands (`serve`, `superuser`, `migrate`, ...) are
available too. Persistent PocketBase flags: `--dir` (data dir), `--http`
(listen address) for `serve`.

PocketCQRS flags (persistent, on every command):

| flag | default | meaning |
| --- | --- | --- |
| `--functionsDir` | `pb_functions` | directory with user-defined JS functions |
| `--tutorial` | `false` | register this repo's example domains (`task`, `order`), their projections, the fulfillment saga and their collections |
| `--cqrsAllowAnonymous` | `false` | dev: commands and `/api/fn` need no auth token (no actor metadata stamped) |
| `--cqrsStrictBoot` | `false` | abort startup when a JS decider fails validation (default: skip it and keep serving) |
| `--cqrsAllowOutboundHTTP` | `false` | expose `$http` to **event, cron and reactor** functions. Not to `//@trigger http` functions (request-driven, so unbounded concurrency), and never to deciders or projections |
| `--cqrsOutboundHost` | *(none)* | a hostname `$http` may call. Repeatable; deployment-wide, not per-function. **No entries permits nothing** |
| `--cqrsAllowPrivateOutbound` | `false` | dev/internal: let `$http` reach loopback and private ranges. Link-local (`169.254.0.0/16`, the metadata endpoint) stays blocked regardless |

Outbound HTTP is off unless asked for, and asking for it takes two flags —
enabling it with no `--cqrsOutboundHost` refuses every call and warns at boot.

```
pocketcqrs serve \
  --cqrsAllowOutboundHTTP \
  --cqrsOutboundHost api.stripe.com \
  --cqrsOutboundHost hooks.slack.com
```

See [the JS guide](../js-guide.md#calling-out-http-event-cron-and-reactor-functions) for the
binding and the bounds it enforces.

## serve

```sh
pocketcqrs serve [--http 127.0.0.1:8090] [--dir pb_data] [--tutorial]
```

Boots the platform and serves HTTP: PocketBase API + admin UI (`/_`), the
[gateway routes](gateway.md), the consumers engine, cron jobs. Bootstrap
applies app migrations, validates JS deciders, reconciles JS projection
schemas and starts catch-up from the event log.

**Without `--tutorial` the platform registers nothing of its own**: no
aggregates, no Go projections, no collections beyond PocketBase's own and
whatever your JS projections declare. The example migrations are not
registered either, so their collections are never created — and because an
unregistered migration is also never *recorded*, turning the flag back on
later still creates them. The switch works in both directions.

Two consequences worth knowing:

- The names `task` and `order` are free for your own JS deciders. With
  `--tutorial` they are taken, and a colliding JS decider is refused.
- If a previous run *did* create the example collections, they stay behind
  when you drop the flag. They keep their write-guard rather than silently
  becoming writable, and boot logs a warning naming them.

## projection

```sh
pocketcqrs projection rebuild <name>
```

Offline (stop `serve` first). Wipes the projection's collections, resets its
durable checkpoint and replays the whole event log through the current code.
Works for JS projections and, under `--tutorial`, the example Go projections
(`tasks`, `orders`) alike.

## deadletter

Failed **effect-function** deliveries are captured with the full event
envelope, error, attempt count and timestamps (the checkpoint still
advances — poison never blocks the log).

```sh
pocketcqrs deadletter list [--all]   # pending (+ resolved with --all)
pocketcqrs deadletter retry <id|all> # re-deliver through the CURRENT code
pocketcqrs deadletter dismiss <id>   # mark resolved without retrying
```

Retry semantics: a successful retry resolves the letter; a failing one
increments the attempt count and records the error. Projections do not
dead-letter — they block at the failing event by design.

The same three actions are on the HTTP API
([`POST /api/cqrs/deadletters/{id}/retry|dismiss`](gateway.md#operations)) and
on the ops dashboard's Consumers page. All of them share one implementation,
so they adjudicate a retry identically — the CLI is for an operator at a
shell, the API for one at a browser.

## dryrun

Runs candidate JS code against real history without appending events or
touching collections. The general workflow: extract fixtures, change code,
dry-run, deploy.

```sh
pocketcqrs dryrun extract <aggregate> <id> [--file out.json]
pocketcqrs dryrun decider <file.js> [aggregate-id]
pocketcqrs dryrun decide <file.js> <aggregate-id> <command> [payload-json]
pocketcqrs dryrun projection <file.js> [--diff]
```

- **extract** — dump a stream as a JSON fixture (the harness's "prior
  events").
- **decider** — fold existing streams of the decider's aggregate through the
  candidate code; prints stream/event counts, and the final state for a
  single id. Evolve errors and undeclared `//@handles` coverage fail the run.
- **decide** — fold one stream, then show the events a command WOULD
  produce (nothing is appended). **A domain rejection is a verdict, not a
  command failure**: the CLI exits `0` and prints "The decider REFUSED this
  command... This is a domain answer, not a failure." — the same
  `{ok:false, rejected:true}` shape the HTTP dry-run returns as a `200`.
- **projection** — simulate over the whole log in memory (upsert/delete
  counts, final rows per collection). `--diff` compares the simulation
  against each live collection and prints per-collection differences.
  Note: projections that read the live read side via `pb.query`
  (read-modify-write style) are not `--diff`-clean — the simulation reads
  the already-projected state. Recompute-style projections diff cleanly.

The same checks run over HTTP at
[`POST /api/cqrs/admin/dryrun`](gateway.md#dry-run) (source in the body
rather than a path, so the ops dashboard's editor can dry-run code that has
never been written to disk), and from the dashboard's Functions page. One
difference worth knowing: the HTTP `decider` mode also applies the gate a
reload applies — the contract probe and `//@handles` coverage — so it answers
"would a reload accept this?", which folding alone cannot for an aggregate
with no history yet.

## system

```sh
pocketcqrs system maintenance status   # print the current mode
pocketcqrs system maintenance on       # reject domain commands (503)
pocketcqrs system maintenance off      # resume serving
```

The mode is persisted in `events.db` and read per request, so these commands
work from a separate process while `serve` is running and take effect
immediately. Maintenance is the barrier for reloading schema-bearing
function files — see the [reload endpoint](gateway.md#admin).

## catalog

```sh
pocketcqrs catalog                     # Markdown + Mermaid to stdout
pocketcqrs catalog --json              # the raw catalog document
pocketcqrs catalog --skeletons docs/domains [--force]
```

Introspects the platform: registered aggregates (Go and JS, with declared
handles/transforms), empirical event types and version ranges from the log,
consumers (kind, triggers, owned collections, checkpoints), guarded
collections, HTTP/cron functions, and reactor flows derived from causation
metadata. The Markdown rendering includes a Mermaid flowchart
(aggregates → consumers → collections, plus empirical reactor edges).

`--skeletons` writes one domain-doc skeleton per aggregate (the
[convention template](../domains/README.md)) — events, flows and
implementation prefilled, commands left as TODO rows. Existing files are
skipped unless `--force`.

## pack

```sh
pocketcqrs pack export <outdir> --name <name> [--version v] [--description d]
    [--functions a.js,b.js] [--collections c1,c2]
pocketcqrs pack import <packdir> [--force]
```

Export bundles function files (default: all `.js` in `--functionsDir`) plus
optionally plain (non-projection-owned) collection schemas into a domain
pack directory (`manifest.json`, `pb_functions/`, `collections.json`).
Projection-owned collections are refused on export — they're recreated from
`//@schema` on import. Both directions load-validate the function files
first.

Import copies the function files into `--functionsDir` (skipping existing
unless `--force`) and applies `collections.json` via PocketBase's native
collection import. Activate with a restart or a maintenance-mode reload.
See [domain packs](../packs.md).

## superuser (PocketBase)

```sh
pocketcqrs superuser upsert <email> <password>
```

Create/update a superuser (admin UI + reload endpoint auth).

## schema

```sh
pocketcqrs schema import <document.json|manifest-dir> [--out dir] [--docs dir]
    [--aggregate <elementId>=<name>]... [--force]
pocketcqrs schema export <document.json>
```

Import and export [EventModeling](https://eventmodeling.org) documents —
see [the guide](../schema.md) for the mapping and what survives a round trip.

`import` accepts a single document or a split manifest directory. Without
`--out` it maps and reports without writing anything, which is the quickest
way to see what a document would produce. `--docs` writes per-aggregate
domain docs carrying the methodology prose (`reason`, `question`,
descriptions, hotspots) that has no home in code. Existing files are skipped
unless `--force`, matching `catalog --skeletons`, so a re-import cannot
overwrite prose someone has edited.

`--aggregate` supplies the aggregate for an element the document leaves
untagged. Import **refuses** rather than guessing: the write side is
organised by aggregate, and deriving one from the swimlane would silently
merge unrelated stream families.

Nothing imported is live. Save the generated files through the editor or copy
them into `--functionsDir`, then reload — schema-bearing files behind the
maintenance barrier.

`export` reconstructs a document from the running catalog, synthesizing the
required-but-absent design-time pieces (one swimlane, a screen per
screen-bearing slice, `status: informational`) and reporting everything it
invented or could not carry.
