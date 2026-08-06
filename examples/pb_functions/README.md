# Example function files

The JS half of this repo's examples. Nothing here is loaded automatically —
`--functionsDir` defaults to `pb_functions/`, which is **yours** and empty in
git. Copy in what you want to run:

```sh
cp examples/pb_functions/*.js pb_functions/
go run . serve --tutorial
```

The copy is deliberate rather than serving straight out of this directory.
The functions editor and the reload endpoint **write into** `functionsDir` —
a save keeps a `.prev` copy as its undo, and the browser probes install their
own fixtures there. Pointing that at tracked repo files would dirty the tree
on every save.

| file | tier | needs |
| --- | --- | --- |
| `task_audit.js` | effect (event) | `--tutorial` (the Go `task` aggregate) |
| `hello.js` | effect (http) | `--tutorial` (queries the `tasks` collection) |
| `heartbeat.js` | effect (cron) | `--tutorial` (queries `tasks`; also reads `notes`) |
| `note.js` | decider | nothing — a self-contained JS aggregate |
| `notes.js` | projection | `note.js` |
| `orders_by_customer.js` | projection | `--tutorial` (rolls up the Go-maintained `orders`) |
| `task_completion_note.js` | reactor | `--tutorial` **and** `note.js` — it spans both |

Only `note.js` + `notes.js` work without `--tutorial`: they are the fully
JS-defined vertical, with no Go code behind them. Everything else in this
table reaches for something the example Go domains own.

See [the JS guide](../../docs/js-guide.md) for what each tier means, and
[getting started](../../docs/getting-started.md) for the walkthrough these
files back.
