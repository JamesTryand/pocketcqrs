# Browser probes

Six headless-browser checks for the things HTTP assertions cannot see. Each
one exists because it caught a real defect that every other gate passed:

| probe | what it proves | the bug that earned it |
| --- | --- | --- |
| `components.mjs` | every `<wa-*>` element actually upgraded (`shadowRoot`), and the event-modeling post-it colours hold in dark mode | the whole vendored tree was the wrong build, so **no** component ever defined — over HTTP the page looked perfect |
| `live.mjs` | htmx polls fire, swapped-in rows are re-processed, a `<wa-tag>` arriving in a swap upgrades, and an out-of-band rider updates a figure outside the swapped table | polling that stops after one tick, and swapped rows rendering as undefined elements — both silent |
| `system.mjs` | the System page boots, the reload indicator is hidden while idle, and the report swaps in place | — |
| `editor.mjs` | CodeMirror attaches, **writes through to the `<textarea>` htmx submits**, re-attaches after a swap, and the typed source is what reaches the backend | a save that landed and was then overwritten by a second request the same click fired; every endpoint's own test passed |
| `scaffold.mjs` | the wizard's components boot, and filling it in a browser really generates a slice | Web Awesome inputs are form-associated custom elements: their values reach a plain form submit, which no server-side test can confirm |
| `reactor.mjs` | the `mode=reactor` dry run renders its **dispatches table**, and the scaffolder's **warnings callout** appears for an unfinished slice | a populated `dispatches` array and a page rendering an empty panel are indistinguishable to every backend test; and the callout is the only thing saying a generated slice that *runs* is unfinished |

They complement `go test -tags=smoke ./smoke/`, which covers everything
reachable over HTTP. Anything involving a shadow DOM, a canvas, a timer or a
synthesised DOM event needs a browser, and belongs here.

## Running them

Start an instance for the probes to drive. From the repo root:

```sh
go build -o /tmp/pocketcqrs . && go build -o /tmp/pocketcqrs-dashboard ./pocketcqrs-dashboard
/tmp/pocketcqrs superuser upsert smoketest@example.com smoke-pass-1234 --dir /tmp/probe-data
/tmp/pocketcqrs serve --tutorial --http 127.0.0.1:8390 --dir /tmp/probe-data --functionsDir pb_functions &
/tmp/pocketcqrs-dashboard --backend http://127.0.0.1:8390 --listen 127.0.0.1:8391 &
```

`--tutorial` is required: `components.mjs`, `live.mjs` and `reactor.mjs` all
assert against the example `task` aggregate, and without the flag the seeding
commands below 404 silently — which shows up as "every element is missing"
rather than "the aggregate is not registered".

Give it some history to show — the probes assert against real data, and an
empty log exercises none of the interesting UI:

```sh
TOKEN=$(curl -s -X POST http://127.0.0.1:8390/api/collections/_superusers/auth-with-password \
  -H 'Content-Type: application/json' \
  -d '{"identity":"smoketest@example.com","password":"smoke-pass-1234"}' | jq -r .token)
curl -s -X POST http://127.0.0.1:8390/api/cqrs/task/t1/CreateTask \
  -H "Authorization: $TOKEN" -H 'Content-Type: application/json' -d '{"title":"first"}'
curl -s -X POST http://127.0.0.1:8390/api/cqrs/task/t1/CompleteTask \
  -H "Authorization: $TOKEN" -H 'Content-Type: application/json' -d '{}'
```

Then:

```sh
cd pocketcqrs-dashboard/probe
npm install          # puppeteer-core only; it drives a browser you already have
node components.mjs
node live.mjs
node system.mjs
node editor.mjs
```

Each exits non-zero on the first failure and prints one `ok`/`FAIL` line per
check, plus any page error or console warning — a probe that passes but logs
a console error still fails, because that is usually a component failing
quietly.

The probes bring their own fixtures: `live.mjs` installs a poisoned effect
function through the admin API so a dead letter appears while the page sits
open (that is how it proves the out-of-band swap end to end), and
`editor.mjs` writes the file it then edits. They assert that counts **move**
rather than that they start at zero, so an instance you have already been
using is fine — a probe that only passes against a hand-prepared instance is
a probe nobody else can run.

## Configuration

All four read the environment, so they can point at any instance:

| variable | default |
| --- | --- |
| `PROBE_BACKEND` | `http://127.0.0.1:8390` |
| `PROBE_DASHBOARD` | `http://127.0.0.1:8391` |
| `PROBE_BROWSER` | the system Edge on Windows |
| `PROBE_USER` / `PROBE_PASS` | `smoketest@example.com` / `smoke-pass-1234` |

`PROBE_BROWSER` takes any Chromium binary (Chrome, Chromium, Edge), which is
why the dependency is `puppeteer-core` rather than `puppeteer`: no browser is
downloaded.

## Why these are not in CI

They need a real browser and a running pair of servers. The Go smoke suite
covers the same flows at the HTTP level and does run in CI; the probes are
the layer above it, run before shipping UI work. If CI ever grows a browser
image, `PROBE_BROWSER` is the only thing that needs setting.

**Authenticate by planting the cookie, never by driving the login form.** A
form-driven probe that mistypes silently stays on the sign-in page and then
reports that every single component failed — which costs an hour before you
notice the page it is checking is the wrong one.
