# PocketCQRS

A CQRS + functions-as-a-service backend built on [PocketBase](https://pocketbase.io) as a Go dependency.

- **Write side**: commands arrive at a gateway route, are handled by Go [Deciders](https://thinkbeforecoding.com/post/2021/12/17/functional-event-sourcing-decider) (`InitialState` / `Decide` / `Evolve`), and commit as events to an append-only log — the source of truth.
- **Event store**: separate SQLite file `events.db` alongside PocketBase's `data.db`, with per-aggregate optimistic concurrency and a global position for catch-up.
- **Read side**: projections fold events into ordinary PocketBase collections, so the stock REST API, realtime subscriptions, auth rules and admin UI serve queries unchanged. Rebuild a projection offline with `pocketcqrs projection rebuild <name>`.
- **Write-guard**: direct record writes to guarded collections are rejected for everyone (superusers included); the only writer is the projection engine.
- **Sagas**: reactors are durable consumers that map committed events to follow-up commands, dispatched back through the registry with causation/correlation metadata — reactions become events like everything else.
- **Functions (FaaS)**: user-defined JS functions from `pb_functions/` (`//@trigger event ...` / `//@trigger http`), delivered durably (checkpointed, at-least-once), with read-only query-side bindings. Commands and HTTP functions require PocketBase auth by default (`--cqrsAllowAnonymous` for dev).

## Status

Early development — see milestones in the issue worktree (`task_plan.md`). Not affiliated with PocketBase; upstream (`pocketbase/pocketbase`) is an unmodified dependency pinned in `go.mod`.

## Development

Requires Go 1.25+.

```sh
go run . serve
```

Then open the PocketBase admin UI at `http://127.0.0.1:8090/_/`.

## License

MIT (see `LICENSE`). PocketBase itself is MIT-licensed, (c) Gani Georgiev.
