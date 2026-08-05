# Directive reference

Directives are `//@`-prefixed declarations in the **leading comment lines**
of a `.js` file in `pb_functions/`. The first non-comment line ends the
directive block. Unknown directives are ignored; files without any directive
are ignored (logged).

A file is **single-purpose** when it is a projection, a decider or a
reactor: those roles may not combine with each other or with event/http/cron
triggers. Event, http and cron triggers may combine freely in one file.

`react` is deliberately not part of that combinable group. A file declaring
both `//@trigger event X` and `//@trigger react X` would be two delivery
paths over one event, with two checkpoints and no sensible reading — so it
is refused by name rather than one of them being silently dropped.

---

## `//@trigger event <EventType...>`

Registers an **effect function** (tier 1) delivered once per committed event
of the given types (durable, at-least-once, dead-lettered on failure).
The script body runs per event with `event`, `console`, `pb` bindings.

```
//@trigger event TaskCreated TaskCompleted
```

## `//@trigger http`

Registers an **HTTP function** at `GET|POST /api/fn/<basename>`. The file
must define `handle(request)`.

## `//@trigger cron <5-field schedule>`

Registers a **cron function**; the script body runs per tick with a `job`
binding (`{name, firedAt}`). One cron trigger per file; the schedule keeps
its spaces:

```
//@trigger cron */15 * * * *
```

## `//@trigger projection <name> on <EventType...>`

Registers a **JS projection** (tier 2) named `<name>` (the durable
checkpoint key), delivered events of the given types. The file must define
`project(event)` and declare at least one `//@schema`.

```
//@trigger projection orders_by_customer on OrderPlaced OrderConfirmed
```

## `//@schema <collection> <field>:<type> ...`

Declares a projection output collection. Repeatable for multi-collection
projections (see `//@key` for pairing). Names must match
`^[a-zA-Z][a-zA-Z0-9_]*$` (they are interpolated into index DDL).

Field types:

| type | PocketBase field |
| --- | --- |
| `text` | text (the `//@key` field is made required when text) |
| `number` | number |
| `bool` | bool |
| `date` | date |
| `json` | json |
| `relation(<collection>)` | single relation (`maxSelect: 1`) to the named collection |

Schemas are reconciled **at boot and on maintenance reload, additively
only**: missing collections/fields/indexes are created; existing fields are
never removed, retyped or renamed (a declared/actual type mismatch is logged
and kept). Relation targets may be other declared collections (any order) or
pre-existing ones (migration-created, auth collections). A relation whose
target collection does not exist fails the reconcile.

## `//@key <field>`

The projection's idempotency key: upserts find-or-create by it, and it gets
a unique index. Must be a declared field of its schema. **Exactly one per
`//@schema`, pairing with the most recent one.** A `//@key` before any
`//@schema`, a second `//@key` for the same schema, or a schema without a
key is a load error.

```
//@schema customers cust:text total:number
//@key cust
//@schema products sku:text sold:number
//@key sku
```

## `//@trigger decider <aggregate>`

Registers a **JS decider** (tier 3) for `<aggregate>` into the decider
registry. Collision with a built-in (Go) aggregate is refused. The file must
define `initialState()`, `decide(command, state)`, `evolve(state, event)`
and declare `//@handles`.

## `//@handles <EventType...>`

Required for deciders: the event types the decider may produce or must fold.
Boot/reload validation fails if an existing stream contains a type not
declared here.

## `//@commands <Name...>`

Optional, deciders only: the commands this aggregate accepts.

It is **documentation, not enforcement** — `decide()` still adjudicates, and
an unlisted command is not rejected by the directive. It exists because
commands are the one part of a slice that cannot be recovered from the log:
events leave a trace there and can be reported empirically, commands leave
none. Without a declaration the catalog cannot list them, a schema export
cannot reproduce them, and nothing can validate a payload later.

```js
//@trigger decider note
//@handles NoteCreated NoteTextChanged NoteArchived
//@commands CreateNote ChangeNoteText ArchiveNote
```

Go deciders declare the same thing with the `Commands` field:

```go
return &decider.Decider[TaskState]{
    Commands:     []string{CmdCreateTask, CmdCompleteTask},
    InitialState: func() TaskState { return TaskState{} },
    // …
}
```

Declared commands appear in `GET /api/cqrs/catalog`, in the `catalog`
Markdown, and fill the command table of a generated domain-doc skeleton
(which otherwise stays a `TODO` row — an honest blank rather than a guess
from event names).

## `//@trigger react <EventType...>`

Registers a **JS reactor** (tier 4): a durable consumer that maps committed
events to **commands**. The file must define `react(event)`, which returns
dispatch descriptors.

```js
//@trigger react OrderConfirmed
//@dispatches task/CreateTask

function react(event) {
  return [{
    aggregate: 'task',
    id: 'fulfill-' + event.data.orderId,   // deterministic: see below
    command: 'CreateTask',
    payload: { title: 'fulfil order ' + event.data.orderId }
  }];
}
```

**A reaction enters as a COMMAND, through the decider registry** — never as
a direct append. That is the whole point of the tier: an appended event
would be an unconditional pass-through, while a command can be refused, so
the resulting event stays a *decision*. This is the same rule the Go
`reactors` package follows, and both tiers share one implementation
(`reactors.Dispatch`) so they cannot drift.

`react(event)` **returns** descriptors rather than calling a `dispatch()`
host binding, matching `project()` returning row ops and `decide()`
returning events. The function stays pure, so `mode=react` can report what
it *would* send without stubbing anything out.

Anything returned that is not a descriptor is **counted and logged**, then
discarded — a reactor returning plain objects sends nothing, and the count
is what says so out loud. A descriptor that is recognisably meant to
dispatch but is missing its `aggregate`, `id` or `command` fails loudly
instead.

**Delivery is at-least-once, so reactions must be idempotent.** The standard
pattern is the one above: derive the target id from the source event, so a
replay hits a domain rejection ("already exists"), which is logged and
skipped rather than dispatching twice. `Math.random` is seeded from the
event position for the same reason — a retry must decide what the attempt it
is retrying decided.

Failure semantics match the effect tier: a reactor that throws is
**dead-lettered**, not blocking. A concurrency conflict on the target stream
propagates so the whole event is retried; a domain rejection is logged and
the consumer advances.

Reactors declare no schema, so they **activate in `running` mode** — no
maintenance barrier for an ordinary saga edit.

The durable checkpoint key is `fn-react:<basename>`, deliberately *not*
`reactor:<name>`, which Go reactors use — a shared prefix would mean a
shared checkpoint. The metadata `actor` stamped on dispatched commands
*does* use `reactor:<name>`, because that is what the catalog's flow
detection joins on. Two decisions that look like one, made in opposite
directions on purpose.

## `//@dispatches <aggregate>/<Command>...`

Optional, reactors only: the commands this reactor sends.

Same reasoning as `//@commands`, and the same limits — documentation, not
enforcement. A dispatched command leaves no trace of its own; the *event* it
causes does, but only once the reactor has actually fired, so an automation
that has never run is invisible to the catalog without this. A malformed
entry is refused rather than dropped.

Declared dispatches appear on the consumer in `GET /api/cqrs/catalog`
(`kind: "js-reactor"`).

## `//@transform <Type> <from> <to>`

Declares an upcaster for event `<Type>` from version `<from>` to `<to>`
(positive integers). The file must define
`transform_<Type>_<from>_to_<to>(data)`. Chains compose (`1→2`, `2→3`).
Applied at the store read path — see the
[versioning contract](js-guide.md#deciders-tier-3).

---

## Row ops (projection return values)

`project(event)` may return: nothing (`undefined`/`null`), one op, or an
array of ops.

```js
{ upsert: { key: <keyval>, fields: { <field>: <value>, ... } } }
{ delete: <keyval> }
{ collection: "<declared collection>", ... }  // either op kind
```

- `collection` is optional with one declared schema, **required** with more
  than one; it must name a declared schema.
- `id`, `created`, `updated` and the key field may not appear in `fields`
  (ignored with a log).
- Upserts merge into the keyed row; deletes are no-ops when the row is
  absent. Ops apply in order.
