---
name: pocketcqrs-domain
description: "Build and change domains in a PocketCQRS project: JS deciders, projections, reactors and effect functions, plus Go aggregates. Use when writing or editing files in pb_functions/, working with //@trigger or //@schema directives, adding an aggregate/command/event/read model, hot-reloading functions, dry-running candidate code, importing an event-model schema, or debugging why a function did not load, a projection wrote nothing, or a reload was refused."
---

# Building a domain with PocketCQRS

PocketCQRS is event sourcing + CQRS on PocketBase. Commands go to a **decider**, which
returns **events**; events are the only source of truth; **projections** fold them into
read-only collections; **reactors** turn them into further commands. Domains are written in
hot-reloadable JS (`pb_functions/*.js`) or compiled Go.

This skill is the workflow and the traps. The reference material lives in the repo and is
authoritative — read it rather than trusting a summary here:

| you need | read |
| --- | --- |
| tiers, bindings, worked examples | `docs/js-guide.md` |
| every directive, exactly | `docs/reference/directives.md` |
| Go aggregates, and JS→Go graduation | `docs/go-guide.md` |
| commands, flags | `docs/reference/cli.md` |
| HTTP surface, dry-run modes | `docs/reference/gateway.md` |
| empty dir → running slice | `docs/tutorial.md`, `docs/getting-started.md` |
| a design document → generated code | `docs/schema.md` |

## The invariant

**A decider must never reach outside itself.** No `pb`, no `Date`, no `Math.random`, no
network. It sees only the command and the folded state, and returns events. This is what makes
replay reproducible, and everything else rests on it. The runtime enforces it — a decider VM
has those bindings removed — so if a design seems to need decide-time external state, the
design is wrong, not the restriction.

The same rule in one line: **state changes become events by going through a command.** There
is deliberately no write binding in any tier. Projection-owned collections reject direct
writes (`403`, naming the command API).

## The four tiers

| tier | trigger | writes | may call out |
| --- | --- | --- | --- |
| 1 effect | `event`, `http`, `cron` | nothing | `$http` on `event`/`cron` only |
| 2 projection | `projection` | its own collections, via returned row ops | no |
| 3 decider | `decider` | nothing (returns events) | **never** |
| 4 reactor | `reactor` | nothing (returns commands) | `$http` |

## The loop

1. **Design the slice**: what command, what events, what read model answers what question.
2. **Write the file(s)** into `--functionsDir` (default `pb_functions/`). One purpose per file.
3. **Dry-run before activating** — `pocketcqrs dryrun decider|decide|projection <file>`, or
   `POST /api/cqrs/admin/dryrun`, or the dashboard's Functions page. This runs candidate code
   against real history without appending anything.
4. **Reload.** Schema-bearing files (anything with `//@schema`) need the maintenance barrier:
   ```sh
   pocketcqrs system maintenance on
   curl -X POST .../api/cqrs/admin/reload -H "Authorization: Bearer $TOKEN"
   pocketcqrs system maintenance off
   ```
   Effects, HTTP, cron and reactors reload in **running** mode — no barrier for an ordinary
   saga or effect edit.
5. **Read the reload report.** It names what reloaded and what was *refused*, per tier. A
   refusal here is the system telling you something; do not skip past it.
6. **Verify with real traffic**, not by reading. Send the command, check the events, check the
   read model.

## Traps that have actually cost time

Each of these produced a real defect in this project.

- **A projection returning plain objects writes nothing, forever.** `project()` must return
  **row ops** — `{ upsert: { key, fields } }` or `{ delete: <keyval> }` — not the row. Anything
  else is counted and logged as discarded, but the collection just stays empty. If a projection
  "runs fine" and the table is empty, this is why.
- **One purpose per file.** Projection, decider and reactor files must be single-purpose; a
  file mixing tiers is **refused at load** with `must be single-purpose`. (It used to load
  happily and silently discard the extra trigger — the refusal exists because that left a cron
  job that simply never ran.) Split them.
- **Reactor target ids must derive from the source event.** Delivery is at-least-once, so a
  redelivery must dispatch the *same* command to the *same* id and be rejected as a duplicate
  ("already exists"). `id: 'fulfil-' + event.aggregateId`, never a random or time-based id.
- **`//@trigger reactor`, not `react`.** And the function is `reactTo(event)`.
- **A JS decider cannot take a name a Go decider already has.** The reload refuses it and says
  so. Under `--tutorial`, `task` and `order` are taken.
- **Events are append-only and additive.** Add properties, never remove or retype. A shape
  change is a *new event type* plus an upcast transform. Collections are create/extend-only
  too: never drop, retype or rename.
- **`//@handles` must cover what the decider actually folds**, or the dry run and reload fail
  the decider. That is the gate working.
- **Nothing is registered by default.** PocketCQRS ships empty; `--tutorial` opts into the
  example `task`/`order` domains and their collections. Do not assume they exist.

## Calling a third party

`$http` reaches **event and cron functions, and reactors** — never deciders or projections, and
not `//@trigger http` functions (request-driven concurrency is unbounded). It is off unless the
server was started with `--cqrsAllowOutboundHTTP`, and every destination must be named with a
repeatable `--cqrsOutboundHost`. An empty allow-list permits nothing.

One attempt, no retry: an uncaught failure dead-letters and the checkpoint advances. The 3s
call deadline is **per call, not per function** — two sequential slow calls exhaust the 5s
function budget. Full detail in `docs/js-guide.md`.

## When something does not work

- **File ignored entirely** → directives must be in the **leading comment lines**; anything
  after the first non-comment line is not parsed. A file with no directives is skipped (logged).
- **Reload refused the decider** → read the report's reason: a name collision with a built-in,
  or `//@handles` not covering what it folds, or a failed contract probe.
- **Collection missing** → `//@schema` only materialises through a reload **in maintenance
  mode**. In running mode the schema tier is skipped, and the report says
  `"skipped: not in maintenance"`.
- **Effect function failed** → it dead-lettered rather than blocking the log.
  `pocketcqrs deadletter list`, fix the code, `pocketcqrs deadletter retry all` re-delivers
  through the *current* code.
- **A projection is stuck** → projections do **not** dead-letter; they block at the failing
  event deliberately. Fix the code and `pocketcqrs projection rebuild <name>`.

## Verifying your work

Do not report a domain as working because the code looks right. Send the command, assert the
events came back, and assert the read model reached its **final** state — a row that merely
exists may be half-projected if two events feed it. The project's own smoke suite
(`go test -tags=smoke ./smoke/`) and browser probes (`pocketcqrs-dashboard/probe/`) are worked
examples of doing this properly.
