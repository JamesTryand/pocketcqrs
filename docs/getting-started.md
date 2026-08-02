# Getting started

PocketCQRS is a CQRS + functions-as-a-service backend built on PocketBase as a
Go dependency. The event log is the source of truth; PocketBase collections
are the query side; JavaScript functions extend both sides at runtime.

## Run it

Requires Go 1.25+.

```sh
go run . serve
```

This starts PocketBase on `http://127.0.0.1:8090` (admin UI at `/_/`) with
the CQRS gateway, the function runtime, and the example functions from
`pb_functions/`. Data lives in `pb_data/` — `data.db` (PocketBase),
`events.db` (the event log), `logs.db`.

Create a superuser (admin) account, needed for the admin UI and the
reload endpoint:

```sh
go run . superuser upsert admin@example.com very-secret-password
```

## Your first command (write side)

Commands go to the gateway; events come back. Authenticate first —
commands require a PocketBase auth token by default:

```sh
# 1. get a token
curl -X POST http://127.0.0.1:8090/api/collections/_superusers/auth-with-password \
  -H "Content-Type: application/json" \
  -d '{"identity":"admin@example.com","password":"very-secret-password"}'

# 2. send a command (use the token from step 1)
curl -X POST http://127.0.0.1:8090/api/cqrs/task/t1/CreateTask \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" \
  -d '{"title":"learn pocketcqrs"}'
```

Response: the appended events, e.g. one `TaskCreated` at position 1 with
`metadata.actor` / `actorCollection` / `now` stamped.

(For local dev you can skip auth entirely: `go run . serve --cqrsAllowAnonymous`.)

## Your first query (read side)

A projection has already folded that event into the `tasks` collection —
an ordinary PocketBase collection, served by the stock REST API:

```sh
curl http://127.0.0.1:8090/api/collections/tasks/records
# -> items: [{ taskId: "t1", title: "learn pocketcqrs", completed: false, ... }]
```

Try to write to that collection directly:

```sh
curl -X POST http://127.0.0.1:8090/api/collections/tasks/records \
  -H "Content-Type: application/json" -d '{"taskId":"hack"}'
# -> 403: state changes must go through the command API
```

That is the **write-guard**: projection-owned collections are read-only for
everyone, superusers included. State changes are commands; commands become
events; events become read models. No exceptions.

## Your first function (FaaS)

Files in `pb_functions/` are loaded at boot; directives in the leading
comment lines declare what a file is. The examples shipped:

| file | role |
| --- | --- |
| `task_audit.js` | effect: logs every task event (durable, at-least-once) |
| `hello.js` | HTTP function at `GET/POST /api/fn/hello` |
| `heartbeat.js` | cron function, every minute |
| `note.js` | full write side of a `note` aggregate (JS decider) |
| `notes.js` | read side of `note` (JS projection, creates its own collection) |
| `orders_by_customer.js` | JS projection rolling up the Go-maintained `orders` |

Try the note vertical — entirely user-defined, no Go code involved:

```sh
curl -X POST http://127.0.0.1:8090/api/cqrs/note/n1/CreateNote \
  -H "Authorization: Bearer <token>" \
  -H "Content-Type: application/json" -d '{"text":"hello notes"}'

curl http://127.0.0.1:8090/api/collections/notes/records
# -> items: [{ noteId: "n1", text: "hello notes", archived: false, ... }]
```

The `notes` collection was created by the `//@schema` directive in
`notes.js` at boot — no migration, no admin UI clicking.

## Change code without restarting

Edit `pb_functions/hello.js` (change the message), then:

```sh
curl -X POST http://127.0.0.1:8090/api/cqrs/admin/reload \
  -H "Authorization: Bearer <token>"
```

Effect-tier functions (event/http/cron) swap immediately. Schema-bearing
files (JS projections, JS deciders) only reload behind the maintenance
barrier:

```sh
go run . system maintenance on
# ... commands now return 503 ...
curl -X POST http://127.0.0.1:8090/api/cqrs/admin/reload -H "Authorization: Bearer <token>"
go run . system maintenance off
```

## Before you deploy a change: dry-run it

The dry-run harness runs candidate code against real history without
persisting anything:

```sh
go run . dryrun decider pb_functions/note.js          # fold all note streams
go run . dryrun decide pb_functions/note.js n1 ArchiveNote
go run . dryrun projection pb_functions/notes.js --diff
```

## Where next

- [JS guide](js-guide.md) — directives, tiers, bindings, determinism, workflows
- [Go guide](go-guide.md) — deciders, projections, reactors in Go
- [Reference](reference/directives.md) — directive / [CLI](reference/cli.md) / [gateway](reference/gateway.md)
- [Domain docs](domains/README.md) — how this repo documents its own domains
