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

### Catalog

```
GET /api/cqrs/catalog
```

**Auth**: superuser token required (`401` otherwise).

Returns the platform catalog as JSON — the same document the
[`catalog` CLI](cli.md#catalog) renders as Markdown: aggregates, empirical
event types, consumers with checkpoints, guarded collections, functions and
reactor flows, plus mode and log totals. The `mermaid` field carries the
platform flowchart as Mermaid source (the same rendering the CLI embeds in
Markdown), so consumers can show the diagram without reimplementing the
renderer.

## Operations

The operational API is the platform's public log interface — dashboards and
out-of-process read-model consumers tail the log through it instead of
touching `events.db` (preserving the single-process model). All routes
require a superuser token (`401` otherwise); all reads see events at their
latest schema version (store-level upcasting).

```
GET /api/cqrs/events?after=&limit=&aggregate=&type=
```

The log feed, in global position order. `after` is exclusive (0 = from the
start); `limit` defaults to 100 and caps at 1000; `aggregate` and `type`
filter. Response: `{ "events": [...] }` — page by re-requesting with
`after` = the last seen position.

```
GET /api/cqrs/streams?aggregate=
```

One row per stream: `{ "streams": [{ aggregate, aggregateId, events,
lastPosition, updated }] }`, ordered by aggregate and stream id.

```
GET /api/cqrs/deadletters?all=
```

Failed function deliveries: `{ "deadLetters": [...] }` — pending only unless
`all=1`. Retry/dismiss stays with the [`deadletter` CLI](cli.md#deadletter).

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
