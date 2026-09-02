# Changelog

Tracks `eventModelingSchemaVersion` releases of `schema/eventmodeling.schema.json`.
See `docs/design-notes.md` for the full rationale behind each change.

## 2.4.0

**Additive (non-breaking):**
- `readModel` gains an optional `filters` (new `$def` `readModelFilter`, array of
  `{ param, field, kind, presets }`): declares a single-field WHERE-range query filter
  against one of the read model's own columns, with named presets (`last7Days`/
  `lastCalendarMonth`/`custom`, new `$def` `dateRangePreset`) rather than a raw date
  range. `kind` is currently always `"dateRange"` (new `$def` `filterKind`), shaped as a
  discriminator for a possible future second kind.

See `docs/design-notes.md` ("v2.4.0: `readModel.filters`...") for why presets are a
closed enum, the documented (not schema-enforced) runtime `queryParams` value
convention, and why this deliberately does not solve `staffTotals`-style cross-row
correlation.

A 2.3.0 document validates unchanged against 2.4.0.

## 2.3.0

**Additive (non-breaking):**
- `fieldDerivationKind` gains `"groupBy"` — a nested/grouped-rollup fold, computing a
  `cardinality: "list"` field's `subfields` as one row per distinct value of a source
  event payload field (`groupByField`), with each subfield computed within its group by
  an ordinary nested `sum`/`count`/`toggle` `derivation`. `field` gains a matching
  cross-property constraint: `derivation.kind: "groupBy"` requires `cardinality: "list"`
  and a non-empty `subfields`.

See `docs/design-notes.md` ("v2.3.0: grouped-rollup derivation (`groupBy`)") for why
this reuses `field`'s existing recursive `subfields` shape rather than adding a new
`$def`, and for what it deliberately does not solve (row-scoping by date range,
value-filtering contributing events — see the still-open `dateRange` capability).

A 2.2.0 document validates unchanged against 2.3.0.

## 2.2.0

**Additive (non-breaking):**
- `field` gains an optional `derivation` (new `$def` `fieldDerivation`): computes a
  read-model field as a fold over named events instead of copying a same-named
  payload key. Three kinds — `toggle` (`onEventIds`/`offEventIds`/`initial`),
  `count` (`incrementOnEventIds`/`decrementOnEventIds`/`rowKeyField`), `sum`
  (`addOnEventIds`/`subtractOnEventIds`/`amountField`/`rowKeyField`).
- `event` gains an optional `endsStream` (boolean, default `false`) — marks an
  event that resets a stream's synthesized existence to `false`, the write-side
  counterpart of a `toggle`'s "off" event.
- `readModel` gains an optional `scopes` (new `$def` `readModelScope`, array of
  `{ param, via: { readModelId, matchParamTo, selectField, filterLocalField } }`):
  declares that a stateView query param resolves through a different read model
  rather than naming one of this read model's own columns.

See `docs/design-notes.md` ("v2.2.0: derived read-model fields, stream-ending
events, scoped queries") for the four recurring codegen gaps this closes and why
each shape landed where it did.

A 2.1.0 document validates unchanged against 2.2.0.

## 2.1.0

**Additive (non-breaking):**
- `sliceStatus` gains `"accepted"` — the notation-layer value for a slice that has
  been reviewed and signed off for build, distinct from `"review"` (under review,
  not yet agreed) and `"done"` (built). Authoring workflows that move a slice
  `planned → accepted` on committing to build it were producing documents no
  `sliceStatus` value fit, which surfaced as a confusing cascade: an out-of-enum
  `status` fails the `sliceBase` `$ref` inside `slice`'s `allOf`, so its evaluated
  properties are dropped and the sibling `unevaluatedProperties: false` then flags
  `id`/`name`/`swimlaneId`/`chapterId`/`businessCapability`/`status` on every
  slice. The `slice` shape itself was never at fault. See design-notes.md
  ("Slice status has a conventional default...").

A 2.0.0 document validates unchanged against 2.1.0.

## 2.0.0

**Breaking:**
- Removed the `translation` slice pattern and scenario kind entirely
  (`Event(s) → Read Model → Event(s)`, no command). It never had a valid
  Given/When/Then shape once checked against primary EventModeling sources
  (Adam Dymitruk's own article and a canonical worked blueprint) rather than a
  secondary cheat sheet — a Read Model can be consulted for context but never
  originates an Event; only a Command can. A v1 document using
  `pattern: "translation"` will not validate against 2.0.0. See design-notes.md
  ("v2: `translation` removed...").

**Additive (non-breaking):**
- Optional typed `field` system (name/type/optional/cardinality/`idAttribute`/`pii`/
  recursive `subfields`) on `event`, `command`, and `readModel` definitions.
- `automation`'s `readModelId` changed from required to optional, supporting
  stateless boundary-crossing automations ("Bridge") that go straight from event
  to command with no persisted state.
- Optional `aggregate` (string, type-level) tag on `event` and `command`
  definitions.
- A multi-file composition layer: `schema/manifest.schema.json` plus
  `schema/scripts/{split,join,roundtrip-check}.js`, letting a document be split
  into a manifest + one file per registry/slice and joined back losslessly. No
  change to the core document schema's shape.

A v1.0.0 document with no `translation` slices, no `fields`, no `aggregate` tags
validates unchanged against 2.0.0.

## 1.0.0

Initial draft: swimlanes, the 5 elements (Event/Command/ReadModel/Screen/
Automation), 4 slice patterns (State Change/State View/Automation/Translation),
scenarios, and the optional notation layer (hotspots, chapters, actor lanes,
slice status).
