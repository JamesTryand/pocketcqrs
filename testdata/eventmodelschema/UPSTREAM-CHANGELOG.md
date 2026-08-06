# Changelog

Tracks `eventModelingSchemaVersion` releases of `schema/eventmodeling.schema.json`.
See `docs/design-notes.md` for the full rationale behind each change.

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
