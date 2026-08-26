# Concepts: coming from CRUD

You know CRUD: a table has rows, a request updates a row in place, a query
reads the current row. PocketCQRS keeps that experience on the read side —
you still hit a REST endpoint and get back rows — but the write side works
differently, and that difference is the whole point of CQRS
(Command Query Responsibility Segregation) and event sourcing. This page is
the "why does it work like *that*" companion to
[Getting started](getting-started.md)'s "here's how to run it."

## The core shift: facts instead of current state

CRUD stores **current state** and overwrites it: `UPDATE tasks SET completed
= true WHERE id = 't1'`. The previous value is gone — the row only ever
tells you *what is true now*, never *what happened*.

Event sourcing stores **facts about what happened**, and derives current
state from them. Instead of overwriting a row, you append `TaskCreated`,
then later `TaskCompleted`, to a log for `t1`. Nothing is ever overwritten
or deleted. "Current state" becomes a read-time (or projection-time)
computation: fold every event for `t1` in order and see where you land.

That log — `events.db`, a SQLite file alongside PocketBase's own `data.db`
— is the **source of truth**. Every PocketBase collection you query is a
*derived*, disposable view of it. You could delete every collection and
rebuild them from the log; you could never do that with a CRUD table,
because the table *is* the truth.

## Commands and events are not the same thing

CRUD has one kind of write: a mutation. PocketCQRS splits writes into two
concepts that are easy to conflate at first:

| | is a request or a fact? | can it fail? | example |
| --- | --- | --- | --- |
| **Command** | a request — "please do this" | yes, can be refused | `CompleteTask` |
| **Event** | a fact — "this happened" | no, it already did | `TaskCompleted` |

A command arrives at the gateway (`POST /api/cqrs/task/t1/CompleteTask`).
It is *intent*, named as an imperative verb, and it can be rejected — the
task might already be complete, or the caller might not be allowed to
complete it. If it's accepted, it produces one or more events, named as
past-tense facts, which are what actually get appended to the log. The
distinction matters because only events are stored; a rejected command
leaves no trace, exactly like a CRUD `UPDATE` that fails validation and
never reaches the database.

## Deciders: the write side, and why they don't touch the database directly

In CRUD, request handling and persistence are one step: validate, then
`UPDATE`. In PocketCQRS the write side is a pure decision function — a
**decider** — with no database access at all. A decider is three functions:

- `InitialState` — what a brand-new stream looks like before any events
- `Decide(state, command) → events | rejection` — the business rule
- `Evolve(state, event) → state` — how one event changes the state

To handle a command, the runtime *replays* every existing event for that
aggregate through `Evolve` to reconstruct current state, then calls
`Decide` against that reconstructed state. If `Decide` accepts, the
resulting events are appended to the log — that append is the only
database write in the whole path. The decider itself never reads or writes
a row; it only ever sees state folded from events. This is what "the event
log is the source of truth" means concretely: the decider's whole world is
the log, replayed.

("Decider" is a named pattern, not a PocketCQRS invention — see
[the pattern write-up](https://thinkbeforecoding.com/post/2021/12/17/functional-event-sourcing-decider)
linked from the README.)

## Aggregates: the CRUD "table" is now a stream, scoped by ID

In CRUD, a row's identity is its primary key, and any row can be updated
independently of any other. PocketCQRS calls a decider's unit of
consistency an **aggregate** — e.g. `task`, `order` — and every command
targets one aggregate instance by ID (`task/t1`). All of `t1`'s events live
in one stream, appended with per-stream optimistic concurrency: two
concurrent commands against `t1` can conflict and one is refused, the same
guarantee an `UPDATE ... WHERE version = ?` gives you in CRUD. Two
different tasks, `t1` and `t2`, never contend with each other — same as two
unrelated rows.

## Projections: your familiar REST API, rebuilt from facts

This is the part that looks exactly like CRUD from the outside: a
**projection** folds events into an ordinary PocketBase collection, and you
query it with the stock REST API, realtime subscriptions, auth rules, the
admin UI — nothing about *reading* changes. `TaskCreated` then
`TaskCompleted` for `t1` becomes one row in `tasks` with `completed: true`,
same as CRUD would show you.

The differences only show up when you think about *how that row got
there*: a projection is disposable and reproducible. If you change how a
projection folds events, or discover a bug in it, you don't migrate the
row — you fix the code and run `pocketcqrs projection rebuild <name>`,
which replays the whole log from scratch and produces a correct collection.
A CRUD table has no equivalent operation, because it has no log behind it
to replay.

## The write-guard: why you can't just POST to a collection

Naturally, the next question is "what stops someone writing to `tasks`
directly, the CRUD way, and skipping all of this?" That's the
**write-guard**: any direct write to a projection-owned collection —
including from a superuser — is rejected with 403. The read-side API is
read-only, full stop. State changes are commands; commands become events;
events become read models. There is exactly one path in, and it goes
through a decider.

## Reactors: sagas, or "triggers that dispatch commands, not SQL"

CRUD sometimes reaches for a DB trigger to make one write cascade into
another. PocketCQRS's equivalent is a **reactor**: a durable consumer that
watches committed events and, on a match, dispatches a *follow-up command*
— back through the same decider registry a human caller would use, not by
appending events directly. `TaskCompleted` → reactor → `CreateNote`
command → decider decides → `NoteCreated` event. Because the reaction is a
real command, it can be refused, it's idempotent under replay (a reactor
that dispatches the same command with the same target ID twice doesn't
duplicate), and it shows up in the log with causation/correlation metadata
tying it back to the event that triggered it. This is the multi-aggregate
"saga" pattern CQRS literature talks about — one decider's fact triggering
another decider's decision.

## Eventual consistency, made concrete

In CRUD, a read after a write always sees the write — it's the same row.
Here, the write side (append to the log) and the read side (fold into a
collection) are two separate steps connected by a **consumer** that polls
the log and applies each new event to its projection. In this single-node
setup that catch-up is typically fast enough not to notice, but it is not
the same operation as the write, and nothing forces it to be instant —
that gap is what "eventually consistent" means. It's the same gap a
CRUD system gets from a read replica or a cache: the source of truth moved
first, the read view catches up. (Multi-node deployments make the gap more
visible: secondaries poll a replicated log and read models converge
slightly behind the master — see the [CLI reference](reference/cli.md#multi-node-single-writer-multiple-readers).)

## A CRUD → PocketCQRS glossary

| CRUD instinct | PocketCQRS equivalent |
| --- | --- |
| `UPDATE table SET ...` | append a command → decider decides → events appended |
| the row *is* the data | the row is a *projection* of the events; the log is the data |
| SELECT / REST GET | same — projections are ordinary PocketBase collections |
| primary key | aggregate ID (`task/t1`) — scopes one event stream |
| `WHERE version = ?` optimistic lock | per-stream optimistic concurrency on append |
| schema migration | projection rebuild (read side) — deciders don't have "schema" the way rows do |
| DB trigger cascading a second write | reactor dispatching a follow-up command |
| direct table write | rejected by the write-guard — there is no direct write |
| audit log bolted on afterward | not needed — the event log already *is* the full history |

## Where next

- [Getting started](getting-started.md) — run these ideas: your first
  command, your first projection, your first reactor
- [Tutorial](tutorial.md) — go from an EventModeling design document to a
  running slice
- [Go guide](go-guide.md) / [JS guide](js-guide.md) — write deciders,
  projections and reactors in either language
- [Domain docs](domains/README.md) — worked examples (`task`, `order`,
  `note`) with their full decider/projection/reactor code
