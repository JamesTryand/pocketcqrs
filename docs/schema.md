# EventModeling import and export

`pocketcqrs schema` reads and writes documents in the format defined by
[eventmodelschema](https://github.com/jamestryand/eventmodelschema) — the
JSON Schema behind [EventModeling](https://eventmodeling.org) design tooling.

Both directions are **one-shot**. There is no live connection between a
document and a running platform, and importing does not activate anything.

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
  – 1 field(s) are marked pii and NOTHING here carries that flag: event order-placed.customerEmail
  – slice status is board state and is not preserved (done×2, inProgress×1, planned×1)
```

Read it. A folded type, a defaulted key or a dropped PII flag that nobody is
told about is the failure mode this whole layer exists to avoid.

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

`idAttribute: true` names a read model's row key. `pii` and `optional` have
no home in `//@schema` and are reported as lossy.

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

**The largest gap, stated plainly**: nothing in the runtime links a command
to the events it produces. `//@commands` names the commands, `//@handles`
names the events, and no directive joins a pair — so every exported
`stateChange` slice lists its aggregate's *whole* event set, and a round trip
widens `eventIds`. A `//@produces <Command> <Event...>` directive is the
obvious fix if this ever matters.

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

**The document's `eventModelingSchemaVersion` cannot be used to tell schema
generations apart.** v2 removed enum values — a breaking change — while the
worked examples still declare `"1.0.0"` and the schema's own default is still
`"1.0.0"`. Import therefore branches on the document's *shape*: a slice using
the removed `translation` pattern is refused with an explanation of where it
went (it collapses into `automation`, because a read model never originates
an event).
