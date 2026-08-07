# Tutorial: from a design document to a running slice

[Getting started](getting-started.md) walks the platform with hand-written
files. This walks the other on-ramp: **`pocketcqrs schema import`** turns an
[EventModeling](https://eventmodeling.org) design document into working code.
Every command and every line of output below is real — run against the
document this project vendors for its own round-trip tests, not paraphrased.

You need a clone of this repo (the document lives in `testdata/`, not in the
installed binary) and a superuser, same as
[getting started](getting-started.md#run-it). Everything here runs in a
scratch data directory so it can't collide with an instance you already have.

## The document

`testdata/eventmodelschema/examples/order-fulfillment.json` is the schema
project's own worked example: an `order` aggregate placing and shipping
orders, a read model summarising order status, and two automations —
auto-shipping a placed order, and notifying a shipping partner once it
ships. Four slices, three slice patterns, on purpose: it is designed to
exercise the importer, not to be minimal.

```sh
mkdir tutorial && cp testdata/eventmodelschema/examples/order-fulfillment.json tutorial/
cd tutorial
go run .. schema import order-fulfillment.json --dev=false
```

(`go run ..` because `tutorial/` is one level under the module root; from the
repo root itself it's plain `go run . schema import ...`.)

## First run: refused, and told exactly why

```
REFUSED (1):
  ✗ command "notify-shipping-partner" has no `aggregate` tag and no override
    was supplied. This project's write side is organised by aggregate and an
    automation's result events are real log entries, so something must own
    the stream. Supply --aggregate notify-shipping-partner=<name>, or add the
    tag to the document; deriving one from the swimlane would silently merge
    unrelated stream families
```

`aggregate` is optional in the schema, and this document leaves that one
element untagged — a boundary-crossing automation, exactly the case
[the schema guide](schema.md#untagged-aggregates-are-refused-not-guessed)
says import refuses rather than guesses. Nothing was written. Supply the
missing piece:

```sh
go run .. schema import order-fulfillment.json --dev=false \
  --aggregate notify-shipping-partner=shipmentNotice \
  --out generated --docs domaindocs
```

## What the report says

```
Decisions taken on the document's behalf (10):
  ! command place-order.customerId is a uuid and becomes text (no uuid column type here)
  ! command place-order.items is a list of custom and becomes a json column
  ! event order-placed.orderId is a uuid and becomes text (no uuid column type here)
  ! event order-placed.items is a list of custom and becomes a json column
  ! event "order-shipped" declares no fields; generated as carrying no payload
  ! automation "auto-ship-slice" consults read model "pending-shipments"; the generated reactor does not read it — `pb.findRecord`/`pb.query` are available inside reactTo() if the rule needs it
  ! command "notify-shipping-partner" has no `aggregate` tag; using the supplied override "shipmentNotice"
  ! event "shipment-notified" declares no fields; generated as carrying no payload
  ! read model order-summary.orderId is a uuid and becomes text (no uuid column type here)
  ! read model "pending-shipments" marks no field with idAttribute; keying rows on "orderId" (the aggregate id)
Not carried across (6):
  – 1 field(s) are marked pii and NOTHING here carries that flag: event order-placed.customerEmail. They are stored as ordinary columns; treat them accordingly
  – 1 chapter(s) are design-time grouping with no runtime concept here
  – 1 actor lane(s) are design-time notation with no runtime concept here
  – 1 hotspot(s) are open questions about the model; they are carried into the domain doc, not into code
  – 2 screen(s) have no runtime concept here; an export synthesizes them back
  – slice status is board state and is not preserved (done×2, inProgress×1, planned×1)

Scenarios checked against the generated code: 3 passed, 1 failed, 0 skipped
  ✓ [stateChange] Placing an order records Order Placed
      (would append OrderPlaced — the example payload doesn't fully match;
      passing means the type is right, not that every field is)
  ✗ [stateView] Order summary reflects a placed order before shipment
      result: status missing
  ✓ [stateChange] A placed order automatically triggers shipment
  ✓ [stateChange] A warehouse shipment automatically notifies the shipping partner

Wrote 8 file(s) to generated
```

(abbreviated — the real "passed" lines each carry a longer note about which
example fields didn't line up; only the failure is load-bearing here)

Read every line — that is the point of the report, not a formality. Two
things worth slowing down on:

- **The PII line matters more than it looks.** `order-placed.customerEmail`
  is marked `pii` in the source document. This project has no runtime PII
  flag yet (`//@pii` is deliberately unbuilt — see `NEEDS.md`), so the
  generated `orderId`/`customerEmail`/`items` land in the log and in
  `//@schema` as ordinary fields. The report telling you this, by name, is
  the whole reason the report exists: a silently dropped protection is worse
  than an absent one nobody claimed.
- **The failing scenario is not a bug in the import.** `order-summary`
  expects a `status` field that nothing in the document ever assigns —
  the generated projection can't invent a rule the document doesn't state.
  This is what "run the scenarios, don't just read them" is for: it finds
  the one read-model field with no author yet, instead of you finding it
  the hard way after saving.

## What got generated

```sh
ls generated
# autoShipPendingOrders.js  notifyShippingPartner.js  order.js  orderSummary.js
# pendingShipments.js       shipmentNotice.js
ls domaindocs
# order.md  shipmentNotice.md
```

Open `generated/order.js`. It carries the same directives a hand-written
decider would — `//@commands`, `//@handles`, and (because it was generated,
not hand-written) `//@produces` on every command, so this slice round-trips
an `export` faithfully with no widening:

```js
//@trigger decider order
//@handles OrderPlaced OrderShipped
//@commands PlaceOrder ShipOrder
//@produces PlaceOrder OrderPlaced
//@produces ShipOrder OrderShipped
```

`domaindocs/shipmentNotice.md` carries what code has no home for: the
command's `reason` from the document, and its one open hotspot
(*"Unclear whether the Order Status screen should show 'processing' before
the shipment automation fires..."*) — a real open question the document's
author left, now sitting where the next person to touch this domain will
actually see it, instead of staying trapped in a design tool nobody
running the platform has open.

**Nothing here is live.** Generating and reporting never touch the running
platform — see each file in the [editor](dashboard.md#functions), dry-run
it, save it, then reload behind the barrier, exactly like hand-written code.

## The gotcha this walkthrough exists to show you

Point a real instance at the generated files and boot it — with
`--tutorial`, which registers this repo's own example domains. That flag is
not incidental here: it is what puts a **built-in `order` aggregate in the
way** of the one this document just generated, and that collision is the
thing worth seeing.

```sh
go run .. superuser upsert tutorial@example.com tutorial-pass-1234 --dir pb_data --dev=false
go run .. serve --tutorial --http 127.0.0.1:8398 --dir pb_data --functionsDir generated --dev=false &
```

Authenticate, enter maintenance, and reload — the step that activates
schema-bearing files:

```sh
TOKEN=$(curl -s -X POST http://127.0.0.1:8398/api/collections/_superusers/auth-with-password \
  -H 'Content-Type: application/json' \
  -d '{"identity":"tutorial@example.com","password":"tutorial-pass-1234"}' \
  | grep -o '"token":"[^"]*"' | cut -d'"' -f4)
curl -s -X POST http://127.0.0.1:8398/api/cqrs/admin/mode -H "Authorization: $TOKEN" \
  -H 'Content-Type: application/json' -d '{"mode":"maintenance"}'
curl -s -X POST http://127.0.0.1:8398/api/cqrs/admin/reload -H "Authorization: $TOKEN"
```

```json
{
  "mode": "maintenance",
  "reactorsReloaded": ["autoShipPendingOrders", "notifyShippingPartner"],
  "projectionsReloaded": ["orderSummary", "pendingShipments"],
  "decidersReloaded": ["shipmentNotice"],
  "decidersRefused": ["order (collides with a built-in decider)"]
}
```

**`order` refused to load, on purpose.** `--tutorial` registered this repo's
example Go `order` aggregate (`aggregates/order.go`) in `main.go`, before any
JS file was read. The document's own `order` aggregate happens to share that
name, so the generated `order.js` collides with it — and
[a Go aggregate, once registered, can never be displaced by JS](reference/gateway.md#admin).
The reload report names the refusal instead of silently keeping whichever
code loaded first, which is the difference between a surprise discovered
under load and one discovered in a report you were already reading.

Nothing about this is specific to the example domains. **Any** Go aggregate
registered at boot claims its name ahead of every JS decider — that is the
rule, and `--tutorial` is just a convenient way to have one in the way. On a
real system the thing in the way is your own write side.

This is not a defect in the document or the importer — it is what happens
when a design document's boundaries and a running platform's boundaries are
drawn by different people, which is the ordinary case. The fix is to rename
one of them: `--aggregate` accepts an override per element, or you change
the Go aggregate — whichever is cheaper. (Here there is a third option that
a real system does not have: drop `--tutorial`, and the name is free.) This
walkthrough leaves it refused, deliberately, because the two slices that
*did* load are enough to show the rest of the flow end to end without a
rename.

## Go live with what did load

Exit maintenance and send the one command whose aggregate had no collision:

```sh
curl -s -X POST http://127.0.0.1:8398/api/cqrs/admin/mode -H "Authorization: $TOKEN" \
  -H 'Content-Type: application/json' -d '{"mode":"running"}'

curl -s -X POST http://127.0.0.1:8398/api/cqrs/shipmentNotice/order-1/NotifyShippingPartner \
  -H "Authorization: $TOKEN" -H 'Content-Type: application/json' -d '{"orderId":"order-1"}'
```

```json
{"events":[{"position":1,"aggregate":"shipmentNotice","aggregateId":"order-1",
            "type":"ShipmentNotified","data":{}, "...": "..."}]}
```

A command from a design document neither of us wrote, generated by code
neither of us wrote, appended a real event to a real log. Check the catalog
to see the whole picture at once — the built-in `order` next to the
imported `shipmentNotice`, in the same document:

```sh
curl -s http://127.0.0.1:8398/api/cqrs/catalog -H "Authorization: $TOKEN"
```

```json
{"aggregates": [
  {"name": "order", "origin": "go", "commands": ["AddOrderLine", "CancelOrder", "ConfirmOrder", "PlaceOrder"]},
  {"name": "shipmentNotice", "origin": "js", "commands": ["NotifyShippingPartner"],
   "produces": {"NotifyShippingPartner": ["ShipmentNotified"]}},
  {"name": "task", "origin": "go", "commands": ["CompleteTask", "CreateTask"]}
]}
```

Stop the server and clean up when you're done — everything above lived in
`tutorial/pb_data`, isolated from any instance you already have:

```sh
# Ctrl-C the background serve, then:
rm -rf tutorial
```

## Where next

- [Schema guide](schema.md) — the full mapping reference: field types, the
  three-namespace derivation, what round-trips and what doesn't, `export`.
- [JS guide](js-guide.md#reactors-tier-4) — what the generated reactor files
  actually mean, if you want to edit one by hand next.
- [Dashboard](dashboard.md#scaffold) — the same generator, without a design
  document: describe a slice in a form instead of importing one.
- The `order` refusal above is the same load-check every hand-written
  decider goes through — see
  [getting started](getting-started.md#change-code-without-restarting) for
  the ordinary (non-colliding) version of this workflow.
