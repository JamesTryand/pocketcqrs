# Getting started

PocketCQRS is a CQRS + functions-as-a-service backend built on PocketBase as a
Go dependency. The event log is the source of truth; PocketBase collections
are the query side; JavaScript functions extend both sides at runtime.

## Run it

Requires Go 1.25+.

**PocketCQRS ships empty.** `go run . serve` gives you the gateway, the
function runtime and an empty event log — no aggregates, no collections,
nothing you did not write. That is the shape you want for real work, and
what you get from `go install`.

This walkthrough needs examples to walk through, so opt into both halves of
them — the Go domains with a flag, the JS functions with a copy:

```sh
cp examples/pb_functions/*.js pb_functions/
go run . serve --tutorial
```

`--tutorial` registers this repo's example `task` and `order` aggregates,
their projections and their collections. The copy puts the example function
files where `--functionsDir` looks (it defaults to `pb_functions/`, which is
*yours* — see [`examples/pb_functions/`](../examples/pb_functions/README.md)
for what each file is and which of them need the flag).

That starts PocketBase on `http://127.0.0.1:8090` (admin UI at `/_/`). Data
lives in `pb_data/` — `data.db` (PocketBase), `events.db` (the event log),
`logs.db`.

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

(For local dev you can skip auth entirely:
`go run . serve --tutorial --cqrsAllowAnonymous`.)

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
comment lines declare what a file is. The examples you copied in
(their sources live in [`examples/pb_functions/`](../examples/pb_functions/README.md)):

| file | role |
| --- | --- |
| `task_audit.js` | effect: logs every task event (durable, at-least-once) |
| `hello.js` | HTTP function at `GET/POST /api/fn/hello` |
| `heartbeat.js` | cron function, every minute |
| `note.js` | full write side of a `note` aggregate (JS decider) |
| `notes.js` | read side of `note` (JS projection, creates its own collection) |
| `orders_by_customer.js` | JS projection rolling up the Go-maintained `orders` |
| `task_completion_note.js` | reactor: on `TaskCompleted`, dispatches `CreateNote` |

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

## Your first reactor (a saga spanning two aggregates)

Complete the task created in the first command above — a Go decider — and
watch a JS reactor create a note about it on a JS decider, with no code of
yours in between:

```sh
curl -X POST http://127.0.0.1:8090/api/cqrs/task/t1/CompleteTask \
  -H "Authorization: Bearer <token>"

curl http://127.0.0.1:8090/api/collections/notes/records
# -> now also: { noteId: "completed-t1", text: "task t1 was completed", ... }
```

`task_completion_note.js` reacted to `TaskCompleted` by dispatching
`CreateNote` **through the decider registry**, not by appending directly —
so the reaction is a decision that could have been refused, and a replay
lands on the same `completed-t1` id instead of creating a duplicate. See the
[JS guide's reactor section](js-guide.md#reactors-tier-4).

This is the one step that needs *both* halves of the opt-in at once: the Go
`task` aggregate from `--tutorial`, and three copied files
(`task_completion_note.js` plus `note.js`/`notes.js` for the aggregate it
dispatches into). Miss either and the note never appears.

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

- [Tutorial](tutorial.md) — go from a design document to a running slice
  with `pocketcqrs schema import`, instead of hand-writing files
- [JS guide](js-guide.md) — directives, tiers, bindings, determinism, workflows
- [Go guide](go-guide.md) — deciders, projections, reactors in Go
- [Reference](reference/directives.md) — directive / [CLI](reference/cli.md) / [gateway](reference/gateway.md)
- [Domain docs](domains/README.md) — how this repo documents its own domains
