# Gateway (HTTP) reference

PocketCQRS adds four route groups to the stock PocketBase API (which serves
queries unchanged — see any PocketBase docs for `/api/collections/...`).

## Commands

```
POST /api/cqrs/{aggregate}/{id}/{command}
```

Body: the command payload as JSON (may be empty; defaults to `{}`).

**Auth**: a PocketBase record or superuser token is required unless
`--cqrsAllowAnonymous`. The authenticated record is stamped into every
resulting event's metadata:

```json
{ "actor": "<record id>", "actorCollection": "<collection>", "now": "<UTC ts>" }
```

**Responses**:

| status | when |
| --- | --- |
| `200` | `{ "events": [...] }` — the appended envelopes (empty when the decider returned no events) |
| `400` | domain/validation rejection from the decider (its thrown message) |
| `401` | no/invalid auth token |
| `404` | unknown aggregate |
| `409` | concurrency conflict (the stream changed between load and append; retry the command) |
| `503` | maintenance mode — `{ error, hint }`; retry after `system maintenance off` |

**Idempotency**: deciders that reject already-applied intents (e.g. "task
already exists") make retried commands safe — a `400` after a timed-out
`200` means the first attempt landed.

## HTTP functions

```
GET|POST /api/fn/{name}
```

Runs the `handle(request)` of `pb_functions/<name>.js`. Same auth rules as
commands. The function shapes its own response: returning
`{status, body, headers}` is honored as-is; anything else is a `200` JSON
body. Function errors return `500` with the error message (also logged).

## Admin

```
POST /api/cqrs/admin/reload
```

**Auth**: superuser token required (`401` otherwise).

Hot-reloads the functions directory. Everything compiles/validates before
any live structure is touched — a broken file aborts with `400` and the
previous code keeps serving.

- **Any mode**: effect (event), HTTP and cron functions are swapped;
  durable checkpoints carry by name.
- **Maintenance only** (`system maintenance on`): JS projection schemas are
  reconciled (additive), JS projection consumers are swapped, and JS
  deciders are re-validated then swapped — a decider that fails validation
  keeps its previous code; removed files unregister (404); built-in Go
  aggregates can never be displaced. The store's upcaster is rebuilt from
  the validated decider set.

**Response** `200`:

```json
{
  "mode": "maintenance",
  "effectsReloaded": ["fn:task_audit.js"],
  "httpReloaded": ["hello"],
  "cronReloaded": ["heartbeat.js"],
  "schemaTier": "reloaded",
  "projectionsReloaded": ["notes"],
  "decidersReloaded": ["note"],
  "decidersRemoved": [],
  "decidersRefused": []
}
```

(`schemaTier` is `"skipped: not in maintenance"` when running; the
`*Removed`/`*Refused` fields appear only when non-empty.)

### Function files

```
GET    /api/cqrs/admin/functions           list the functions directory
GET    /api/cqrs/admin/functions/{name}    read one file's source
PUT    /api/cqrs/admin/functions/{name}    write it   body: {"source": "..."}
DELETE /api/cqrs/admin/functions/{name}    remove it
```

**Auth**: superuser token required (`401` otherwise).

**Trust model — stated plainly**: these routes write code that the server
executes in-process (goja). That is deliberate and it is not a new exposure:
functions are trusted, owner-authored code by design, `pack import` already
writes the same files, and a superuser can already reload them. Untrusted
function code remains the open question a wasm runtime would answer. Treat
anything holding a superuser token as an administrative surface.

`{name}` must be a single `.js` file name — letters, digits, dot, dash and
underscore, starting with a letter or digit. Anything else is a `400`:
separators, `..`, absolute paths and Windows device names (`CON.js`) are all
refused, and the resolved path is re-checked to be a direct child of the
functions directory.

The listing reports what each file declares, as the loader reads it:

```json
{
  "dir": "pb_functions",
  "files": [
    {"name": "task_audit.js", "size": 412, "modified": "2026-08-04 09:00:00.000Z",
     "declaration": {"kind": "effect", "eventTypes": ["TaskCreated"], "schemaBearing": false}},
    {"name": "notes.js", "size": 690, "modified": "2026-08-04 09:01:00.000Z",
     "declaration": {"kind": "projection", "projection": "notes",
                     "collections": ["notes"], "schemaBearing": true}}
  ]
}
```

`kind` is `effect` (event/http/cron), `projection`, `decider`, or `none` for a
file with no directives — which the loader ignores, and a reload drops.
`schemaBearing` is what decides whether activation needs the maintenance
barrier. A file that does not parse is listed **with its `error`** rather than
omitted: it is the file that blocks every reload, so it is exactly the one
worth finding.

**A write is refused unless the source loads** (`400` with the parse or
compile error). Reloads are all-or-nothing, so an unloadable file in the
directory would abort every later reload — including the one that fixes it.

**Saving is not activating.** `PUT` and `DELETE` answer with
`"active": false` and a hint: nothing is live until
`POST /api/cqrs/admin/reload`, and a schema-bearing file needs maintenance
mode first. A deleted file keeps serving until that reload drops it.

### Dry run

```
POST /api/cqrs/admin/dryrun
body: {"name": "note.js", "source": "...", "mode": "decider",
       "streamId": "n1", "command": "CreateNote", "payload": {}, "diff": false}
```

Runs candidate source against real history **without appending events or
touching collections** — the HTTP face of the [`dryrun` CLI](cli.md#dryrun),
and the check to run before saving. The candidate is compiled into a scratch
runtime, never the live one.

`mode` is required and explicit — the caller knows the file's kind from the
listing, and a server-side guess would fail by silently running the wrong
check:

| mode | what it does |
| --- | --- |
| `compile` | parses and compiles only — the whole check for effect/http/cron functions |
| `decider` | applies the same gate a reload does — contract probe plus `//@handles` coverage — then folds existing streams (`streamId` limits it to one, and returns its final state). Passing means a reload would accept the decider, which folding alone would not tell you for an aggregate with no history yet |
| `decide` | reports the events a command **would** produce on a real stream; needs `streamId` and `command` |
| `projection` | simulates the projection over the log in memory; `diff: true` also compares against the live collections |

Every mode answers `200` with `ok`, a `summary` sentence and its mode-specific
fields; a candidate that fails to load or fold is a `400` carrying the error.
The `diff` caveat from the CLI applies here too: read-modify-write projections
read live state during simulation, so only absolute-recompute projections are
expected to come back clean.

### Catalog

```
GET /api/cqrs/catalog
```

**Auth**: superuser token required (`401` otherwise).

Returns the platform catalog as JSON — the same document the
[`catalog` CLI](cli.md#catalog) renders as Markdown: aggregates, empirical
event types, consumers with checkpoints, guarded collections, functions and
reactor flows, plus mode and log totals.

`totals` carries `events`, `maxPosition`, `streams` and
`deadLettersPending`. Measure consumer lag against **`maxPosition`**, not
`events`: checkpoints record a position, and positions are `AUTOINCREMENT`,
so an event count is not interchangeable with the head of the log.

The `mermaid` field carries the platform flowchart as Mermaid source (the
same rendering the CLI embeds in Markdown), so consumers can show the
diagram without reimplementing the renderer.

## Operations

The operational API is the platform's public log interface — dashboards and
out-of-process read-model consumers tail the log through it instead of
touching `events.db` (preserving the single-process model). All routes
require a superuser token (`401` otherwise); all reads see events at their
latest schema version (store-level upcasting).

```
GET /api/cqrs/events?after=&before=&limit=&aggregate=&aggregateId=&type=
```

The log feed, always returned in ascending global position order. `limit`
defaults to 100 and caps at 1000. Response: `{ "events": [...] }`.

Filters: `aggregate` and `type`; `aggregateId` narrows to a single stream
(pair it with `aggregate` — stream identity is aggregate + id).

Bounds: `after` is exclusive (0 = from the start), `before` is exclusive
(0 = no upper bound). Page **forward** with `after` = the last seen position;
page **backward** with `before` = the first seen position.

`before` is not sugar for `after − limit`: under a filter the matching
positions are sparse, so the previous page cannot be computed by
subtraction. When `before` is set the batch is taken from the `before` end
of the range, so if both bounds are given, `after` acts as a floor guard
rather than the start of the window.

The catalog's `totals.maxPosition` is the companion to this feed: it is the
head of the log, so `maxPosition − checkpoint` is how far a consumer is
behind. See [Catalog](#catalog) above.

```
GET /api/cqrs/streams?aggregate=
```

One row per stream: `{ "streams": [{ aggregate, aggregateId, events,
lastPosition, updated }] }`, ordered by aggregate and stream id.

```
GET /api/cqrs/deadletters?all=
```

Failed function deliveries: `{ "deadLetters": [...] }` — pending only unless
`all=1`.

```
POST /api/cqrs/deadletters/{id}/retry
POST /api/cqrs/deadletters/retry             (every pending dead letter)
POST /api/cqrs/deadletters/{id}/dismiss
```

`retry` re-delivers the captured event envelope through the **current**
function code — fix the function, reload it, then retry. It answers
`200` with

```json
{ "id": 3, "consumer": "fn:audit", "resolved": false, "attempts": 4, "error": "..." }
```

A retry that fails again is **not** an HTTP error: a poison event staying
poison is the ordinary case, and a caller has to tell it apart from a broken
endpoint. Successes set `resolved: true`; failures record another attempt and
return the new failure text. `4xx` is reserved for a malformed or unknown id.
The bulk form answers `{ "results": [...] }` with one entry per pending
letter, oldest first.

`dismiss` resolves a dead letter without re-delivering it (`404` for an
unknown id). Both routes adjudicate exactly like the
[`deadletter` CLI](cli.md#deadletter) — they share one implementation.

```
GET  /api/cqrs/admin/mode
POST /api/cqrs/admin/mode   { "mode": "running" | "maintenance" }
```

Reads or sets the system mode barrier. `POST` validates like
`store.SetMode`: an invalid mode is a `400` and the current mode is
unchanged. While `maintenance`, domain commands are rejected (`503`) and
schema-bearing functions may be reloaded.

## Write-guard (all collections API routes)

`POST/PATCH/DELETE /api/collections/<guarded>/records` on any
projection-owned collection returns `403` for everyone, superusers
included:

> Direct writes to '\<collection\>' are disabled; state changes must go
> through the command API (POST /api/cqrs/{aggregate}/{id}/{command}).

The only writers are projections (server-side, via an internal marker that
never crosses the HTTP boundary).
