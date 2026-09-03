# eventmodelschema — vendored reference copy

Fixtures for M14 (eventmodelschema import/export). Copied verbatim, unmodified.

- **Source**: `github.com/jamestryand/eventmodelschema`
- **Commit**: `649e86d`, tag `v2.5.0` — *"Schema 2.5.0: command authorization
  (requiredRole, fieldGatedRole, requiredOwnership, scope)"*, 2026-09-03 —
  platform/command-authorization, ported into this repo the same day (see
  `emschema/document.go`'s `Command` type and package `authorize`).
- **Copied**: 2026-09-03 (previously `0acc987` / `2.4.0`, "readModel.filters
  for single-field date-range query filtering", 2026-09-02; before that
  `bb4a060` / `2.3.0`, "groupBy derivation for nested rollups", 2026-09-02;
  before that `v2.1.0` / `a9f0d8e7`, "added accepted status", 2026-08-30;
  before that `v2.0.0` / `1b4a01c`, "Bump eventModelingSchemaVersion to
  2.0.0", 2026-08-06; before that `852989a`, "v2 M3: multi-file composition
  layer"). This refresh is additive-only over 2.4.0 — `command` gains four
  optional keywords (new `$defs` `commandRole`/`commandFieldGatedRole`/
  `commandOwnership`/`commandScope`) — the copy is documentation/example
  fixture only (nothing in this repo validates a document against it at
  runtime; see `emschema/document.go`'s own header comment for the Go-side
  source of truth, and `emschema/lint.go`'s `lintCommandAuth` for this
  repo's own referential-integrity checks over the four new keywords).
- Same author as this repository; no separate licence file exists upstream.

## Contents

| path | upstream path | what it is |
| --- | --- | --- |
| `eventmodeling.schema.json` | `schema/` | the document schema (JSON Schema 2020-12) |
| `manifest.schema.json` | `schema/` | the multi-file composition manifest schema |
| `examples/minimal.json` | `schema/examples/` | smallest valid document: one swimlane, one `stateChange` slice, no notation layer |
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
themselves — left unmodified, since re-stamping an example's own version
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
