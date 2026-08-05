# The ops dashboard

`pocketcqrs-dashboard` is a second binary that browses and operates a running
PocketCQRS instance. It consumes the **public HTTP API only** — it imports no
pocketcqrs package and keeps its own copy of the JSON shapes — so everything
it does, your own tooling can do. That is deliberate: the operational API is
the deliverable, and the dashboard is the proof that it is genuinely usable
from outside.

```sh
go install github.com/jamestryand/pocketcqrs/pocketcqrs-dashboard@latest
pocketcqrs-dashboard --backend http://127.0.0.1:8090 --listen 127.0.0.1:8091
```

Sign in with a PocketBase **superuser**. The token is held in an `HttpOnly`
cookie and used server-to-server; the browser never talks to the backend
directly, so there is no CORS to configure and no token in client JavaScript.
For deployment topologies (same-origin, reverse proxy, in-process) see
[consuming](consuming.md).

The binary is self-contained: Web Awesome, htmx, cytoscape and CodeMirror are
vendored and served from the executable. No CDN, no network access beyond
your backend.

## Overview

The mode banner, five totals, and a **catalog explorer** drawn in
event-modeling notation: events are orange post-it nodes, read models green
rectangles, aggregates neutral rectangles, consumers ellipses (dashed for
reactors), and dashed edges are reactor flows **observed in the log**, not
declared anywhere. Blue is reserved for commands (M13) and screens are not
modelled yet, which the legend says.

The banner and totals refresh themselves every couple of seconds. The graph
does not: laying it out again would be wasteful and would throw away whatever
node you had selected.

## Aggregates → streams → events

`Aggregates` lists what is registered, where each came from (Go or JS), and
the event types each has **actually emitted**, with counts — empirical, from
the log, not from a declaration. Drill into an aggregate for its streams, and
into a stream for its events as a vertical post-it timeline with payload and
metadata behind a disclosure.

## Events

The whole log with filters (aggregate, type, page size) and bidirectional
paging. Positions are `AUTOINCREMENT` and conflict retries burn values, so
never read a position as a count.

This page does **not** poll, unlike the consumer table. An unbounded page is
the *start* of the log, so a timer would re-fetch the same oldest rows
forever; `Newer` is the live control here.

## Consumers

Every checkpointed consumer — Go projections, JS projections, reactors and
effect functions alike — with its checkpoint and how far behind it is.
Behind-by is measured against the **head of the log**
(`totals.maxPosition − checkpoint`), never against an event count.

Below it, the dead-letter queue: failed effect-function deliveries captured
with their whole event envelope. The checkpoint still advances, so a poison
message never blocks the log.

- **Retry** re-delivers through the **current** function code. Fix the
  function on the Functions page, reload it, then retry.
- **Dismiss** resolves a letter without re-delivering it.
- A retry that fails again is a *result*, not an error: the row stays pending
  with another attempt recorded and the new failure text.

The checkpoint table is live; the dead-letter table is not, because its rows
hold disclosures you open while diagnosing and a refresh would close them.
The pending count in its header is live, so a new arrival still announces
itself.

## Functions

The function editor: the files in the backend's functions directory, what
each one declares (as the loader reads it, not guessed from the name), and an
editor over the selected file.

The workflow the page is built around:

1. **Edit** — CodeMirror over a plain `<textarea>`; with scripting off you
   get the textarea and no highlighting, and everything still works.
2. **Dry run** — folds the candidate over real history without appending
   events or touching collections. For deciders it applies the same gate a
   reload applies, so passing means *a reload would accept this*. Naming a
   command instead reports the events that command **would** append.
3. **Save** — writes the file. A save is refused outright if the source does
   not parse or compile: reloads are all-or-nothing, so an unloadable file
   would abort every later reload including the one that fixes it. The
   replaced version is kept, and **Load previous version** brings it back
   into the editor.
4. **Reload** — on the System page. Saving is not activating.

A file marked **unreadable** in the list is blocking every reload until it is
fixed or deleted; that is why it is listed rather than hidden.

## System

The mode barrier and hot reload, on one page because the barrier is an
ordering rule between them:

> maintenance on → reload → maintenance off

- **Effect, HTTP and cron** functions reload in any mode — they declare no
  schema.
- **JS projection schemas and JS deciders** reload only in maintenance.
  Reloading them while running is not refused, it is *skipped*, and the
  report says so.
- Deciders are re-validated against real streams on the way in; a refusal
  keeps the previous code serving.

The reload report is rendered rather than redirected away — it is the result
of the action, and a redirect would discard it.

While in maintenance the gateway rejects domain commands with `503`. The mode
is persisted in `events.db`, so a system left in maintenance stays there
across a restart.

## What it deliberately does not do

- **Edit records.** Projection-owned collections are write-guarded for
  everyone; corrections are commands. Plain collections stay in the
  PocketBase admin UI, which the header links to rather than duplicating.
- **Design collections.** Guarded collections derive from `//@schema`
  directives; the editor changes the directive, not the table.
- **Replace the PocketBase admin UI.** Different job, no fork.

## Checking it after a change

Two gates, both in the repo:

```sh
go test -tags=smoke ./smoke/          # the whole platform over HTTP, incl. the dashboard
cd pocketcqrs-dashboard/probe && npm install && node components.mjs
```

The [browser probes](../pocketcqrs-dashboard/probe/README.md) cover what HTTP
assertions cannot see: whether components actually upgraded, whether polling
fires, whether an editor writes through to the field that is submitted. Each
one exists because it caught a defect every other gate passed.
