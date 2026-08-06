# Task

A minimal work item with a lifecycle: created, then completed. The smallest
Go-defined vertical in the platform — one decider, one projection, one
audit effect function.

## Commands

| command | payload | intent | rejects when |
| --- | --- | --- | --- |
| `CreateTask` | `{ title: string }` | open a task | task already exists; `title` empty |
| `CompleteTask` | `{}` | mark it done | task does not exist; already completed |

## Events

| event | data | since version | notes |
| --- | --- | --- | --- |
| `TaskCreated` | `{ title: string }` | v1 | |
| `TaskCompleted` | `{}` | v1 | |

## Read models

| collection | owner | shape | notes |
| --- | --- | --- | --- |
| `tasks` | Go projection `tasks` (`projections/projections.go`) | `taskId` (unique), `title`, `completed` | keyed by `taskId` |

## Flows (reactors/sagas)

- The fulfillment saga creates tasks: on `OrderConfirmed` → `CreateTask` on
  `task/fulfill-<orderId>` (see [order](order.md)).
- `TaskCompleted` has a downstream consumer: the JS reactor
  `pb_functions/task_completion_note.js` dispatches `CreateNote` on
  `note/completed-<taskId>` (see [note](note.md)) — an example wiring one
  Go-defined and one JS-defined aggregate through the reactor tier.

## Scenarios

- given nothing → `CreateTask {title:"x"}` → `TaskCreated {title:"x"}`
- given `TaskCreated` → `CreateTask` → rejected ("task already exists")
- given `TaskCreated` → `CompleteTask` → `TaskCompleted`
- given `TaskCreated`, `TaskCompleted` → `CompleteTask` → rejected ("task already completed")

## Implementation

- decider: `aggregates/task.go` (registered in `aggregates.RegisterAll`)
- projection: `projections/projections.go` (`NewTasks`)
- collection migration: `migrations/1754200000_tasks_collection.go`
- effect function: `pb_functions/task_audit.js` (logs every task event) —
  ships in `examples/pb_functions/`, copy it in to run it
- reactor: `pb_functions/task_completion_note.js` — same, and it also needs
  `note.js`/`notes.js` for the aggregate it dispatches into
