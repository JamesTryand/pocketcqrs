# CLI reference

All commands are subcommands of the `pocketcqrs` binary (`go run . <cmd>`).
PocketBase's own commands (`serve`, `superuser`, `migrate`, ...) are
available too. Persistent PocketBase flags: `--dir` (data dir), `--http`
(listen address) for `serve`.

PocketCQRS flags (persistent, on every command):

| flag | default | meaning |
| --- | --- | --- |
| `--functionsDir` | `pb_functions` | directory with user-defined JS functions |
| `--cqrsAllowAnonymous` | `false` | dev: commands and `/api/fn` need no auth token (no actor metadata stamped) |
| `--cqrsStrictBoot` | `false` | abort startup when a JS decider fails validation (default: skip it and keep serving) |

## serve

```sh
pocketcqrs serve [--http 127.0.0.1:8090] [--dir pb_data]
```

Boots the platform and serves HTTP: PocketBase API + admin UI (`/_`), the
[gateway routes](gateway.md), the consumers engine, cron jobs. Bootstrap
applies app migrations, validates JS deciders, reconciles JS projection
schemas and starts catch-up from the event log.

## projection

```sh
pocketcqrs projection rebuild <name>
```

Offline (stop `serve` first). Wipes the projection's collections, resets its
durable checkpoint and replays the whole event log through the current code.
Works for Go projections (`tasks`, `orders`) and JS projections alike.

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
  produce (nothing is appended; domain rejections surface as errors).
- **projection** — simulate over the whole log in memory (upsert/delete
  counts, final rows per collection). `--diff` compares the simulation
  against each live collection and prints per-collection differences.
  Note: projections that read the live read side via `pb.query`
  (read-modify-write style) are not `--diff`-clean — the simulation reads
  the already-projected state. Recompute-style projections diff cleanly.

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

## superuser (PocketBase)

```sh
pocketcqrs superuser upsert <email> <password>
```

Create/update a superuser (admin UI + reload endpoint auth).
