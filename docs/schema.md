# EventModeling import and export

`pocketcqrs schema` reads and writes documents in the format defined by
[eventmodelschema](https://github.com/jamestryand/eventmodelschema) — the
JSON Schema behind [EventModeling](https://eventmodeling.org) design tooling.

Both directions are **one-shot**. There is no live connection between a
document and a running platform, and importing does not activate anything.

New to this? [The tutorial](tutorial.md) walks `import` end to end against a
real document, including a real refusal and a real name collision, before
this page's reference detail.

```sh
pocketcqrs schema import order-fulfillment.json --out pb_functions_new
pocketcqrs schema import ./order-fulfillment-split --out pb_functions_new
pocketcqrs schema export platform.json
```

## Import

A document is mapped onto the same intermediate model the dashboard's
scaffolder uses, and generated through the same code path — `Generate()` is
the only place in this project that emits JavaScript, so a document and a
wizard form cannot produce differently-shaped deciders.

| document | becomes |
| --- | --- |
| `stateChange` slice | a command on its aggregate's decider |
| `stateView` slice / `readModels` | a JS projection per read model |
| `automation` slice | a JS reactor plus the command it dispatches |
| `Command.reason`, `ReadModel.question`, descriptions, hotspots | `docs/domains/<aggregate>.md` |

**Nothing is written live.** `--out` receives the generated files; save them
through the editor (or copy them into the functions directory) and reload.
Schema-bearing files still need maintenance mode. Imported code gets no
shortcut past the load check, the dry run or the barrier — a document is a
description, and describing something must not be a way around the gates.

Both layouts are accepted: a single document, or a directory containing a
`manifest.json` with its registry and slice files. Export always writes a
single document.

### The report is the point

Every decision taken on the document's behalf is named:

```
Decisions taken on the document's behalf (10):
  ! event order-placed.items is a list of custom and becomes a json column
  ! event "order-shipped" declares no fields; generated as carrying no payload
  ! command "notify-shipping-partner" has no `aggregate` tag; using the supplied override "shipmentNotice"
Not carried across (6):
  – slice status is board state and is not preserved (done×2, inProgress×1, planned×1)
```

Read it. A folded type, a defaulted key or a dropped protection that nobody is
told about is the failure mode this whole layer exists to avoid. (A `pii` field
is never dropped: the Go target encrypts it, and the JS target refuses the
aggregate. See [Personal data](personal-data.md).)

### What it can't apply is refused

The schema says a generator must refuse what it can't apply, never drop it
(3.8.0 for access rules, 3.10.0 for refusals). So import checks every key in
the document against what the importer reads, and refuses the whole document,
naming each one, if anything is left over:

```
emschema: refusals.json declares 2 thing(s) pocketcqrs cannot apply yet, so nothing was generated (dropping them would generate code without them):
  /commands/claim-entry-for-invoice/refuseWhen: refuseWhen, rules that refuse a command (schema 3.10.0; numeric operators 3.11.0)
  /commands/update-entry/refuseWhen: refuseWhen, rules that refuse a command (schema 3.10.0; numeric operators 3.11.0)
```

The check is an allow-list, so a key added upstream after the vendored schema
is refused too, and so is a derivation or filter `kind` the importer doesn't
know. Example data (`data`, `queryParams`, a scenario's expected `result`) is
the domain's own and isn't checked. A split document is checked file by file,
and the pointer starts with the file's path.

Refused today, from schema 3.2.0 on: `selfAccess.via`, `dataSubjects`,
`outcomes`, ingresses, delays, schedules and `elapsed`, automation `effect`,
partitioning, `rowsPerListItem`, the `sumOfItems` and
`byEvent` derivations, and `projection`. Each leaves this list as it is
implemented.

### Who may read a read model

A read model's access declarations become its PocketBase `ListRule`/`ViewRule`
on the JS target (a `//@rule` in the generated projection), so the record API
enforces them on every list, view and realtime subscription:

| declared | the rule admits |
| --- | --- |
| `requiredRole` | a caller whose role is one of them: every row |
| `selfAccess.subjectField` (3.2.0) | any caller: the rows whose subject field is their own subject id |
| a scope with `grantsAccess` (3.8.0) | any caller: the rows a row of the scope's read model selects for their subject id |
| none of these | every signed-in caller (3.8.0), as `//@rule <collection> authenticated` |

The caller's role and subject id are fields of their auth record:
`schema import --roleField` (default `role`) and `--subjectField` (default the
record's own id), matching the server's `--cqrsRoleField`/`--cqrsSubjectField`,
which also stop a user setting either on themselves. An empty subject id
matches nothing. `selfAccess.param` only ever narrows, so on this target it is
the client's filter (`staffId = @request.auth.staffId`), named in a comment.
`selfAccess.via` is refused. On the Go target the same rule is applied by the
generated read route (`queryroute.Spec`'s `RequiredRole`, `SelfSubject`,
`SelfParam` and `Grants`, with the host's `ResolveOwnRole` and `SubjectOf`
hooks; registering without the hooks a rule needs fails), and a read model with
any access rule has PocketBase's own record API locked to superusers so the
route is the only way to read it.

### Refusals

A command's `refuseWhen` rules (3.10.0, numeric operators 3.11.0) are
generated into its decider, in order, after the existence checks: the first
rule whose conditions all hold refuses with its message, which an `error`
scenario names. A condition reads a field of the aggregate's own state or of
the command, and compares it with a literal or another field. Unset (missing or
null) is its own value: `is: set`/`notSet` test it, `equals` treats two unset
values as equal, and a numeric comparison with an unset value never holds. Both
targets share these semantics; the Go decider keeps the folded event values in
`State.Known` for them, since a typed zero can't say "unset". A rule naming a
field that isn't there, comparing personal data, comparing a non-number
numerically or asking a non-list whether it `contains` something is refused at
import.

### Ownership from the aggregate's own state

`requiredOwnership.stateField` (3.15.0) is decided by the aggregate: after the
existence checks, the generated decider compares that field of its own state
with the command's actor and refuses anyone else as **forbidden** (the gateway
answers `403`, not `400`), with the declared `message` or
`only this <aggregate>'s own <field> may <Command>`. No read model is read, so
the first command after a create is never refused while a projection catches
up. A JS decider throws an `Error` named `Forbidden`; a Go decider returns
`decider.Forbidden`. `requiredOwnership` is exactly one of `via` and
`stateField`, and `bypassRoles` goes with `via` only.

### Rows keyed by a field of the event

A read model's `rowKeyField` (3.14.0) keys each row by that field of the
event, in place of the event's stream, so one stream's events can update many
rows (a pay run recording a payment per person). Every event the read model is
built from or removed by must carry it, or the import is refused; at run time
an event without it stops the projection rather than write a row keyed by
nothing.

### Removals

A read model's `removedByEventIds` (3.9.0) become a removal at the top of its
generated projection (JS `{delete: key}`, Go `remove`): the event deletes the
row its stream targets, and does nothing else. A later event for that stream
creates the row again. Each must also be in `builtFromEventIds`, or the import
is refused: a removal the projection never receives would leave the row, and
any access it grants, behind.

### Untagged aggregates are refused, not guessed

`aggregate` is optional in the schema, and real documents leave boundary
elements untagged on purpose. But this project's write side is organised by
aggregate throughout, and an automation's `resultEventIds` are real log
entries — so something must own that stream.

Import therefore **refuses** and names the element:

```sh
pocketcqrs schema import doc.json \
  --aggregate notify-shipping-partner=shipmentNotice \
  --out pb_functions_new
```

Deriving one from the `swimlaneId` was rejected as a design: it is always
available and so always produces *an* answer, but swimlanes are
organisational, so two unrelated aggregates sharing a team lane would be
silently merged into one stream family — invisible until the log is wrong.

### Scenarios are run, not just read

A document's scenarios are given/when/then — which is exactly the shape of a
dry run — so an import executes them against the code it just generated,
before anything is written:

```
Scenarios checked against the generated code: 3 passed, 1 failed, 0 skipped
  ✓ [stateChange] Placing an order records Order Placed
      would append OrderPlaced — but the example data differs: …
  ✗ [stateView] Order summary reflects a placed order before shipment
      result: status missing
```

Nothing is appended and no decider is registered: each scenario runs in its
own scratch store against source held in memory. `stateChange` and `error`
scenarios are `mode=decide` runs — the `error` kind works only because a
refusal is a *verdict* rather than an error, so "the decider correctly
refused" is distinguishable from "the candidate is broken". An `error`
scenario passes only on **its own** refusal: the decider's message must be
the scenario's `then.error.message`, so a rule the model declares and the code
lacks fails even when some other rule refuses first. A command scenario's
`when.actor` and `when.at` (schema 3.10.0) are passed to the decider as
`command.actor` and `command.now`. `stateView` scenarios fold the fixture
through the generated projection and query the resulting rows. A `stateView`
scenario naming a `when.caller` (3.8.0) reads as that caller: the rows are
loaded, with the collections a granting scope joins, into a scratch PocketBase
app carrying the exact `//@rule` the import generated, and only the rows
PocketBase's own rule engine lets that caller see are kept. So the check runs
the rule that will be enforced, not a description of it. A granting scope's
param names the caller whatever it was given, and `selfAccess.param` narrows
even a role holder. Without a caller, the reader holds `requiredRole`.

**A failing scenario is not a broken import.** The generated slice records
what happened and refuses the obvious contradictions; every other rule is the
author's. A failure usually means the document describes behaviour nobody has
written yet — like a read model field that no event carries — which is the
most useful thing an import can tell you. Pass `--skip-scenarios` to turn the
check off.

Event **types** are the assertion; example `data` is reported but does not
fail a scenario, because the source schema is explicit that scenario data is
not cross-checked against declared fields.

### Field types

Ten schema types fold onto five `//@schema` types:

| schema | `//@schema` |
| --- | --- |
| `string`, `uuid` | `text` |
| `boolean` | `bool` |
| `integer`, `long`, `decimal`, `double` | `number` |
| `date`, `dateTime` | `date` |
| `custom` | `json` |
| anything with `cardinality: "list"` | `json` |
| anything with `subfields` | `json` |

`idAttribute: true` names a read model's row key. `optional` has no home in
`//@schema` and is reported as lossy. `pii` (with its `piiSubject`) is carried
by the Go target and refused by the JS target; see [Personal data](personal-data.md).
`match` filters (schema 3.1.0) are generated as search indexes for the Go
target, described on the same page.

## Export

`schema export` reconstructs a document from the running platform's catalog.
It is a **reconstruction, not a recovery**: a catalog knows what exists, not
how it was described.

Some of the source schema's required properties have no runtime counterpart
at all, so export **synthesizes** them rather than omitting them — omitting a
required property produces an *invalid* document, which is worse than a lossy
one. One `system` swimlane is invented (every event and every slice needs
one), a screen per `stateChange`/`stateView` slice, and `status:
"informational"` throughout. Automation slices deliberately get **no**
screen: the source schema is `allOf` + `if`/`then` under a single
`unevaluatedProperties: false`, so a property is legal only on the pattern
whose branch declares it, and a `screenId` there would be rejected outright.

### What round-trips, and what does not

Round-tripping is a **loss measurement**, not a fidelity promise.

| | round-trips |
| --- | --- |
| event names | ✅ declared in `//@handles` and present in the log |
| command names | ✅ the `//@commands` declared surface |
| which events a command produces | ✅ **with `//@produces`**, otherwise widened to the whole aggregate |
| read models and their source events | ✅ |
| automation wiring | ✅ via `//@dispatches` |
| field names and types | ⚠️ lossy both ways (ten types onto five) |
| element ids and display names | ⚠️ mechanically derived |
| swimlanes, screens, status | ❌ synthesized on the way out |
| `reason`, `question`, chapters, actor lanes, hotspots | ❌ one-way into the domain doc |
| scenarios | ❌ one-way into dry-run assertions |

**`//@produces` is what makes a faithful export possible.** `//@commands`
names an aggregate's commands and `//@handles` names its events; neither
joins a pair. A decider declaring `//@produces <Command> <Event...>` exports
one slice per command listing exactly that command's events. A decider
without it gets its aggregate's *whole* event set on every slice, and the
report says how many slices were widened and why. The scaffolder and the
importer emit the directive automatically, so anything generated round-trips
faithfully; only hand-written deciders predating the directive widen.

## Names

Three identifier spaces have to be reconciled:

| | example |
| --- | --- |
| schema id (`^[a-z0-9]+(-[a-z0-9]+)*$`) | `order-placed` |
| schema name (prose) | `Order Placed` |
| pocketcqrs type | `OrderPlaced` |

Import folds two into one; export derives both back. The derivation keeps
acronym runs intact — `OrderPDFGenerated` ↔ `order-pdf-generated`, not
`order-p-d-f-generated`. Aggregate names are normalised to this project's
lower-camel convention (`Order` → `order`).

## Schema version

This project reads **schema 3.17.0** documents, and an export declares it.
Import reads everything up to 3.1.1 and refuses the rest by name (above);
every version since 3.1.1 is additive, so an export, which uses only what
import reads, is a valid 3.17.0 document.

**Import reports a document's declared version but does not branch on it**,
which is deliberate rather than left over: the 2.0.0 bump fixed the signal
going forward, but a document authored between the v2 schema changes and the
bump declares `1.x` while being a v2 document, and a genuine v1 document
declares the same. Only the shape distinguishes them.

So import checks the *shape*: a slice using the removed `translation` pattern
is refused with an explanation of where it went — it collapses into
`automation`, because a read model never originates an event — and a version
that differs from the one this project reads is noted without being treated as a problem.
