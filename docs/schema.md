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
| `automation` slice | a JS reactor plus the command it dispatches, to the stream its `targetIdField` names |
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
  /commands/hold-booking-for-event-plan/refuseWhen: refuseWhen, rules that refuse a command (schema 3.10.0; numeric operators 3.11.0)
  /commands/change-booking/refuseWhen: refuseWhen, rules that refuse a command (schema 3.10.0; numeric operators 3.11.0)
```

The check is an allow-list, so a key added upstream after the vendored schema
is refused too, and so is a derivation or filter `kind` the importer doesn't
know. Example data (`data`, `queryParams`, a scenario's expected `result`) is
the domain's own and isn't checked. A split document is checked file by file,
and the pointer starts with the file's path.

Refused today, from schema 3.2.0 on: `selfAccess.via` and partitioning. Each
leaves this list as it is implemented.

**The boundary (schema 6.0.0, v0.25.0).** A document names messages, never
their sources, so the parts that cross the edge are read, and only the model
in them is generated:

- a `stateChange` slice started by an **ingress** (`ingressId`) generates its
  command like any other; the ingress itself generates nothing (how the message
  arrives and is checked is surface tooling's);
- a `stateChange` slice with **neither a screen nor an ingress** (schema 6.1.0,
  v0.27.0) is sent by another of your systems, over a coordinating document's
  internal edge: its command must be `public` (or the lint refuses the slice),
  is generated like any other, and the report names it; the reaction that
  sends it comes from the edge, not this document;
- an automation with a **delay** generates its command but no reactor: sending
  it after the delay is its source's;
- an automation with an **effect** generates its answer and its give-up
  commands but no reactor: the call is its source's (an ext-caller rule). The
  answer and the give-up may each begin the stream (v0.26.0), and whichever
  comes first does: the other is refused as "already exists", so a call has
  exactly one outcome;
- an **output** generates nothing; import checks each field it says leaves is
  on its trigger events, and reports it.

The report names each of these.

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
the client's filter (`memberId = @request.auth.memberId`), named in a comment.
`selfAccess.via` is refused. On the Go target the same rule is applied by the
generated read route (`queryroute.Spec`'s `RequiredRole`, `SelfSubject`,
`SelfParam` and `Grants`, with the host's `ResolveOwnRole` and `SubjectOf`
hooks; registering without the hooks a rule needs fails), and a read model with
any access rule has PocketBase's own record API locked to superusers so the
route is the only way to read it.

### Who may send a command

A command's `requiredRole`, `fieldGatedRole`, `requiredOwnership.via` and
`scope` become an [`//@authorize`](reference/directives.md#authorize-command-policy-json)
line in the generated decider on the JS target, and **the stock server enforces
it at the gateway** with nothing to wire: a caller the policy refuses gets `403`
(`not authorized to issue <Command> on <aggregate>`, or a scope's own
`message`), and nothing is recorded. The policy is the one verify checks
scenarios against, so a scenario's `when.role` tests what a deployment does.

It is optional. A command that declares none of these gets no line and is
allowed to every caller the gateway admits, as before; a document with no
command permissions generates exactly what it did. A superuser passes; the
caller's role and subject id are the same auth-record fields read models use
(above). `requiredOwnership.stateField` is decided by the generated decider
itself (below). It covers commands sent through the gateway; a command an
automation's reactor sends goes through the registry in-process and is not
checked. On the Go target the host wires `authorize.Authorizer` into
its own `gateway.Config.Authorize`, as before.

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

### Outcomes

A slice's `outcomes` (3.3.0; `when` and `set` 3.12.0, `repeated` 3.13.0) become
the command's alternatives, tried in order after its refusals: the first whose
`when` holds is the result, and the last has none (the default). A `when` is
refusal conditions (any group holds), so it reads the same state and command
fields. `set` fixes field values on an alternative's events. Several
alternatives with no `when` at all say what may happen but not which, and are
refused.

`when: "hand"` and `repeated` events are **hand-written hooks**: the JS decider
names them (`//@hand`, `//@hooks`, see the directives reference) and loads them
from `<aggregate>.hand.js`; the Go decider calls functions you write in
`<aggregate>_hand.go` (missing ones are a build error). The import writes
`.example` skeletons for both. A repeated event's hook returns one data object
per item, in order (none is fine).

Verify can't run a hook you haven't written: a scenario that reaches one is
reported **HAND** (✋) when its expected events fit one of the declared
alternatives, and fails when they fit none. An `error` scenario that reaches a
hook fails.

An event tagged with an aggregate that no slice produces (something else
appends it) is folded by that aggregate's decider all the same, and the import
says so.

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
rows (a grant round closing with an award per recipient). Every event the read model is
built from or removed by must carry it, or the import is refused; at run time
an event without it stops the projection rather than write a row keyed by
nothing.

### A field set by event

A `byEvent` derivation (3.16.0) sets a field to a literal per event: a status
that reads `staged` after one event and `sent` after another. It is applied
after the event's own row update, so the event's other fields still land; an
event it doesn't name leaves the field as it was, and a removing event removes
the row as usual. A literal of the wrong type for its field is refused at
import.

### A row per group of a list

A read model's `rowsPerListItem` (3.16.0) makes a row per group of a stream's
list items in place of one row per stream: a delivery's parcels become a load
per depot, keyed `<stream>|<group>`. An event carrying the list replaces it;
the read model's other fields merge from the stream's events onto every row
(so a vehicle named only when the delivery was planned is still on the rows
its dispatch rebuilt). A group that leaves the list loses its row, an item with
no group is in no row, and a removing event removes all of the stream's rows.
A `sumOfItems` field is the sum of a subfield over the row's items, and null
until every one of them carries it. Its column is json, whatever number type
the model gives it, because a PocketBase number field can't hold null (it would
read 0); a Go host's migration declares it json too.

The host keeps each stream's merged fields and list in a companion
collection, `<collection>_streams`, readable by superusers only
(`//@rule <collection>_streams superuser`), so the projection never reads its
own rows back and a scenario verifies the same as the live run. The Go target
plans the rows with the same code (`projections/itemrows`); both its
collections are your migrations, as ever. Not with `rowKeyField`, and the
rows fold no toggle, count, sum or groupBy derivations.

### A hand-written projection

A read model with `projection: "hand"` (3.17.0) is one no declaration
expresses, such as a join across two streams. The import generates its query
route but not its projection: it writes `<collection>.js.example` (or
`<collection>.go.example`), the generated field merge with a note, for you to
copy to `<collection>.js` (`.go`) and write. Nothing loads an `.example`.
Verify reports its view scenarios as **HAND**: only a host check, which runs
your own projection, can decide them.

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
lacks fails even when some other rule refuses first. A `stateChange` scenario
whose `then.events` is empty (schema 6.2.0) is an assertion too: it passes,
as "accepted, nothing recorded", only when the command is accepted and
records no events, so a forgotten event or a refusal fails it; a `then` with
no `events` at all is refused. A command scenario's
`when.actor` and `when.at` (schema 3.10.0) are passed to the decider as
`command.actor` and `command.now`. Before the decider, the command's
authorization runs as the gateway runs it (package `cmdauth`), over read
models projected from the scenario's `given` events: an ownership `via` or a
`scope` that refuses the actor refuses the command, with the scope's
`message` (4.0.0) when it declares one. A command scenario may name the
role its actor holds (`when.role`, 4.1.0): then every declaration runs with
it, so `requiredRole` and `fieldGatedRole` must admit it (refusing with
"not authorized to issue <Command> on <aggregate>") and a declaration whose
`bypassRoles` name it is skipped. Without a role, the actor holds the
command's required roles and none of its bypass roles. `stateView` scenarios fold the fixture
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

### Verify on its own, and against a host

`pocketcqrs schema verify <document>` runs the same scenarios with nothing
written, and exits non-zero when one fails, so a build can run it. HAND is not
a failure.

With `--functions <dir>` (a host's `pb_functions`), the scenarios run against
that directory's own files in place of the generated ones, matched by name:
its deciders, its hand files (`<aggregate>.hand.js`, so a hook really decides
and its scenario passes or fails instead of being HAND), and its projections,
including a hand-written one (3.17.0), whose view scenarios then run. A file
the host doesn't have is a skip that says so, never the generated code in its
place. A projection's `pb` reads only its own collections, served in a
scenario run from the rows simulated so far, so a hand projection that keeps
the rows its join needs is checked like any other.

With `--lang go` the scenarios run against Go code rather than the JS dry run:
the Go the import generates (hand hooks as stand-ins that report HAND), or,
with `--functions`, a host's own Go packages in the import's layout
(`<dir>/<package>/`). The code is compiled with package `verifyharness` into a
scratch module and run once: deciders fold each scenario's events and decide
through the registry, nothing appended, personal data sealed and revealed by
an in-memory key service; each view scenario gets its own PocketBase test app,
so its rows are read back as PocketBase serves them. The answers are compared
by the same code as the JS check's, reading a row as PocketBase serves it: an
unset bool is false, and a date field's `2026-09-15 00:00:00.000Z` is the
model's `2026-09-15` or the same instant. So the Go check can find what the
JS dry run can't: a model's null that a PocketBase number field holds as 0,
say (`sumOfItems` before every item is converted).

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

### A system's edges, from a coordinating document (v0.28.0)

A coordinating document (eventmodelschema's `architecture.schema.json`, 0.2.0)
says how several systems work together. Name it, and this document's system
in it, and the Go target also writes the system's edges:

```sh
pocketcqrs schema import model/commerce.eventmodel.json --lang go --out domain \
  --architecture model/landing-pages.architecture.json --system commerce
```

- `edges.go`, in the wiring package: `EdgesIn` (each internal edge into this
  system: the source event at its version, the local aggregate and command,
  the field the target stream id comes from, and the field mapping) and
  `EdgesOut` (each public event an edge out of it starts from, with the
  fields its public marker lists (schema 6.2.0), or else only the fields some
  edge maps). Data only: package `edges` is what they mean
  (`edges.Publish`, `edges.In.Translate`), so there is one implementation.
- **Each edge's `examples` run** against `edges.In.Translate`, after the
  sender's filter, and a failing one refuses the import: the checker
  (`architecture-check.js`) and this generator cannot disagree.
- **Refused:** a system document that is not the one the coordinating
  document names; an edge's event or command that is not public here; a
  target id of several fields; a personal field on an edge or in an event's
  published fields (carrying one is not built); and an edge reading a field
  its event's marker does not list.
- **No carrier is named.** The host gives the tables to one: with
  pocketcqrs-extensions' `federation`, `PublisherConfig.Edges = EdgesOut` on
  the sending system and `IngestConfig.Edges = EdgesIn` on the receiving one.
  Which carrier, its subjects and its credentials are surface tooling's.
- What crosses is `edges.Message`, the architecture schema's `edgeMessage`:
  the source event's id, system, event and public version, and only the
  published fields. Not this store's own event record, so either runtime can be
  on either end.
- **A message names its sender by the sender's document id** (v0.33.0,
  coordinating document 0.3.1), never by the name a view gives it:
  `EdgesSystem` and `EdgesOut[].System` are this document's `id`, and
  `EdgesIn[].FromSystem` is the sender's, read from its document or its
  release. An edge from a placeholder is refused: it has no id yet.
- **A system named by release** generates too: the document given must be
  the one whose `id` the release names.

### A sender from its own document alone (v0.33.0)

(Since v0.33.1 the same holds with `--architecture`: every event whose marker
lists its fields is published, whether or not an edge in that document starts
from it.)

With no coordinating document, `--lang go` writes `edges.go` when some public
event's marker lists the fields it publishes (schema 6.2.0):
`"public": { "version": 1, "fields": ["memberId", "branchId"] }`. `EdgesOut`
has each such event with exactly those fields, and `EdgesIn` is empty. An event
whose marker lists nothing publishes nothing without a coordinating document,
since its fields are those some edge maps. So a sender is built from its own
document, and a link written in another repository's view can rely on what it
publishes.

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
| automation wiring | ✅ via `//@dispatches` and `//@target` (a reactor without `//@target` is left out) |
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

This project reads **schema 6.2.0** documents, and an export declares it.
6.2.0 is additive: a scenario accepted with nothing recorded (above); a
document's `expects`, what its system relies on from others (by each one's
document id, with the events and only the fields it uses), read and
generating nothing, since an expected event is never this system's own; and a
public event's marker listing the fields it publishes, which the edge tables
publish (above). Lint refuses a listed field the event does not declare, and
`expects` naming the document's own id.
Import refuses by name anything it cannot apply (above). 4.0.0 is breaking
in one place: a command `scope`'s `memberOfVia` must name its `actorField`,
the field of the membership read model compared with the command's actor;
lint refuses a scope without it. 4.2.0's `public` marker on an event or
command (the contract another system may use, checked across systems by
eventmodelschema's coordinating document and its `architecture-check`) is
read and generates nothing.

**5.0.0 is breaking: an automation names where its command goes.** Every
automation slice has `targetIdField`, a field of each trigger event whose
value is the id of the stream the command acts on, and the command declares
that field with `idAttribute: true`. The generated reactor sends to
`event.data[<field>]`, and nothing else: the target used to be derived (the
trigger's own stream, or a prefix plus the trigger event's id), which could
never reach an existing stream named by a field. Import refuses an
automation without it, or whose field a trigger event or the command lacks,
and (v0.24.0) one whose trigger and command declare a field of the same
name with different types: the reactor copies fields by name.
Verify runs an automation scenario on the stream its trigger names, and
fails one whose command carries another value for the field, or that shows
no trigger event: before, such a scenario passed with its command going to
the wrong stream. An automation that creates something new for each trigger
event needs the trigger to carry that new thing's id.
The value has to be on the event: a command sent directly puts its stream's
id in its body too, or the reaction has no target and is dropped, said.

**Import checks the document against the schema** (since v0.22.0) before
anything else, and refuses it naming each problem: a key the schema requires
and the document leaves out, a value outside an enum, a malformed id. Before,
only keys the importer does not read were refused, so a view scenario's
caller without its `subjectId` imported cleanly. A problem in a split
document is named in the file it is in (`slices/place.json`,
`registries/events.json/placed`).

**Import reports a document's declared version but does not branch on it**,
which is deliberate rather than left over: the 2.0.0 bump fixed the signal
going forward, but a document authored between the v2 schema changes and the
bump declares `1.x` while being a v2 document, and a genuine v1 document
declares the same. Only the shape distinguishes them.

So import checks the *shape*: a slice using the removed `translation` pattern
is refused with an explanation of where it went — it collapses into
`automation`, because a read model never originates an event — and a version
that differs from the one this project reads is noted without being treated as a problem.
