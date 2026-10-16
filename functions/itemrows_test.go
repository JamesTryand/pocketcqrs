package functions

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/events"
)

// loadsSource keeps a row per delivery and depot (rowsPerListItem, schema
// 3.16.0): each list-carrying event replaces the delivery's parcels, the
// other fields merge from the delivery's events, and the total weight is
// the sum of the depot's weighed weights once every parcel has one
// (sumOfItems).
const loadsSource = `//@trigger projection loads on Planned Dispatched Cancelled
//@schema loads loadId:text deliveryId:text depotId:text customerId:text status:text parcels:json totalWeightKg:number
//@key loadId
//@schema loads_streams stream:text state:json
//@key stream
//@rule loads_streams superuser
function project(event) {
  var op = { stream: event.aggregateId, streams: 'loads_streams', listField: 'parcels', groupBy: 'depotId',
    sums: { totalWeightKg: 'weighedWeightKg' }, fields: {} };
  if (event.type === 'Cancelled') { op.remove = true; return { collection: 'loads', itemRows: op }; }
  var data = event.data || {};
  ['deliveryId', 'customerId'].forEach(function (f) { if (f in data) { op.fields[f] = data[f]; } });
  op.fields.status = event.type === 'Planned' ? 'planned' : 'dispatched';
  if ('parcels' in data) { op.items = data.parcels; }
  return { collection: 'loads', itemRows: op };
}
`

var loadsLog = []events.Event{
	{Position: 1, AggregateID: "del-1", Type: "Planned", Data: json.RawMessage(`{"deliveryId":"del-1","customerId":"c1","parcels":[
		{"parcelId":"pc1","depotId":"d1","declaredWeightKg":225},{"parcelId":"pc2","depotId":"d1","declaredWeightKg":225},
		{"parcelId":"pc3","depotId":"d2","declaredWeightKg":100},{"parcelId":"pc4","depotId":"d3","declaredWeightKg":5}]}`)},
	{Position: 2, AggregateID: "del-1", Type: "Dispatched", Data: json.RawMessage(`{"deliveryId":"del-1","parcels":[
		{"parcelId":"pc1","depotId":"d1","weighedWeightKg":306},{"parcelId":"pc2","depotId":"d1","weighedWeightKg":306},
		{"parcelId":"pc3","depotId":"d2","weighedWeightKg":136},{"parcelId":"loose","weighedWeightKg":22.44}]}`)},
}

// wantLoads checks the rows after loadsLog: d3 left with its parcel, the
// unassigned parcel is in no row, and customerId (from the plan only) is
// kept on rows the dispatch rebuilt.
func wantLoads(t *testing.T, rows map[string]map[string]any) {
	t.Helper()
	if len(rows) != 2 {
		t.Fatalf("want rows del-1|d1 and del-1|d2, got %v", rows)
	}
	for key, want := range map[string]map[string]any{
		"del-1|d1": {"depotId": "d1", "deliveryId": "del-1", "customerId": "c1", "status": "dispatched", "totalWeightKg": 612.0},
		"del-1|d2": {"depotId": "d2", "deliveryId": "del-1", "customerId": "c1", "status": "dispatched", "totalWeightKg": 136.0},
	} {
		row := rows[key]
		if row == nil {
			t.Errorf("no row %s in %v", key, rows)
			continue
		}
		for f, v := range want {
			if !jsonEqual(row[f], v) {
				t.Errorf("%s.%s = %v, want %v", key, f, row[f], v)
			}
		}
		var parcels []any
		raw, _ := json.Marshal(row["parcels"])
		_ = json.Unmarshal(raw, &parcels)
		if want := map[string]int{"del-1|d1": 2, "del-1|d2": 1}[key]; len(parcels) != want {
			t.Errorf("%s has %d parcels, want %d", key, len(parcels), want)
		}
	}
}

func TestItemRowsOverAFixture(t *testing.T) {
	spec, err := LoadProjectionSource(NewGojaRuntime(nil), nil, "loads.js", loadsSource)
	if err != nil {
		t.Fatal(err)
	}
	res, err := DryRunProjectionOver(spec, loadsLog)
	if err != nil {
		t.Fatal(err)
	}
	wantLoads(t, res.Rows["loads"])

	// a planned delivery's total weight is absent: its parcels aren't weighed yet
	res, err = DryRunProjectionOver(spec, loadsLog[:1])
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows["loads"]) != 3 {
		t.Fatalf("a planned delivery has 3 loads, got %v", res.Rows["loads"])
	}
	for key, row := range res.Rows["loads"] {
		if row["totalWeightKg"] != nil {
			t.Errorf("%s: total weight %v before weighing", key, row["totalWeightKg"])
		}
	}

	// a cancellation removes every load, and the stream's own state
	cancelled := append(append([]events.Event{}, loadsLog...),
		events.Event{Position: 3, AggregateID: "del-1", Type: "Cancelled", Data: json.RawMessage(`{"deliveryId":"del-1"}`)})
	res, err = DryRunProjectionOver(spec, cancelled)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows["loads"]) != 0 || len(res.Rows["loads_streams"]) != 0 {
		t.Errorf("a cancellation leaves %v and %v", res.Rows["loads"], res.Rows["loads_streams"])
	}
}

func TestItemRowsLive(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	spec, err := LoadProjectionFile(NewGojaRuntime(nil), app, writeTempFn(t, "loads.js", loadsSource))
	if err != nil {
		t.Fatal(err)
	}
	if err := ReconcileSchemas(app, []*ProjectionSpec{spec}, ""); err != nil {
		t.Fatal(err)
	}
	if col, _ := app.FindCollectionByNameOrId("loads_streams"); col == nil || col.ListRule != nil {
		t.Fatal("loads_streams must exist and be superusers only")
	}
	apply := func(evs ...events.Event) {
		for _, ev := range evs {
			if err := spec.Consumer().Apply(context.Background(), ev); err != nil {
				t.Fatal(err)
			}
		}
	}
	read := func() map[string]map[string]any {
		recs, err := app.FindAllRecords("loads")
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]map[string]any{}
		for _, r := range recs {
			out[r.GetString("loadId")] = r.PublicExport()
		}
		return out
	}
	apply(loadsLog...)
	wantLoads(t, read())

	apply(events.Event{Position: 3, AggregateID: "del-1", Type: "Cancelled", Data: json.RawMessage(`{}`)})
	if rows := read(); len(rows) != 0 {
		t.Errorf("a cancellation leaves %v", rows)
	}
	if recs, _ := app.FindAllRecords("loads_streams"); len(recs) != 0 {
		t.Errorf("a cancellation leaves the stream's state")
	}
}
