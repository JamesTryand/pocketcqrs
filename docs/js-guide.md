# JS guide

User-defined JavaScript functions live in `pb_functions/` (configurable with
`--functionsDir`). Every `.js` file declares its role with `//@` directives in
its **leading comment lines** — directives after the first non-comment line
are ignored. Files without directives are ignored (logged).

Functions are **trusted, owner-authored code** running in-process (goja).
There is no multi-tenant isolation; see the trust-model note in
[contributing](contributing.md).

## The four tiers

| tier | role | triggers | determinism rules | bindings |
| --- | --- | --- | --- | --- |
| 1 — effect | integrate, serve HTTP, run on a schedule | `event`, `http`, `cron` | none required (best-effort) | `console`, read-only `pb` |
| 2 — projection | fold events into collections | `projection` | replay-deterministic: `Math.random` is seeded per event; use `event.created` for time | `console`, read-only `pb` |
| 3 — decider | the write side of an aggregate | `decider` | strict: no `Math.random` (throws), no `Date`, **no `pb`** | `console` only |
| 4 — reactor | map events to **commands** (sagas, bridges) | `reactor` | `Math.random` seeded per event, because an at-least-once replay must decide the same thing twice | `console`, read-only `pb` |

There is deliberately **no write binding** anywhere: state changes must go
through the command API so they become events (the write-guard enforces it).
A reactor is the closest thing to an exception and proves the rule — it
causes writes, but only by returning a **command** that the decider registry
adjudicates, never by appending.

## The `pb` read binding (tiers 1–2)

```js
var rec  = pb.findRecord("tasks", "r1");              // one record by id, or null
var rows = pb.query("tasks", "completed = false", 100); // PocketBase filter; limit <= 500 (invalid values fall back to 100)
```

Records come back in PocketBase's public-API shape.

## The `event` binding (tiers 1–2)

```js
event = {
  position,     // global log position (catch-up order)
  id, aggregate, aggregateId, sequence,
  type,         // e.g. "TaskCreated"
  data,         // parsed JSON payload
  version,      // event schema version (after store-level upcasting)
  created       // commit timestamp
}
```

## Effect functions (tier 1)

### On events

```js
//@trigger event TaskCreated TaskCompleted
console.log("[audit] " + event.type + " " + event.aggregateId);
```

Delivery is **durable and at-least-once**: each function is a checkpointed
consumer of the log and replays missed events after a restart. A throwing
function is **dead-lettered** — the failure is captured with the full event
envelope and the checkpoint advances (poison never blocks the log). See the
[deadletter CLI](reference/cli.md#deadletter): fix the code, then
`pocketcqrs deadletter retry all` re-delivers through the *current* code.

### On HTTP

```js
//@trigger http
function handle(request) {
  // request: { method, path, query, headers, body, bodyText, auth }
  return { message: "hello" };
  // or: return { status: 201, body: {...}, headers: { "x-foo": "bar" } };
}
```

Served at `GET|POST /api/fn/<basename>` (`hello.js` → `/api/fn/hello`).
Requires auth unless `--cqrsAllowAnonymous`; `request.auth` carries the
caller (`{id, collection}`) or is null.

### On a schedule

```js
//@trigger cron */5 * * * *
console.log("tick " + job.firedAt); // job: { name, firedAt }
```

Standard 5-field cron expressions, registered on PocketBase's cron service.
Cron failures are logged only (there is no event delivery to dead-letter).

## Projections (tier 2)

```js
//@trigger projection notes on NoteCreated NoteTextChanged NoteArchived
//@schema notes noteId:text text:text archived:bool
//@key noteId
function project(event) {
  if (event.type === "NoteCreated") {
    return { upsert: { key: event.aggregateId, fields: { text: event.data.text, archived: false } } };
  }
  // ...
}
```

- `//@schema <collection> <field>:<type> ...` declares the output collection.
  It is materialized at boot **additively** (create missing collections/fields,
  never drop or retype) and publicly readable; writes are write-guarded.
  Field types: `text | number | bool | date | json | relation(<collection>)`.
  Relations wire by target collection name (single, `maxSelect: 1`).
- `//@key <field>` is the idempotency key: upserts find-or-create by it, and
  it gets a unique index. Required.
- **Multiple collections**: repeat `//@schema` (+ its `//@key`) per file.
  Each `//@key` pairs with the most recent `//@schema`. Ops then need a
  `collection` attribute (see below).
- `project(event)` returns row ops:
  - `{ upsert: { key: <keyval>, fields: {...} } }` — merge into the keyed row
  - `{ delete: <keyval> }` — remove the keyed row (no-op if absent)
  - an array of those — or nothing (`undefined`/`null`) for "no-op"
  - optional `"collection": "<name>"` on any op — **required** when the file
    declares more than one schema (ambiguous/undeclared ops block the
    projection at the failing event)
  - `id`, `created`, `updated` and the key field itself may not be set in
    `fields`

**Determinism**: replays must produce the same rows. `Math.random` is
replaced by a PRNG seeded from the event position; use `event.created` for
time. Prefer **recompute-over-increment** (`orders_by_customer.js` recomputes
from the read side instead of `n+1`) — it keeps replays and dry-run diffs
clean. A failing projection **blocks** at the failing event (correctness over
progress); fix the code and [rebuild](reference/cli.md#projection) or let it
catch up.

## Deciders (tier 3)

```js
//@trigger decider note
//@handles NoteCreated NoteArchived
//@transform NoteCreated 1 2

function initialState() { return { exists: false, text: "", archived: false }; }

function decide(command, state) {
  // command: { name, payload, now, actor }
  switch (command.name) {
    case "CreateNote":
      if (state.exists) throw new Error("note already exists");
      return [{ type: "NoteCreated", data: { text: command.payload.text } }];
    // throw = domain rejection (400)
  }
}

function evolve(state, event) {
  // event: { type, data, version } — already upcast to the latest version
  if (event.type === "NoteCreated") { state.exists = true; state.text = event.data.text; }
  return state;
}

function transform_NoteCreated_1_to_2(data) { data.priority = data.priority || 0; return data; }
```

- Registered into the same registry as Go deciders; a name collision with a
  built-in aggregate is refused.
- `//@handles` must cover every event type in existing streams (checked at
  boot and on reload).
- `decide` returns `[{ type, data, version? }]` (default version 1).
  `command.now` is stamped by the registry and recorded in the produced
  events' metadata — the time the decider saw is part of history.
- **Validation**: at boot (and on hot reload) every existing stream of the
  aggregate is folded through `evolve`; a failing or under-declared decider
  is refused (kept out, system keeps serving — or aborts boot with
  `--cqrsStrictBoot`).
- **Versioning contract**: events evolve append-only — properties may be
  added, never removed; a `//@transform <Type> <from> <to>` upcaster converts
  the previous version; a significantly different shape is a **new event
  type**. Transforms are applied at the store's read path, so *every*
  consumer (deciders, projections, effects) sees the latest version.

## Reactors (tier 4)

```js
//@trigger reactor TaskCompleted
//@dispatches note/CreateNote

function reactTo(event) {
  return [{
    aggregate: 'note',
    id: 'completed-' + event.aggregateId,   // deterministic: see below
    command: 'CreateNote',
    payload: { text: 'task ' + event.aggregateId + ' was completed' }
  }];
}
```

A reactor is a durable consumer that maps a committed event to **commands**,
never to a direct append — the same shape the Go `reactors` package uses
(both tiers share one `reactors.Dispatch`). It is the closest thing to a
write binding this project has, and it proves the rule rather than breaking
it: a reactor causes a write only by asking the decider registry, which can
still refuse.

- `reactTo(event)` **returns** dispatch descriptors instead of calling a
  `dispatch()` host binding — mirrors `project()` returning row ops and
  `decide()` returning events, keeps the function pure, and lets
  `mode=reactor` report what *would* be sent without stubbing anything out.
  Anything returned that isn't a recognisable descriptor is counted and
  discarded (a bare object sends nothing, silently, unless you look at the
  count); a descriptor missing `aggregate`, `id` or `command` fails loudly.
- **Delivery is at-least-once, so reactions must be idempotent.** The
  standard pattern, shown above: derive the target id from the source event.
  A replay dispatches the same command to the same id, hits a domain
  rejection ("note already exists"), and that rejection is logged and
  skipped rather than the reaction firing twice. `Math.random` is seeded
  from the event position for the same reason — a retry has to make the
  same decision the attempt it's retrying made.
- **Failure**: a reactor that throws is dead-lettered (like an effect
  function), not blocking. A concurrency conflict on the target stream
  propagates so the whole event retries; a domain rejection is logged and
  the consumer advances — a note that already exists is not a bug.
- **No schema, no barrier**: reactors declare nothing PocketBase-visible, so
  they activate in **running** mode like effect functions — no maintenance
  window for an ordinary saga edit.
- **Checkpoints**: `fn-reactor:<basename>`, deliberately distinct from Go
  reactors' `reactor:<name>` — a shared prefix would mean a shared
  checkpoint. The metadata `actor` stamped on the dispatched command *does*
  use `reactor:<name>`, because that's what the catalog's flow detection
  joins on.
- `//@dispatches <aggregate>/<Command>...` is optional and documentation-only
  (same limits as `//@commands`), but worth adding: a dispatched command
  leaves no trace of its own in the catalog until the reaction has actually
  fired once, so a saga that has never run is otherwise invisible.
- Dry-run before shipping: `pocketcqrs dryrun` has no CLI subcommand for
  reactors yet, but `mode=reactor` over
  [`POST /api/cqrs/admin/dryrun`](reference/gateway.md#dry-run) — and the
  dashboard's Functions page — replays matching history and reports what it
  would dispatch, with no registry installed so nothing can go out even by
  accident.

## Changing code

### Workflow

1. Change the file.
2. Dry-run it against real history (nothing is persisted):
   `pocketcqrs dryrun decider|projection <file.js> [--diff]`
3. Hot-reload: effects anytime, schema-bearing files in maintenance —
   see [getting started](getting-started.md#change-code-without-restarting).
4. Failed effect deliveries? `pocketcqrs deadletter list` → fix → `retry all`.

The same workflow runs in a browser on the ops dashboard's **Functions**
page, over the
[function-file and dry-run API](reference/gateway.md#function-files): edit,
dry run against real history, save, then reload from the System page. Saving
is never activation — and a save is refused outright if the source does not
parse or compile, because reloads are all-or-nothing and an unloadable file
would block every later reload, including the one that fixes it.

### What hot reload swaps when

| file kind | when | semantics |
| --- | --- | --- |
| event / http / cron | any mode | swapped immediately, checkpoints carry |
| reactor | any mode | swapped immediately, checkpoint carries (`fn-reactor:` key, distinct from Go reactors') |
| projection (+schema) | maintenance only | schema reconciled additively, consumer swapped, checkpoint carries |
| decider | maintenance only | re-validated, then swapped; refusal keeps old code |

A file that loses all its directives is treated as a non-function and
dropped on the next reload (same as deleting it).

## Limits (by design)

- Effect functions are at-least-once; make them idempotent.
- No `Date`/`Math.random` in deciders — time and randomness are inputs
  (`command.now`, event metadata), not ambient state.
- No snapshotting; long streams fold from position 0.
- Historical commands are not persisted — validation covers the
  "prior events" half of the harness (see `dryrun extract`).
