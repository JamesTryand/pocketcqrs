# CLI reference

All commands are subcommands of the `pocketcqrs` binary (`go run . <cmd>`).
PocketBase's own commands (`serve`, `superuser`, `migrate`, ...) are
available too. Persistent PocketBase flags: `--dir` (data dir), `--http`
(listen address) for `serve`.

PocketCQRS flags (persistent, on every command):

| flag | default | meaning |
| --- | --- | --- |
| `--functionsDir` | `pb_functions` | directory with user-defined JS functions |
| `--tutorial` | `false` | register this repo's example domains (`task`, `order`), their projections, the fulfillment saga and their collections |
| `--cqrsAllowAnonymous` | `false` | dev: commands and `/api/fn` need no auth token (no actor metadata stamped) |
| `--cqrsStrictBoot` | `false` | abort startup when a JS decider fails validation (default: skip it and keep serving) |
| `--cqrsAllowOutboundHTTP` | `false` | expose `$http` to **event, cron and reactor** functions. Not to `//@trigger http` functions (request-driven, so unbounded concurrency), and never to deciders or projections |
| `--cqrsOutboundHost` | *(none)* | a hostname `$http` may call. Repeatable; deployment-wide, not per-function. **No entries permits nothing** |
| `--cqrsAllowPrivateOutbound` | `false` | dev/internal: let `$http` reach loopback and private ranges. Link-local (`169.254.0.0/16`, the metadata endpoint) stays blocked regardless |
| `--cqrsExternalCallerCollection` | *(none)* | name of a PocketBase auth collection whose authenticated records are external service integrations (e.g. `pocketcqrs-extensions`' `extcaller`), not end users or reactors — see [the Go guide](../go-guide.md) for the `"extcall:<name>"` actor shape this produces |
| `--cqrsSchemaDefaultRule` | *(none, = `public`)* | default `ListRule`/`ViewRule` for newly created `//@schema` collections: `public`, `authenticated`, or a raw PocketBase rule expression. A [`//@rule <collection> <value>`](directives.md#rule-collection-value) directive overrides this per collection. Writes stay write-guarded either way. Never changes an already-existing collection's rule — reconcile stays additive-only, same guarantee field changes already get |

Outbound HTTP is off unless asked for, and asking for it takes two flags —
enabling it with no `--cqrsOutboundHost` refuses every call and warns at boot.

```
pocketcqrs serve \
  --cqrsAllowOutboundHTTP \
  --cqrsOutboundHost api.stripe.com \
  --cqrsOutboundHost hooks.slack.com
```

See [the JS guide](../js-guide.md#calling-out-http-event-cron-and-reactor-functions) for the
binding and the bounds it enforces.

## serve

```sh
pocketcqrs serve [--http 127.0.0.1:8090] [--dir pb_data] [--tutorial]
```

Boots the platform and serves HTTP: PocketBase API + admin UI (`/_`), the
[gateway routes](gateway.md), the consumers engine, cron jobs. Bootstrap
applies app migrations, validates JS deciders, reconciles JS projection
schemas and starts catch-up from the event log.

**Without `--tutorial` the platform registers nothing of its own**: no
aggregates, no Go projections, no collections beyond PocketBase's own and
whatever your JS projections declare. The example migrations are not
registered either, so their collections are never created — and because an
unregistered migration is also never *recorded*, turning the flag back on
later still creates them. The switch works in both directions.

Two consequences worth knowing:

- The names `task` and `order` are free for your own JS deciders. With
  `--tutorial` they are taken, and a colliding JS decider is refused.
- If a previous run *did* create the example collections, they stay behind
  when you drop the flag. They keep their write-guard rather than silently
  becoming writable, and boot logs a warning naming them.

## Multi-node (single writer, multiple readers)

One master appends to `events.db`; any number of secondaries poll a
replicated copy of that file read-only and run their own local projections.
There is no leader election — the master is fixed by configuration.
`data.db` (auth records, settings, signing secrets) is per-node and **never
replicated**; only `events.db` is.

**Verified today**: a secondary pointed at the master's `events.db` by a
plain shared local path (`--cqrsEventsPath`, same machine or a filesystem
both processes see directly) — this is what every test and this project's
own smoke suite uses. **Genuine cross-host replication uses LiteFS** (see
"Cross-host replication with LiteFS" below) — decided and tested at the
same-host, two-process level; not yet separately verified across a real
network, though the mechanism is architecturally the same either way. Do not
point `--cqrsEventsPath` at a network-mounted (NFS/SMB) copy of the file as a
substitute for either approach: `events.db` runs in WAL mode, and SQLite's
own documentation states WAL is not safe over network filesystems (the
`-shm` coordination file needs shared-memory semantics those don't reliably
provide) — a stated incompatibility, not a scale-dependent risk.

| flag | default | meaning |
| --- | --- | --- |
| `--cqrsRole` | `master` | `master` appends to `events.db`; `secondary` polls a replica read-only and refuses local writes |
| `--cqrsEventsPath` | `<dir>/events.db` | where `events.db` lives — a secondary points this at the master's replicated file (a LiteFS FUSE mount path, for genuine cross-host — see below) |
| `--cqrsVFS` | *(none)* | **dead hook — not the cross-host mechanism.** A SQLite VFS name to open `events.db` through on a secondary, if one is already registered with the driver. Nothing in this codebase registers a VFS, and LiteFS (the actual decided mechanism) does not use this flag at all — it works by FUSE-mounting a directory, not by SQLite VFS registration. Leave this unset |
| `--cqrsMasterAddr` | *(none)* | the master's base URL; when set on a secondary, commands are proxied there instead of refused |
| `--cqrsForwardAuth` | `false` | also route PocketBase's own auth-collection traffic (login, token refresh, `_users`/`_superusers` records — reads included) to the master, since a secondary's copies of those tables are unrelated local tables, not replicas |
| `--cqrsVerifyAuth` | `false` | verify bearer tokens against the master with a bounded local verdict cache, so a secondary's own authenticated **local** reads work; implies `--cqrsForwardAuth` |
| `--cqrsVerifyCacheTTL` | `5m` | how long a verdict is trusted before re-checking, always additionally capped by the token's own `exp`. Also the revocation-lag bound |
| `--cqrsVerifyGrace` | `0` | opt-in: how far past expiry a stale verdict may still serve while the master is **unreachable** (never past the token's `exp`). `0` fails closed |

A read-write-capable secondary is these three together:

```sh
pocketcqrs serve --cqrsRole secondary \
  --cqrsEventsPath /replica/events.db \
  --cqrsMasterAddr http://master:8090 \
  --cqrsVerifyAuth
```

`/replica/events.db` above is deliberately generic — same-host, it's a path both processes see
directly; under LiteFS, it's the `events.db` inside that node's own FUSE mount (see below).

### Cross-host replication with LiteFS

Decided after evaluating Litestream first: Litestream's `restore -f` follow mode stalls 30-90+
seconds under a continuously-polling reader (confirmed cross-platform, not a Windows quirk) —
exactly what a live secondary's own read loop is, so it cannot deliver the low-latency replication
this design needs. [LiteFS](https://github.com/superfly/litefs) was tested under the identical
scenario and showed no stall: writes replicate in roughly 1-2s, with no accumulating lag under
sustained load. **`static` lease, not `consul`** — this project's multi-node design has one fixed
master by configuration, deliberately no automatic failover (see "Manual promotion" below), so
`static` needs no extra infrastructure. `consul` lease remains a real option if automatic failover
is ever actually taken up as its own project — not something to reach for by default.

LiteFS is a FUSE filesystem: it intercepts writes to a mounted directory and replicates them to
other nodes, transparently to whatever opens files inside that directory. **The master's own
`events.db` has to be opened from inside the FUSE mount, not a plain path** — this is the one real
structural difference from a design like Litestream's, where a sidecar could tail an
already-running master's plain file. `events.Open`/`events.OpenReadOnly` need no code changes to
work through a FUSE mount — verified directly against both.

LiteFS is not a pocketcqrs dependency (`go.mod` stays clean) — it runs as its own binary, one per
node, alongside `pocketcqrs serve`. Get it from [GitHub
releases](https://github.com/superfly/litefs/releases) or build it from source
(`CGO_ENABLED=1 go build ./cmd/litefs` — needs `gcc`). Linux only: LiteFS is FUSE-based, and FUSE
has no Windows or (without extra setup) macOS story.

#### Config shape

One `litefs.yml` per node. The primary:

```yaml
fuse:
  dir: "/litefs/mnt"       # pocketcqrs's --cqrsEventsPath points inside here
data:
  dir: "/litefs/data"      # LiteFS's own internal storage -- not the FUSE mount
http:
  addr: ":20202"
lease:
  type: "static"
  hostname: "primary"
  advertise-url: "http://master.internal:20202"
  candidate: true
```

Every secondary uses the same shape with `candidate: false` and its own `fuse.dir`/`data.dir` — but
**`lease.advertise-url` is always the primary's address**, on every node including the primary
itself (the primary's own address just happens to equal its own `advertise-url`). This is not a
per-secondary setting to vary; it's the fixed address of whichever node is currently primary.

Run `litefs mount -config litefs.yml` as its own process on every node, then point pocketcqrs's
`--cqrsEventsPath` at `<fuse.dir>/events.db`:

```sh
# on the primary, after `litefs mount -config primary.yml` is running:
pocketcqrs serve --cqrsEventsPath /litefs/mnt/events.db ...

# on a secondary, after its own `litefs mount -config secondary.yml` is running:
pocketcqrs serve --cqrsRole secondary \
  --cqrsEventsPath /litefs/mnt/events.db \
  --cqrsMasterAddr http://master:8090 \
  --cqrsVerifyAuth
```

pocketcqrs must not start reading/writing `--cqrsEventsPath` before its node's `litefs mount` has
actually become primary or finished syncing — starting them as two separately-supervised processes
(the shape tested and documented here) needs that ordering handled by your process supervisor
(health-check gate, or a startup script polling for the primary's log line
`"primary lease acquired"` / the secondary's mount directory appearing). LiteFS also offers an
`exec:` config option that runs a given command as its own supervised subprocess only once the node
is ready, folding both processes into one unit — a real option, not yet exercised by this project's
own testing, which deliberately keeps `litefs mount` and `pocketcqrs serve` as two separate,
independently-inspectable processes.

#### Adopting LiteFS on an existing deployment

A single-instance deployment does not need to decide about LiteFS up front. Run on a plain
`--cqrsEventsPath` for as long as you want; when you're ready for a secondary, migrate the existing
`events.db` in with:

```sh
litefs import -url http://localhost:20202 -name events.db /path/to/existing/events.db
```

against an already-running (empty) primary. Verified directly: all pre-existing data survives, the
primary keeps appending normally afterward, and a secondary attached only after the import receives
the complete history automatically, indistinguishable from a database that lived inside LiteFS from
the start.

**Do not just `cp` the existing file into the FUSE mount** — this fails outright
(`Input/output error`), and if the failure isn't checked, pocketcqrs will silently open a fresh,
empty database at that path instead of the real one. `litefs import` is the only supported path.

#### Restarts

- **A secondary restarting** (same config, same local data dir) resyncs from its own persisted
  state in a fraction of a second — not a from-scratch replica rebuild. Safe to restart routinely
  (deploys, host reboots) without special handling.
- **The primary restarting** with the same config (`candidate: true` unchanged) resumes as primary
  cleanly — re-acquires its lease, all prior data intact, accepts writes immediately. Also no
  special handling needed for an ordinary process restart.

#### Manual promotion (the actual failover mechanism)

`static` lease has **no automated handoff** — a secondary cannot be promoted by any LiteFS command
or API call. If the primary is gone for good, promote a synced secondary manually:

1. Confirm which secondary is most caught up (least lag), if more than one exists.
2. Edit that secondary's `litefs.yml`: `lease.candidate: true`, `lease.advertise-url` set to **its
   own** address.
3. Restart its `litefs mount` process. It becomes primary, retaining everything already replicated
   to it, and immediately accepts writes.
4. Restart `pocketcqrs serve` on that node with `--cqrsRole master` (dropping `--cqrsRole secondary`
   and its forwarding flags).
5. Update every other node's `litefs.yml` (`lease.advertise-url`) to point at the new primary and
   restart them.

**Data-loss window, stated plainly rather than left implicit**: any write that had not yet
replicated to the promoted secondary at the moment the old primary failed is genuinely lost — this
is inherent to any primary/secondary replication design, not a LiteFS defect, but it is exactly what
an operator needs to understand before promoting during a real incident. If the old primary is still
reachable enough to check (a graceful failover, not a hard crash), confirming replication lag is
near zero before promoting avoids this entirely.

### How auth works across nodes

PocketBase verifies a token with per-record + per-collection key material
that lives only in each node's own `data.db` — so no node can verify a
token another node minted, and secrets are deliberately never synced (a
compromised secondary must gain nothing). Instead:

- **Login forwards.** With `--cqrsForwardAuth` (implied by
  `--cqrsVerifyAuth`), every auth flow is proxied to the master, so every
  token a client holds is master-minted.
- **Verification asks the master.** The master exposes
  `POST /api/cqrs/auth/verify` — a validity oracle running exactly the
  check it applies to its own requests, returning the record the token
  belongs to and nothing more. A secondary materializes a local auth
  context from the answer and caches the verdict for `--cqrsVerifyCacheTTL`
  (SHA-256 of the token as the key; the raw token is never stored).
- **Writes always end at the master** and are verified there per request —
  the cache is only ever about a secondary's own local reads.
- **The ops routes never use the cache.** `/api/cqrs/events`, `/streams`,
  `/deadletters/*`, `/admin/*`, `/catalog` re-verify against the master on
  every request, so revoking an operator's token (rotating its `tokenKey`,
  or — for a capability grant, see below — editing it away) bites
  immediately, not after a TTL window.
- **Master unreachable**: a live cached verdict keeps serving; anything
  else answers `503` (not `401` — a re-login cannot work either while the
  master is down), unless `--cqrsVerifyGrace` opts into serving expired
  verdicts for a bounded window. The tradeoff is explicit: grace extends
  availability through an outage and extends the revocation lag by the
  same amount.

Known limits on a secondary: API rules referencing hidden fields of the
authenticated record evaluate against the verdict's serialization, which
omits them; protected-file tokens are not covered; rate-limit state stays
per node.

### Capability-based access below superuser (Item 11)

Five read-only ops routes accept more than a superuser token:
`GET /api/cqrs/events`, `/streams`, `/deadletters`, `/admin/mode`, and
`/catalog`. Every other ops/admin route (dead-letter retry/dismiss, the
`POST /admin/mode` mode switch, function admin, dryrun, scaffold, reload)
stays superuser-only — this does not touch write access anywhere.

A superuser still passes every one of these unconditionally, exactly as
before. Additionally, ANY authenticated record — from any auth collection,
not only the `roles` one below — whose own `capabilities` JSON field
contains the route's capability string also passes:

| route | capability string |
| --- | --- |
| `GET /api/cqrs/events` | `ops.events.read` |
| `GET /api/cqrs/streams` | `ops.streams.read` |
| `GET /api/cqrs/deadletters` | `ops.deadletters.read` |
| `GET /api/cqrs/admin/mode` | `ops.mode.read` |
| `GET /api/cqrs/catalog` | `ops.catalog.read` |

`pocketcqrs` provisions a `roles` auth collection for this (every login
method enabled — a role record is a real person signing in, unlike
`pocketcqrs-extensions`' `service_accounts`) with one `capabilities: json`
field: an array of capability strings. Every collection rule is nil
(superuser-only management, same sensitivity as editing superusers), so
create a role and grant it capabilities through PocketBase's own admin UI
(`/_/`) — there is no separate dashboard for this. `["ops.events.read",
"ops.streams.read", "ops.deadletters.read", "ops.mode.read",
"ops.catalog.read"]` reproduces the full "poweruser" observability tier the
decision doc originally asked for; a subset is just as valid (e.g.
`["ops.catalog.read"]` alone for a role that should only ever see the
platform catalog).

One capability string per route, deliberately, not one shared grant — a
future role can be scoped to a subset with no schema or gate change, the
actual point of building a general per-capability model (the accepted
`pocketcqrs-futures` decision) instead of a single fixed tier. The gate is
collection-agnostic (`authverify.RequireCapability`): it does not know
about the `roles` collection by name, only about a `capabilities` field on
whichever record authenticated — so a future role/permission field on a
different collection (e.g. an end-user `users` collection) composes with
zero gate changes.

Multi-node: `RequireCapability` mirrors the superuser gate's remote-verify
behavior exactly (`v == nil` uses the request's already-loaded auth record;
with a `--cqrsVerifyAuth` `Verifier`, it re-verifies fresh against the
master on every request, no cache). The `pocketcqrs-dashboard` UI itself is
not capability-aware yet — a role session can reach these five routes
directly, but the dashboard's own nav/panels are not filtered per role
(tracked as follow-up work, not part of this build).

### The `users` collection (Item 12, end-user identity)

`pocketcqrs` also always provisions a plain, ordinary `users` auth
collection — deliberately unrelated to everything above. `roles` and
`_superusers` gate operator/ops-dashboard access; `service_accounts`
(`pocketcqrs-extensions`) is for non-human integration credentials. `users`
is neither: a blank slate for whatever end-user identity your own app
needs, with no `capabilities` field and no special access of any kind — a
`users` record can never satisfy `RequireCapability` or `RequireSuperuser`,
no matter what fields your app adds to it later.

Out of the box it ships with:

- Password auth on, everything else PocketBase's own defaults.
- `CreateRule ""` — open self-registration, the common baseline for a
  collection literally named `users`.
- `ListRule`/`ViewRule`/`UpdateRule`/`DeleteRule` `"@request.auth.id = id"`
  — a signed-in user sees and manages only their own record.

All four of those are ordinary collection settings, changeable at any time
through PocketBase's own admin UI (`/_/`) or your own migration — lock down
self-registration, add OAuth2 providers, whatever your app needs. **Used at
the app level, by design**: core's own migration
(`users.RegisterCollection`) is create-only — it never touches an
already-existing `users` collection — so your app's own migration can
safely fetch it (`app.FindCollectionByNameOrId("users")`) and add fields on
top, the same additive pattern `//@schema` reconcile already uses, without
fighting a later pocketcqrs boot reasserting a narrower shape.

Not built yet: any actual sign-in method beyond PocketBase's own default
password auth (Microsoft/Entra ID in particular — the OAuth2 provider
config is confirmed config-only, but the PKCE login/callback route pair and
session-cookie handling are still open work, tracked in this worktree's
`NEEDS.md`/`FAULTS-AND-WORK.md` as Item 12's remainder).

## projection

```sh
pocketcqrs projection rebuild <name>
```

Offline (stop `serve` first). Wipes the projection's collections, resets its
durable checkpoint and replays the whole event log through the current code.
Works for JS projections and, under `--tutorial`, the example Go projections
(`tasks`, `orders`) alike.

## deadletter

Failed **effect-function** deliveries are captured with the full event
envelope, error, attempt count and timestamps (the checkpoint still
advances — poison never blocks the log).

```sh
pocketcqrs deadletter list [--all]   # pending (+ resolved with --all)
pocketcqrs deadletter retry <id|all> # re-deliver through the CURRENT code
pocketcqrs deadletter dismiss <id>   # mark resolved without retrying
```

Retry semantics: a successful retry resolves the letter; a failing one
increments the attempt count and records the error. Projections do not
dead-letter — they block at the failing event by design.

The same three actions are on the HTTP API
([`POST /api/cqrs/deadletters/{id}/retry|dismiss`](gateway.md#operations)) and
on the ops dashboard's Consumers page. All of them share one implementation,
so they adjudicate a retry identically — the CLI is for an operator at a
shell, the API for one at a browser.

## dryrun

Runs candidate JS code against real history without appending events or
touching collections. The general workflow: extract fixtures, change code,
dry-run, deploy.

```sh
pocketcqrs dryrun extract <aggregate> <id> [--file out.json]
pocketcqrs dryrun decider <file.js> [aggregate-id]
pocketcqrs dryrun decide <file.js> <aggregate-id> <command> [payload-json]
pocketcqrs dryrun projection <file.js> [--diff]
```

- **extract** — dump a stream as a JSON fixture (the harness's "prior
  events").
- **decider** — fold existing streams of the decider's aggregate through the
  candidate code; prints stream/event counts, and the final state for a
  single id. Evolve errors and undeclared `//@handles` coverage fail the run.
- **decide** — fold one stream, then show the events a command WOULD
  produce (nothing is appended). **A domain rejection is a verdict, not a
  command failure**: the CLI exits `0` and prints "The decider REFUSED this
  command... This is a domain answer, not a failure." — the same
  `{ok:false, rejected:true}` shape the HTTP dry-run returns as a `200`.
- **projection** — simulate over the whole log in memory (upsert/delete
  counts, final rows per collection). `--diff` compares the simulation
  against each live collection and prints per-collection differences.
  Note: projections that read the live read side via `pb.query`
  (read-modify-write style) are not `--diff`-clean — the simulation reads
  the already-projected state. Recompute-style projections diff cleanly.

The same checks run over HTTP at
[`POST /api/cqrs/admin/dryrun`](gateway.md#dry-run) (source in the body
rather than a path, so the ops dashboard's editor can dry-run code that has
never been written to disk), and from the dashboard's Functions page. One
difference worth knowing: the HTTP `decider` mode also applies the gate a
reload applies — the contract probe and `//@handles` coverage — so it answers
"would a reload accept this?", which folding alone cannot for an aggregate
with no history yet.

## system

```sh
pocketcqrs system maintenance status   # print the current mode
pocketcqrs system maintenance on       # reject domain commands (503)
pocketcqrs system maintenance off      # resume serving
```

The mode is persisted in `events.db` and read per request, so these commands
work from a separate process while `serve` is running and take effect
immediately. Maintenance is the barrier for reloading schema-bearing
function files — see the [reload endpoint](gateway.md#admin).

## catalog

```sh
pocketcqrs catalog                     # Markdown + Mermaid to stdout
pocketcqrs catalog --json              # the raw catalog document
pocketcqrs catalog --skeletons docs/domains [--force]
```

Introspects the platform: registered aggregates (Go and JS, with declared
handles/transforms), empirical event types and version ranges from the log,
consumers (kind, triggers, owned collections, checkpoints), guarded
collections, HTTP/cron functions, and reactor flows derived from causation
metadata. The Markdown rendering includes a Mermaid flowchart
(aggregates → consumers → collections, plus empirical reactor edges).

`--skeletons` writes one domain-doc skeleton per aggregate (the
[convention template](../domains/README.md)) — events, flows and
implementation prefilled, commands left as TODO rows. Existing files are
skipped unless `--force`.

## pack

```sh
pocketcqrs pack export <outdir> --name <name> [--version v] [--description d]
    [--functions a.js,b.js] [--collections c1,c2]
pocketcqrs pack import <packdir> [--force]
```

Export bundles function files (default: all `.js` in `--functionsDir`) plus
optionally plain (non-projection-owned) collection schemas into a domain
pack directory (`manifest.json`, `pb_functions/`, `collections.json`).
Projection-owned collections are refused on export — they're recreated from
`//@schema` on import. Both directions load-validate the function files
first.

Import copies the function files into `--functionsDir` (skipping existing
unless `--force`) and applies `collections.json` via PocketBase's native
collection import. Activate with a restart or a maintenance-mode reload.
See [domain packs](../packs.md).

## skill

Agent skills for [Claude Code](https://claude.com/claude-code), carried inside the binary.

```sh
pocketcqrs skill list                       # what this binary carries
pocketcqrs skill install                    # into ~/.claude/skills (every project)
pocketcqrs skill install --dir .claude/skills   # into this project only
pocketcqrs skill install --force            # overwrite files you have edited
```

The skills are also in the repo at `.claude/skills/`, which Claude Code finds on its own
if you cloned it. Install them when you did **not** clone — when you ran `go install` and
write your functions in a directory of your own, which is the ordinary way to use
PocketCQRS and the case the in-repo copy cannot reach.

Existing files are left alone unless `--force`, so an edited skill is never overwritten
silently. This is the one command that does **not** bootstrap the app: it copies files and
touches nothing else, so it will not create a `pb_data/` in the directory you happen to be
standing in.

## superuser (PocketBase)

```sh
pocketcqrs superuser upsert <email> <password>
```

Create/update a superuser (admin UI + reload endpoint auth).

## schema

```sh
pocketcqrs schema import <document.json|manifest-dir> [--out dir] [--docs dir]
    [--aggregate <elementId>=<name>]... [--lang js|go] [--force]
pocketcqrs schema export <document.json>
```

Import and export [EventModeling](https://eventmodeling.org) documents —
see [the guide](../schema.md) for the mapping and what survives a round trip.

`import` accepts a single document or a split manifest directory. Without
`--out` it maps and reports without writing anything, which is the quickest
way to see what a document would produce. `--docs` writes per-aggregate
domain docs carrying the methodology prose (`reason`, `question`,
descriptions, hotspots) that has no home in code — ignored with `--lang go`.
Existing files are skipped unless `--force`, matching `catalog --skeletons`,
so a re-import cannot overwrite prose someone has edited.

`--aggregate` supplies the aggregate for an element the document leaves
untagged. Import **refuses** rather than guessing: the write side is
organised by aggregate, and deriving one from the swimlane would silently
merge unrelated stream families.

`--lang` picks the output language: `js` (the default, unchanged) or `go` —
a compiled decider/projection/reactor per [the Go guide](../go-guide.md)'s
JS→Go table, printing suggested (not applied) registration lines for
`main.go`. **Scaffolding, not migration**: there is no JS→Go transpiler,
`go` output is exactly as much a starting point as `js` output is — see the
Go guide for what "converting a domain" actually means per tier. Passing
`--docs` alongside `--lang go` prints a warning that it was ignored, rather
than writing nothing silently. Scenario checks (unless `--skip-scenarios`)
always run against the JS mapping of the model — there is no Go-specific
scenario checker — and the completion text says so for `--lang go` rather
than implying the generated Go files themselves were exercised.

Nothing imported is live. Save the generated files through the editor or copy
them into `--functionsDir`, then reload — schema-bearing files behind the
maintenance barrier.

`export` reconstructs a document from the running catalog, synthesizing the
required-but-absent design-time pieces (one swimlane, a screen per
screen-bearing slice, `status: informational`) and reporting everything it
invented or could not carry.
