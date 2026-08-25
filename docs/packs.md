# Domain packs

A **domain pack** is a portable bundle of a domain's runtime-defined parts:

```
notes-pack/
  manifest.json          # name, version, description, file lists
  pb_functions/          # the domain's JS: deciders, projections, effects, http, cron
    note.js
    notes.js
  collections.json       # optional: plain collections (PocketBase's native import format)
```

Projection-owned collections are deliberately **not** packed — they are
recreated from `//@schema` directives at the target (boot reconcile or
maintenance reload). Only *plain* collections (reference data, lookup
tables) belong in `collections.json`.

> **Trust**: packs are trusted, owner-authored JavaScript running in-process
> (goja). Only import packs you trust and have reviewed — a pack is code.
> Importing **untrusted** packs (marketplace-style, third-party tenants) is
> the documented trigger for the isolated-runtime (wasm) decision gate;
> until then there is no sandbox.

## Export

The worked example below packs the `note` domain. Its two files ship in
`examples/pb_functions/` — copy them into your functions directory first if
you want to follow along (`cp examples/pb_functions/note*.js pb_functions/`).
`note` needs no `--tutorial`: it is defined entirely in JS.

```sh
pocketcqrs pack export ./notes-pack --name notes-domain --version 1.0.0 \
  --functions note.js,notes.js
# or the whole functions dir:
pocketcqrs pack export ./everything-pack --name platform --collections labels
```

- `--functions` defaults to every `.js` in `--functionsDir`.
- `--collections` names plain collections to include; projection-owned
  (write-guarded) and system collections are refused.
- Export **load-validates** the files: a pack that doesn't parse/compile is
  refused before anything is written.

## Import

```sh
pocketcqrs pack import ./notes-pack          # existing files are skipped
pocketcqrs pack import ./notes-pack --force  # overwrite (pack upgrade)
```

Import load-validates first, then copies the function files into
`--functionsDir` and applies `collections.json` via PocketBase's native
collection import (never deleting anything). Then activate:

```sh
pocketcqrs system maintenance on
curl -X POST http://127.0.0.1:8090/api/cqrs/admin/reload -H "Authorization: Bearer <token>"
pocketcqrs system maintenance off
```

(or just restart). Deciders are dry-run validated during the reload; a
failing decider is refused and reported in the reload response.

## Event data (slice/merge)

`pack export`/`pack import` move a domain's **code**. `events export`/
`events import` are a separate, deliberately opt-in pair of commands that
move a domain's **committed event history** — for two specific use cases
only:

- **Slicing** one pack out of a running deployment into its own instance.
- **Merging** two independently-built deployments into one.

**Never** for the ordinary dev→production promotion workflow above — that
should never move production's actual event data, which is exactly why this
is a separate verb instead of a flag on `pack export`/`pack import`: a
promotion habit can't carry `--with-events` by muscle memory into a run that
should never touch it.

First, declare which aggregate names the pack claims — the selection
boundary for what gets exported:

```sh
pocketcqrs pack export ./notes-pack --name notes-domain --aggregates note
pocketcqrs events export ./notes-pack
# writes ./notes-pack/events.ndjson
```

At the target, **import the pack's code first, activate it, and only then**
import its event data:

```sh
pocketcqrs pack import ./notes-pack
# ... reload behind the maintenance barrier, as above ...
pocketcqrs events import ./notes-pack --dry-run   # preview first
pocketcqrs events import ./notes-pack
```

`events import` refuses outright — before reading `events.ndjson` at all —
if the pack's own function files aren't already present in
`--functionsDir`. This ordering matters for a reason specific to this
mechanism, not just tidiness: importing event data fast-forwards every
currently-registered **effect-tier** consumer's checkpoint (reactors and
event-triggered effect functions — never pure projections) past the
imported batch, so nothing with a real side effect re-fires against history
that already happened at the source deployment. A reactor belonging to the
pack being imported has to already be registered for that fast-forward to
cover it; import the data first and that reactor would start from
checkpoint 0 and dispatch against the entire imported history instead.

Two collision checks, both **before any write**, matching `pack import`'s
own refuse-on-file-collision stance rather than silently renaming anything:

- an exact `(aggregate, aggregateId)` match against an existing stream, and
- the batch naming any aggregate **name** the target already has ANY stream
  of, even under a different id.

The second check is intentionally strict: two systems merging almost always
share ordinary vocabulary (both track a `task` aggregate), so it refuses
even when the ids are disjoint. There's no override today — decide the
naming collision deliberately before merging, rather than importing past it.

Pure projections (Go and JS) are **not** fast-forwarded — they replay the
imported history normally (the same mechanism `projection rebuild` already
proves safe) so the merged or sliced data actually materializes in your
read models.

Never route event data through git — `manifest.json`/`pb_functions/`/
`collections.json` are text and diffable; `events.ndjson` can be large and,
for a real deployment, sensitive. Move it directly between systems (or via
a private file transfer), never as a repo commit.

## Extending a pack over time

The rules that make a pack safely evolvable are the platform's two
append-only contracts:

### Events evolve append-only

- **Add properties, never remove/rename.** Old history keeps its shape;
  new events add fields.
- **A property change needs a transform.** Bump the version and declare the
  upcaster in the decider file:

  ```js
  //@transform NoteCreated 1 2
  function transform_NoteCreated_1_to_2(data) { data.priority = data.priority || "normal"; return data; }
  ```

  Transforms run at the store's read path — every consumer (deciders,
  projections, effects) sees the latest version. Emit new events at the
  latest version.
- **A genuinely different shape is a new event type** (`NoteCreatedV2`-style
  names are a smell; prefer a new type like `NoteDrafted`), with the
  projection handling both.

### Schemas evolve additively

- `//@schema` reconciliation creates missing collections and fields and
  **never** removes, retypes or renames. You can always add a field or a
  collection; you cannot take one away through a pack (do that
  deliberately, via migration, at the target).
- Relations (`field:relation(<collection>)`) may target collections the
  pack doesn't own — declare the dependency in the pack's description and
  keep the target name stable.

### Workflow for a pack change

1. Bump `manifest.version`; note the change in the description/changelog.
2. Change the code; add transforms for event-shape changes.
3. **Dry-run before shipping**, against a copy of a real data dir:

   ```sh
   pocketcqrs dryrun decider pb_functions/note.js
   pocketcqrs dryrun projection pb_functions/notes.js --diff
   ```

   Both run against real history without persisting anything. A decider
   that fails to fold existing streams will also be refused on import
   reload — the dry-run is where you want to find out.
4. Ship; import with `--force` (pack upgrade); reload behind the
   maintenance barrier.

### Namespacing

Events, aggregates, collections and function names are **global** to a
deployment. Packs should prefix their artifacts (`crm.ContactImported`,
`crm_contact` aggregate, `crm_contacts` collection, `crm_sync.js`) so packs
compose without collisions — the catalog (`pocketcqrs catalog`) shows the
merged landscape and makes collisions visible.
