//@trigger projection orders_by_customer on OrderPlaced OrderConfirmed OrderCancelled
//@schema orders_by_customer customerRef:text orderCount:number total:number
//@key customerRef
// Per-customer order rollup. Recompute-style: reads the Go-maintained
// "orders" collection (registered before JS projections, so it is caught
// up by the time this runs) and folds it into one row per customer.
// Recomputing (rather than incrementing) keeps replays idempotent.

function project(event) {
	var customerRef = event.data ? event.data.customerRef : null;

	// OrderConfirmed/OrderCancelled carry no payload: look the order up
	if (!customerRef) {
		var orders = pb.query("orders", "orderId = '" + event.aggregateId + "'", 1);
		if (!orders || orders.length === 0) {
			return; // order row not materialized yet; next event will revisit
		}
		customerRef = orders[0].customerRef;
	}

	var rows = pb.query("orders", "customerRef = '" + customerRef + "'", 500) || [];
	var total = 0;
	for (var i = 0; i < rows.length; i++) {
		total += rows[i].total || 0;
	}

	return { upsert: { key: customerRef, fields: { orderCount: rows.length, total: total } } };
}
