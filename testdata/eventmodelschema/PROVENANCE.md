# eventmodelschema â€” vendored reference copy

Fixtures for the eventmodelschema import/export. Copied verbatim, unmodified.

- **Source**: `github.com/jamestryand/eventmodelschema`
- **Commit**: `aa14961`, *"4.0.0: optional command.scope.message, the
  refusal's own message"* (untagged). Copied 2026-10-17: the schema and
  `state-ownership.json`, whose `approve-loan-request` scope now declares the
  message its refusal scenario expects; nothing else changed upstream.
- **Before that**: `6f0be44`, *"Schema 4.0.0: scope.memberOfVia requires actorField;
  examples and notes in neutral domains"* (untagged: upstream's last tag is
  `v3.9.0`). Copied 2026-10-16. 4.0.0 is **breaking**:
  `command.scope.memberOfVia` now requires `actorField`, the field of the
  membership read model compared with the command's actor (see
  `emschema/document.go`'s `CommandScopeMemberOfVia`). The same commit
  rewrote upstream's worked examples in neutral domains (equipment hire, a
  support desk, room bookings, a grant fund, delivery runs, a tool library);
  `state-ownership.json` also gained a scoped command,
  `approve-loan-request`, using `actorField`. The tests that run the
  examples follow the new names.
- **Previous commit**: `14b448f`, *"Schema 3.17.0: readModel.projection
  (generated | hand)"* (untagged). Copied 2026-10-14, to make the importer
  fail closed (`emschema/supported.go`): every example from 3.2.0 on was a
  fixture the importer had to **refuse**, naming each feature, until that
  feature was implemented here. `minimal.json` was the one that loaded.
- **3.1.1's copy is kept in `v3.1.1/`** (commit `f616eaf`, tag `v3.1.1`). The
  round-trip, join and verify tests use its `order-fulfillment` example,
  which uses nothing past 3.1.1; the current one adds ingresses, delays,
  effects and partitioning. The notes below are that copy's history.
- **Before that**: `f616eaf`, tag `v3.1.1` — *"Schema 3.1.1: match normalizers
  pinned exactly, with test vectors"*, 2026-09-24.
- **Copied**: 2026-10-05, straight from 2.7.0 across three releases, as the
  first step of porting dotnetcqrs's personal-data support. 3.0.0 is
  **breaking**: `field.piiSubject` is required whenever `pii` is true (see
  `emschema/document.go`'s `Field.PIISubject`). 3.1.0 adds the `match` filter
  kind, which the mapper checks and reports lossy until search is generated
  here (`emschema/mapping.go`'s `checkMatchFilter`). 3.1.1 pins the
  normalizers byte-exactly, with test vectors in upstream
  `docs/design-notes.md`; that matters once a hashed index is built here,
  because hashes only match when every stack normalizes identically. The
  worked examples now carry a PII `customerEmail` (subject `customerId`) and
  an `exact` email match filter.
- **Before that**: `c4fcf2c`, tag `v2.7.0` — *"readModel.requiredRole -- the
  read-side mirror of command authorization"*, 2026-09-08 — ported into this
  repo the same day (see `emschema/document.go`'s `ReadModel.RequiredRole`
  and `scaffold.Domain.projection`'s `//@rule` emission), copied 2026-09-08
  (previously `649e86d` / `v2.5.0`, "command authorization (requiredRole, fieldGatedRole,
  requiredOwnership, scope)", 2026-09-03; before that `0acc987` / `2.4.0`,
  "readModel.filters for single-field date-range query filtering",
  2026-09-02; before that `bb4a060` / `2.3.0`, "groupBy derivation for nested
  rollups", 2026-09-02; before that `v2.1.0` / `a9f0d8e7`, "added accepted
  status", 2026-08-30; before that `v2.0.0` / `1b4a01c`, "Bump
  eventModelingSchemaVersion to 2.0.0", 2026-08-06; before that `852989a`,
  "v2 M3: multi-file composition layer"). Two releases skipped straight from
  2.5.0 to 2.7.0 in one refresh — 2.6.0 (`readModelQuery.asOf`, see
  `emschema/verify.go`'s `runViewScenario`/`filterByFilters`) and 2.7.0
  (`readModel.requiredRole`) both landed in `dotnetcqrs` the same day
  (2026-09-08, v0.7.0/v0.8.0) and were ported here together in the same
  pass. The copy is documentation/example fixture only (nothing in this
  repo validates a document against it at runtime; see
  `emschema/document.go`'s own header comment for the Go-side source of
  truth).
- Same author as this repository; no separate licence file exists upstream.

## Contents

| path | upstream path | what it is |
| --- | --- | --- |
| `eventmodeling.schema.json` | `schema/` | the document schema (JSON Schema 2020-12) |
| `manifest.schema.json` | `schema/` | the multi-file composition manifest schema |
| `examples/minimal.json` | `schema/examples/` | smallest valid document: one swimlane, one `stateChange` slice, no notation layer |
| `examples/conditional-outcomes.json`, `refusals.json`, `repeated-outcomes.json`, `read-access.json`, `state-ownership.json`, `rows-per-list-item.json`, `hand-projection.json` | `schema/examples/` | one worked example per feature, each with the scenarios that say what it means (`emschema/upstream_examples_test.go` runs them) |
| `examples/order-fulfillment.json` | `schema/examples/` | worked example exercising all 3 slice patterns, all 3 scenario kinds, typed fields, `aggregate` tags, a hotspot, a chapter, an actor lane |
| `examples/order-fulfillment-split/` | `schema/examples/` | the same document as a manifest + one file per registry/slice |
| `UPSTREAM-CHANGELOG.md` | `CHANGELOG.md` | what changed at each schema version, for reading a document's declared version |

`order-fulfillment.json` and `order-fulfillment-split/` are the same document in
two layouts, so they are also the fixture for "import accepts either form".

## Why vendored rather than fetched

The round-trip loss test needs a real, known-good document that does not change
under it. Upstream is explicitly a moving target ("v2 in progress"), so a test
that fetched `main` would fail for reasons unrelated to this repository.

**Refreshing**: re-copy from a newer upstream release tag, update the **Tag** and
**Copied** lines above, and expect the round-trip expectations to move with it.
Upstream started carrying git tags at `v2.0.0`/`v2.1.0` (2026-08-30); older
refreshes pinned a bare commit hash.

## The version string, as of 2.5.0

The schema's `default` declares `"2.5.0"`. The three examples here
(`minimal.json`, `order-fulfillment.json`, `order-fulfillment-split/`)
predate 2.2.0/2.3.0/2.4.0/2.5.0 and still declare `"1.0.0"`/`"2.1.0"`
themselves â€” left unmodified, since re-stamping an example's own version
string is not part of "copied verbatim, unmodified" and none of them uses a
`groupBy` derivation, `readModel.filters`, or any `command` authorization
keyword anyway. `UPSTREAM-CHANGELOG.md` records what changed at each
version. The last breaking change was the removal of the `translation`
pattern in `2.0.0`; `2.1.0` (a new `sliceStatus` value, `"accepted"`),
`2.2.0` (`field.derivation` toggle/count/sum, `event.endsStream`,
`readModel.scopes`), `2.3.0` (`field.derivation` gains `groupBy`), `2.4.0`
(`readModel` gains `filters`, sibling to `scopes`) and `2.5.0` (`command`
gains `requiredRole`/`fieldGatedRole`/`requiredOwnership`/`scope`) are all
additive only.

**This project still branches on document SHAPE rather than on that field**,
and deliberately so. The bump fixes the signal going forward but cannot fix
documents already written: anything authored between the v2 schema changes and
this bump declares `"1.0.0"` while being a v2 document, and a genuine v1
document with a `translation` slice declares the same. Shape is the only thing
that distinguishes them, and it stays correct whatever the header says. The
version is now worth *reporting*; it is still not worth *trusting*.
