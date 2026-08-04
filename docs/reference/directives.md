# Directive reference

Directives are `//@`-prefixed declarations in the **leading comment lines**
of a `.js` file in `pb_functions/`. The first non-comment line ends the
directive block. Unknown directives are ignored; files without any directive
are ignored (logged).

A file is **single-purpose** when it is a projection or a decider: those
roles may not combine with each other or with event/http/cron triggers.
Event, http and cron triggers may combine freely in one file.

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
