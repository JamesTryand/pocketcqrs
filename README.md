# PocketCQRS

A CQRS + functions-as-a-service backend built on [PocketBase](https://pocketbase.io) as a Go dependency.

- **Write side**: commands arrive at a gateway route, are handled by Go [Deciders](https://thinkbeforecoding.com/post/2021/12/17/functional-event-sourcing-decider) (`InitialState` / `Decide` / `Evolve`), and commit as events to an append-only log — the source of truth.
- **Event store**: separate SQLite file `events.db` alongside PocketBase's `data.db`, with per-aggregate optimistic concurrency and a global position for catch-up.
- **Read side**: projections fold events into ordinary PocketBase collections, so the stock REST API, realtime subscriptions, auth rules and admin UI serve queries unchanged. Rebuild a projection offline with `pocketcqrs projection rebuild <name>`.
- **Write-guard**: direct record writes to guarded collections are rejected for everyone (superusers included); the only writer is the projection engine.
- **Sagas**: reactors are durable consumers that map committed events to follow-up commands, dispatched back through the registry with causation/correlation metadata — reactions become events like everything else.
- **Functions (FaaS)**: user-defined JS functions from `pb_functions/` — effects (`//@trigger event`), HTTP (`//@trigger http`), cron (`//@trigger cron`), projections (`//@trigger projection` + `//@schema`), and full deciders (`//@trigger decider`) — with durability, determinism tiers and read-only query-side bindings per role. Commands and HTTP functions require PocketBase auth by default (`--cqrsAllowAnonymous` for dev).

## Status

Early development — see milestones in the issue worktree (`task_plan.md`). Not affiliated with PocketBase; upstream (`pocketbase/pocketbase`) is an unmodified dependency pinned in `go.mod`.

## Docs

- [Getting started](docs/getting-started.md) — run it, first command, first function, hot reload
- [JS guide](docs/js-guide.md) — directives, tiers, bindings, determinism, dry-run/dead-letter workflows
- [Go guide](docs/go-guide.md) — deciders, projections, reactors in Go
- [Ops dashboard](docs/dashboard.md) — browse the log, operate the barrier, retry dead letters, edit functions
- [Consuming](docs/consuming.md) — deployment patterns for frontends, ops tooling and external read-model sinks (with Caddyfiles)
- Reference: [directives](docs/reference/directives.md) · [CLI](docs/reference/cli.md) · [gateway](docs/reference/gateway.md)
- [Domain docs](docs/domains/README.md) — convention + dogfooded [task](docs/domains/task.md), [order](docs/domains/order.md), [note](docs/domains/note.md)
- [Domain packs](docs/packs.md) — export/import domains, versioning contract, trust model
- [EventModeling import/export](docs/schema.md) — map an eventmodelschema document onto a slice, and back; what round-trips and what does not
- [Contributing](docs/contributing.md)

## Development

Requires Go 1.25+.

```sh
go run . serve
```

Then open the PocketBase admin UI at `http://127.0.0.1:8090/_/`.

## Install

Two binaries, two install paths (`go install <module>@latest` covers only the
root main package):

```sh
go install github.com/jamestryand/pocketcqrs@latest                    # the backend
go install github.com/jamestryand/pocketcqrs/pocketcqrs-dashboard@latest  # the ops dashboard
```

`@latest` resolves to the newest semver tag (see the repo's tags; pin an
exact version with `@vX.Y.Z` or a commit hash). The dashboard prints its
version with `pocketcqrs-dashboard --version`.

## License

MIT (see `LICENSE`). PocketBase itself is MIT-licensed, (c) Gani Georgiev.
