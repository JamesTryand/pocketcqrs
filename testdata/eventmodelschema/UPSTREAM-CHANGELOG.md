# Changelog

Tracks `eventModelingSchemaVersion` releases of `schema/eventmodeling.schema.json`.
See `docs/design-notes.md` for the full rationale behind each change.

## 2.7.0

**Additive (non-breaking):**
- `readModel` gains `requiredRole` — the read-side mirror of `command.requiredRole`
  (2.5.0): the actor's own role must be a member of the declared role(s) to query
  this read model at all. Raised by `project/timesheets`
  (`build-plan/00-decisions-and-blockers.md` D12): every generated read-model query
  route required only *an* authenticated actor, never a role, so any signed-in user
  could read any tenant's full data (confirmed in a real browser — a Staff-role
  account reading the full staff roster including hourly costs).
- **Internal rename, no document-visible change**: the `$def` backing `requiredRole`
  (`command.requiredRole`, `commandFieldGatedRole.requiredRole`, and now
  `readModel.requiredRole`) is renamed from `commandRole` to `roleRequirement` — it
  was never actually command-specific (already shared by `commandFieldGatedRole`
  before this release), and reads oddly once a `readModel` property references it
  too. `$def` names aren't part of a document's own vocabulary — nothing an author
  writes changes.

A 2.6.0 document validates unchanged against 2.7.0. Consuming `readModel.requiredRole`
(a generated `ReadModelAuthorization` check in every generated query route,
structurally parallel to `CommandAuthorization`) is `platform/eventmodeling-codegen`'s
job, not this schema's — same split every other read/write-side capability here has
kept (`filters`, `asOf`, `command.requiredRole` itself).

## 2.6.0

**Additive (non-breaking):**
- `readModelQuery` (a scenario's `when`/`then` for a `stateView` slice) gains an
  optional `asOf` (ISO 8601 date, `YYYY-MM-DD`), sibling to `queryParams`. Meaningful
  only alongside a `filters`-declared (2.4.0) `dateRangePreset` param: when present, a
  verify runner should resolve `last7Days`/`lastCalendarMonth` against `asOf` instead of
  the live clock, letting a scenario pin "today" once instead of drifting out of its own
  window on a rolling cadence. Absent, behavior is unchanged (today's live-clock
  resolution).
- Consuming `asOf` (stubbing the verify-time clock) is a generator's job
  (`platform/eventmodeling-codegen`), not this schema's — same split as `filters`
  itself.

A 2.5.0 document validates unchanged against 2.6.0.

## 2.5.0

**Additive (non-breaking):**
- `command` gains four optional authorization declarations, from the signed-off
  `platform/command-authorization` design proposal:
  - `requiredRole` (new `$def` `commandRole`: a role id, or a non-empty array of
    role ids) — the actor's own role must match one of them.
  - `fieldGatedRole` (new `$def` `commandFieldGatedRole`: `{ field, value,
    requiredRole }`) — `requiredRole` applies only when the command payload's
    `field` equals `value`.
  - `requiredOwnership` (new `$def` `commandOwnership`: `{ bypassRoles?, via:
    { readModelId, keyField, ownerField } }`) — the actor's own id must equal
    the value of `ownerField` on the read-model row `via` resolves (keyed by
    the command's own target), unless the actor's role is in `bypassRoles`.
  - `scope` (new `$def` `commandScope`: `{ bypassRoles?, resolveVia: {
    readModelId, keyField, selectField }, memberOfVia: { readModelId,
    matchField } }`) — resolves a value from the target's own read-model row,
    then the actor's own id must be a member of the set `memberOfVia`
    resolves for that value, unless the actor's role is in `bypassRoles`.

  `requiredOwnership` and `scope` are kept as two separate declarations rather
  than merged into one — see `docs/design-notes.md` for why (a genuine second
  resolution hop distinguishes them: equality-against-actor vs.
  set-membership).

See `docs/design-notes.md` ("v2.5.0: command authorization") for the full
design rationale, including why `requiredOwnership`'s resolution needed a
read-model `via` lookup rather than a bare field name, and what this addition
deliberately leaves to the host layer (how an actor's role claim itself gets
populated — including for an "administrator" role that lives outside any
`staff`-shaped aggregate).

A 2.4.0 document validates unchanged against 2.5.0.

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
