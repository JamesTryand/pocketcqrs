# Order

A customer order: placed for a customer, lines added while open, then
confirmed (needs at least one line) or cancelled. Demonstrates invariants in
a Go decider, a two-collection Go projection, a cross-aggregate saga, and a
JS rollup projection on top.

## Commands

| command | payload | intent | rejects when |
| --- | --- | --- | --- |
| `PlaceOrder` | `{ customerRef: string }` | place a new (open) order | order already exists; `customerRef` empty |
| `AddOrderLine` | `{ sku: string, qty: int, price: int }` | add a line (price in minor units) | order not open; `sku` empty; `qty <= 0`; `price < 0` |
| `ConfirmOrder` | `{}` | confirm for fulfillment | order not open; no lines |
| `CancelOrder` | `{}` | cancel | order not open |

## Events

| event | data | since version | notes |
| --- | --- | --- | --- |
| `OrderPlaced` | `{ customerRef: string }` | v1 | |
| `OrderLineAdded` | `{ sku, qty, price }` | v1 | |
| `OrderConfirmed` | `{}` | v1 | triggers the fulfillment saga |
| `OrderCancelled` | `{}` | v1 | |

## Read models

| collection | owner | shape | notes |
| --- | --- | --- | --- |
| `orders` | Go projection `orders` (`projections/orders.go`) | `orderId` (unique), `customerRef`, `status` (`open`/`confirmed`/`cancelled`), `total`, `lineCount` | totals **recomputed** from lines (idempotent) |
| `order_lines` | same projection | `lineKey` (`<orderId>:<sku>`, unique), `order` (relation → `orders`, cascade delete), `sku`, `qty`, `price` | one projection owns both so line upsert precedes totals recompute |
| `orders_by_customer` | JS projection (`pb_functions/orders_by_customer.js`) | `customerRef` (key), `orderCount`, `total` | recompute-style rollup over `orders` |

## Flows (reactors/sagas)

- **Fulfillment saga** (`reactors/fulfillment.go`): on `OrderConfirmed` →
  `CreateTask` on `task/fulfill-<orderId>` (title `"fulfil order <orderId>"`).
  The deterministic task id makes retries no-ops ("task already exists" is
  the idempotency mechanism). Dispatched through the registry with
  `causationId`/`correlationId` metadata and `actor=reactor:fulfillment`.

## Scenarios

- given nothing → `PlaceOrder {customerRef:"c1"}` → `OrderPlaced`
- given `OrderPlaced` → `ConfirmOrder` → rejected ("cannot confirm an empty order")
- given `OrderPlaced` → `AddOrderLine {sku:"s1",qty:2,price:1100}` → `OrderLineAdded`
- given `OrderPlaced`, `OrderLineAdded` → `ConfirmOrder` → `OrderConfirmed` (and a `fulfill-<id>` task appears)
- given `OrderConfirmed` → `AddOrderLine` → rejected ("order is not open")

## Implementation

- decider: `aggregates/order.go`
- projection: `projections/orders.go` (`NewOrders`)
- collections migration: `migrations/1754300000_orders_collections.go`
- saga: `reactors/fulfillment.go`
- JS rollup: `pb_functions/orders_by_customer.js` — ships in
  `examples/pb_functions/`, copy it in to run it
