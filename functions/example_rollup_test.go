package functions

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/jamestryand/pocketcqrs/events"
)

// The shipped rollup example reads only its own collections (P1), so it
// verifies over a fixture: two orders for one customer, a line re-added.
func TestTheOrdersByCustomerExampleFoldsItsOwnRows(t *testing.T) {
	src, err := os.ReadFile("../examples/pb_functions/orders_by_customer.js")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := LoadProjectionSource(NewGojaRuntime(nil), nil, "orders_by_customer.js", string(src))
	if err != nil {
		t.Fatal(err)
	}
	ev := func(pos int64, order, typ, data string) events.Event {
		return events.Event{Position: pos, Aggregate: "order", AggregateID: order, Type: typ, Data: json.RawMessage(data)}
	}
	res, err := DryRunProjectionOver(spec, []events.Event{
		ev(1, "o1", "OrderPlaced", `{"customerRef":"c1"}`),
		ev(2, "o1", "OrderLineAdded", `{"sku":"s1","qty":2,"price":100}`),
		ev(3, "o2", "OrderPlaced", `{"customerRef":"c1"}`),
		ev(4, "o2", "OrderLineAdded", `{"sku":"s1","qty":1,"price":50}`),
		ev(5, "o1", "OrderLineAdded", `{"sku":"s1","qty":3,"price":100}`), // replaces o1's s1 line
		ev(6, "o3", "OrderPlaced", `{"customerRef":"c2"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Rows["orders_by_customer"]
	if fmt.Sprint(got["c1"]["orderCount"], got["c1"]["total"]) != "2 350" ||
		fmt.Sprint(got["c2"]["orderCount"], got["c2"]["total"]) != "1 0" {
		t.Fatalf("rollup %v", got)
	}
}
