# Domain docs

One Markdown file per aggregate: `docs/domains/<aggregate>.md`. These are
**domain documentation**, not API docs — they describe intent (what the
aggregate is for, what its commands mean, what its events record), with the
mechanics (payload shapes, collections, file locations) kept accurate
against the code.

## Convention

- One file per aggregate, named after the aggregate (`task.md`, `note.md`).
- Sections in the template order below; drop sections that don't apply
  rather than leaving stubs.
- Keep shapes in sync with code: command payloads, event data, read-model
  fields. When behavior changes, the domain doc changes in the same commit.
- Scenarios are written *given/when/then* style: prior events, command,
  expected outcome (events or rejection). They mirror what
  `pocketcqrs dryrun decide` would show.
- Link function files for JS-defined parts, packages for Go parts.

## Template

```markdown
# <Aggregate>

<one paragraph: the business capability this aggregate owns>

## Commands

| command | payload | intent | rejects when |
| --- | --- | --- | --- |
| `Create<Thing>` | `{ name: string }` | creates the thing | already exists; `name` empty |

## Events

| event | data | since version | notes |
| --- | --- | --- | --- |
| `<Thing>Created` | `{ name: string }` | v1 | |

## Read models

| collection | owner | shape | notes |
| --- | --- | --- | --- |
| `<things>` | projection `<things>` (`pb_functions/<things>.js`) | `thingId, name, ...` | keyed by `thingId` |

## Flows (reactors/sagas)

- on `<Event>` → `<Command>` on `<aggregate>/<id rule>` (reactor `<name>`)

## Scenarios

- given nothing → `Create<Thing> {name:"x"}` → `<Thing>Created`
- given `<Thing>Created` → `Create<Thing>` → rejected ("already exists")

## Implementation

- decider: `aggregates/<thing>.go` or `pb_functions/<thing>.js`
- projections: `projections/<thing>.go` or `pb_functions/<things>.js`
- collections migration: `migrations/...` (if Go-owned)
```

## Dogfooded examples

- [task](task.md) — smallest Go-defined vertical
- [order](order.md) — richer Go aggregate + saga + JS rollup
- [note](note.md) — fully JS-defined vertical
