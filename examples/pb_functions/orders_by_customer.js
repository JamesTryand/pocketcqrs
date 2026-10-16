//@trigger projection orders_by_customer on OrderPlaced OrderLineAdded
//@schema orders_by_customer customerRef:text orderCount:number total:number
//@key customerRef
//@schema orders_by_customer_lines lineKey:text orderId:text customerRef:text amount:number
//@key lineKey
//@schema orders_by_customer_orders orderId:text customerRef:text
//@key orderId
// Per-customer order rollup. A projection reads only its own collections,
// so this one keeps what it needs from the events itself (which customer
// each order is for, and each line's amount) rather than reading the Go
// projection's "orders". Recomputing from those rows (rather than
// incrementing) keeps a re-added line from counting twice.

function rows(collection, field, value) {
	return pb.query(collection, field + " = '" + value + "'", 500) || [];
}

function project(event) {
	var orderId = event.aggregateId;
	var customerRef, orders, lines, lineKey, amount;

	if (event.type === "OrderPlaced") {
		customerRef = event.data.customerRef;
		orders = rows("orders_by_customer_orders", "customerRef", customerRef)
			.filter(function (o) { return o.orderId !== orderId; });
		lines = rows("orders_by_customer_lines", "customerRef", customerRef);
		return [
			{ collection: "orders_by_customer_orders", upsert: { key: orderId, fields: { customerRef: customerRef } } },
			rollup(customerRef, orders.length + 1, lines),
		];
	}

	// OrderLineAdded: the line names its order, not the customer
	var order = rows("orders_by_customer_orders", "orderId", orderId);
	if (order.length === 0) {
		return; // a line for an order this projection never saw placed
	}
	customerRef = order[0].customerRef;
	lineKey = orderId + ":" + event.data.sku;
	amount = event.data.qty * event.data.price;
	orders = rows("orders_by_customer_orders", "customerRef", customerRef);
	// re-adding a sku replaces its line, as the Go projection does
	lines = rows("orders_by_customer_lines", "customerRef", customerRef)
		.filter(function (l) { return l.lineKey !== lineKey; })
		.concat([{ amount: amount }]);
	return [
		{ collection: "orders_by_customer_lines", upsert: { key: lineKey,
			fields: { orderId: orderId, customerRef: customerRef, amount: amount } } },
		rollup(customerRef, orders.length, lines),
	];
}

function rollup(customerRef, orderCount, lines) {
	var total = 0;
	for (var i = 0; i < lines.length; i++) {
		total += lines[i].amount || 0;
	}
	return { collection: "orders_by_customer", upsert: { key: customerRef,
		fields: { orderCount: orderCount, total: total } } };
}
