# Consuming PocketCQRS

PocketCQRS serves two audiences over HTTP: **the query side** (PocketBase's
own REST/realtime API over projection-owned collections) and **the
operational API** (the log feed, streams, dead letters, catalog and the admin
barrier — see [gateway reference](reference/gateway.md#operations)).

Everything that consumes the platform — a customer-facing SPA, the ops
dashboard, an external read-model sink — is a client of those two APIs. There
is no privileged inside track: `pocketcqrs-dashboard` is deliberately built
as an ordinary external consumer, importing no pocketcqrs package, so any
pattern it uses is available to you.

Two things shape every deployment decision below:

- **The command API is the only write path.** Guarded collections reject
  direct record writes for everyone, superusers included. Reads are ordinary
  PocketBase; writes are `POST /api/cqrs/{aggregate}/{id}/{command}`.
- **The operational API is superuser-scoped.** Never expose it, or a
  superuser token, to a browser you do not control. Anything holding that
  token is an admin surface.

## Pattern 1 — same-origin static frontend (`pb_public/`)

PocketBase serves `pb_public/` at the root of the same origin as the API.
Drop a built SPA in there and it is done: no second process, no CORS, no
proxy.

```
pb_public/
  index.html
  assets/…
```

```js
// same origin — no base URL, no CORS preflight
const res = await fetch('/api/collections/orders/records');
const cmd = await fetch(`/api/cqrs/order/${id}/ConfirmOrder`, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json', Authorization: token },
  body: JSON.stringify({}),
});
```

Best for: the application's own UI, where the users are PocketBase auth
records and every write is a command. This is the lowest-friction option and
the right default.

Not for: anything needing a superuser token in the browser. Users authenticate
as themselves; the gateway stamps `metadata.actor` from that token.

## Pattern 2 — separate service behind a reverse proxy

Run the consumer as its own process — the ops dashboard is exactly this —
and put both behind one proxy. The consumer talks to PocketCQRS
**server-to-server**, so the superuser token never reaches the browser and
CORS never enters the picture.

```sh
pocketcqrs serve --http 127.0.0.1:8090
pocketcqrs-dashboard --backend http://127.0.0.1:8090 --listen 127.0.0.1:8091
```

Bind both to loopback and let the proxy own the public interface.

### Caddyfile — subdomain per service

```caddyfile
api.example.com {
	reverse_proxy 127.0.0.1:8090
}

dash.example.com {
	# the ops dashboard is an admin surface: put it behind whatever
	# your edge uses for staff access, not on the open internet
	reverse_proxy 127.0.0.1:8091
}
```

### Caddyfile — one host, path rules

```caddyfile
example.com {
	# PocketBase realtime is Server-Sent Events; Caddy proxies it as-is,
	# but disable response buffering if you have enabled it globally
	handle /api/* {
		reverse_proxy 127.0.0.1:8090
	}
	handle /_/* {
		reverse_proxy 127.0.0.1:8090
	}
	handle /ops/* {
		reverse_proxy 127.0.0.1:8091
	}
	handle {
		reverse_proxy 127.0.0.1:8090
	}
}
```

Path rules keep everything on one origin (still no CORS), at the cost of a
shared cookie scope between the app and the admin surface — prefer
subdomains when the dashboard is reachable by anyone but operators.

Both snippets get automatic HTTPS from Caddy. For nginx the equivalent is
`proxy_pass` per `location`, plus `proxy_buffering off;` and
`proxy_read_timeout` raised on `/api/realtime` so SSE streams are not cut.

Best for: ops tooling, staff dashboards, anything that must hold a superuser
token. Also the pattern for an **external read-model consumer**: tail
`GET /api/cqrs/events?after=<checkpoint>` from your own process, keep your own
checkpoint, and write into whatever store you like — the same shape as an
in-process projection, without touching `events.db`.

## Pattern 3 — embed the consumer in-process

PocketBase can serve arbitrary Go routes, so a consumer *can* be compiled
into the same binary and mounted on its own route prefix. That removes the
second process and the proxy hop.

This is a **topology change, not a rewrite**: a consumer written against the
public HTTP API keeps working, because the API it calls is the same one, now
reachable at `127.0.0.1` inside the process. Deferred rather than dismissed —
the dashboard stays a separate binary while its API surface is still moving,
since a separate process is what proves the API is genuinely public.

Cost to weigh: an in-process consumer shares the backend's lifecycle,
crash domain and release cadence, and a bug in it can take the write side
down with it.

## Choosing

| | same-origin `pb_public/` | separate service + proxy | in-process |
| --- | --- | --- | --- |
| Extra moving parts | none | one process + proxy | none |
| CORS | none | none (server-to-server) | none |
| Superuser token in browser | never | never | never |
| Fits | app UI | ops tooling, external sinks | mature, stable consumers |
| Independent deploy | with the backend | yes | no |

## Auth in a server-to-server consumer

The dashboard's flow is the one to copy:

1. `POST /api/collections/_superusers/auth-with-password` with the operator's
   credentials → a token. **PocketBase answers failed logins with `400`**, not
   `401`; map it yourself.
2. Store the token in an `HttpOnly`, `SameSite=Lax` cookie. The browser never
   sees it and never calls PocketCQRS directly.
3. Send it as the `Authorization` header on every backend call, and treat a
   `401`/`403` as "session expired" — clear the cookie and re-prompt.

For unattended consumers (a read-model sink, a batch job), authenticate the
same way at startup and re-authenticate on the first `401`, rather than
holding a token indefinitely.

## Querying read models with declared `scopes`/`filters`

An `eventmodelschema` document may declare a read model's `scopes` (a
semi-join query param resolved through another read model) or `filters` (a
`dateRange` param narrowing one of the read model's own fields, schema
2.4.0). **Neither generates an HTTP route.** Every read model is already
served generically at `GET /api/collections/{collection}/records`, and
PocketBase's own `filter=` query syntax covers both shapes directly —
confirmed against a real read-model collection carrying both a relation
field and a date field (`functions.TestPocketBaseFilterCoversScopesAndDateRange`),
not assumed from the plain unscoped example above.

**A `scopes` semi-join is a relation-traversal filter.** If the read model's
own column is a `relation(<target>)` field (`//@schema`; see
[`js-guide.md`](js-guide.md) and [`reference/directives.md`](reference/directives.md)),
PocketBase's dot-notation lets a filter traverse it directly — no separate
lookup query, no local `IN (...)` set to build client-side:

```
GET /api/collections/time_entries/records
    ?filter=projectId.managerId = "<pmRecordId>"
```

This does the same job `ReadModelScope`'s `via`/`matchParamTo`/`selectField`/
`filterLocalField` shape describes (and the same job `emschema/verify.go`'s
`filterByScopes` simulates for scenario checking) — but as one relation
column and one filter clause, rather than a resolved id set. If the read
model's own field is a plain column instead of a `relation(...)` (the shape
`ReadModelScope` itself assumes — see `docs/schema.md`'s field-types table),
resolve the id set with a separate request first, the same way any
client-side join would.

**A `dateRange` filter is a named preset translated to a literal date
range.** `readModel.filters`' `presets` (`last7Days` / `lastCalendarMonth` /
`custom`) name date math PocketBase's `filter=` has no concept of — the
*translation* is this project's job, not PocketBase's:

```js
// client-side (or a small server-side helper with the same table) —
// resolve a named preset to concrete bounds, then build an ordinary filter=
function dateRangeBounds(preset, custom) {
  const fmt = d => d.toISOString().slice(0, 10) + ' 00:00:00.000Z';
  const today = new Date();
  switch (preset) {
    case 'last7Days': {
      const from = new Date(today); from.setUTCDate(from.getUTCDate() - 6);
      return { from: fmt(from), to: fmt(today) };
    }
    case 'lastCalendarMonth': {
      const firstOfThisMonth = new Date(Date.UTC(today.getUTCFullYear(), today.getUTCMonth(), 1));
      const lastMonthEnd = new Date(firstOfThisMonth - 1);
      const from = new Date(Date.UTC(lastMonthEnd.getUTCFullYear(), lastMonthEnd.getUTCMonth(), 1));
      return { from: fmt(from), to: fmt(lastMonthEnd) };
    }
    case 'custom':
      return { from: custom.from, to: custom.to };
  }
}

const { from, to } = dateRangeBounds('last7Days');
const filter = `taskDate >= "${from} 00:00:00.000Z" && taskDate <= "${to} 23:59:59.999Z"`;
fetch(`/api/collections/time_entries/records?filter=${encodeURIComponent(filter)}`);
```

This mirrors `emschema/verify.go`'s `resolveDateRangeFilter`, the Go-side
equivalent used to check a `dateRange` scenario against the generated
projection at import time — same preset math, same `{"kind": "last7Days"}` /
`{"kind": "custom", "from": "...", "to": "..."}` runtime convention Stage 1
of `eventmodelschema` 2.4.0 documented, just resolved against the wall
clock on the query side instead of a scenario fixture.

Both can combine in one request, exactly as a `pmStaffId`-scoped,
`dateRange`-filtered `time-entries` query would need to:

```
GET /api/collections/time_entries/records
    ?filter=projectId.managerId = "<pmId>" && taskDate >= "..." && taskDate <= "..."
```
