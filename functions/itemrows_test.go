package functions

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/events"
)

// partsSource keeps a row per invoice and project (rowsPerListItem, schema
// 3.16.0): each list-carrying event replaces the invoice's lines, the other
// fields merge from the invoice's events, and the subtotal is the sum of
// the project's converted amounts once every line has one (sumOfItems).
const partsSource = `//@trigger projection parts on Drafted Issued Discarded
//@schema parts partId:text invoiceId:text projectId:text customerId:text status:text lines:json subtotal:number
//@key partId
//@schema parts_streams stream:text state:json
//@key stream
//@rule parts_streams superuser
function project(event) {
  var op = { stream: event.aggregateId, streams: 'parts_streams', listField: 'lines', groupBy: 'projectId',
    sums: { subtotal: 'convertedAmount' }, fields: {} };
  if (event.type === 'Discarded') { op.remove = true; return { collection: 'parts', itemRows: op }; }
  var data = event.data || {};
  ['invoiceId', 'customerId'].forEach(function (f) { if (f in data) { op.fields[f] = data[f]; } });
  op.fields.status = event.type === 'Drafted' ? 'draft' : 'issued';
  if ('lines' in data) { op.items = data.lines; }
  return { collection: 'parts', itemRows: op };
}
`

var partsLog = []events.Event{
	{Position: 1, AggregateID: "inv-1", Type: "Drafted", Data: json.RawMessage(`{"invoiceId":"inv-1","customerId":"c1","lines":[
		{"entryId":"t1","projectId":"p1","localAmount":225},{"entryId":"t2","projectId":"p1","localAmount":225},
		{"entryId":"t3","projectId":"p2","localAmount":100},{"entryId":"t4","projectId":"p3","localAmount":5}]}`)},
	{Position: 2, AggregateID: "inv-1", Type: "Issued", Data: json.RawMessage(`{"invoiceId":"inv-1","lines":[
		{"entryId":"t1","projectId":"p1","convertedAmount":306},{"entryId":"t2","projectId":"p1","convertedAmount":306},
		{"entryId":"t3","projectId":"p2","convertedAmount":136},{"entryId":"fx","convertedAmount":22.44}]}`)},
}

// wantParts checks the rows after partsLog: p3 left with its line, the
// unassigned line is in no row, and customerId (from the draft only) is
// kept on rows the issue rebuilt.
func wantParts(t *testing.T, rows map[string]map[string]any) {
	t.Helper()
	if len(rows) != 2 {
		t.Fatalf("want rows inv-1|p1 and inv-1|p2, got %v", rows)
	}
	for key, want := range map[string]map[string]any{
		"inv-1|p1": {"projectId": "p1", "invoiceId": "inv-1", "customerId": "c1", "status": "issued", "subtotal": 612.0},
		"inv-1|p2": {"projectId": "p2", "invoiceId": "inv-1", "customerId": "c1", "status": "issued", "subtotal": 136.0},
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
		var lines []any
		raw, _ := json.Marshal(row["lines"])
		_ = json.Unmarshal(raw, &lines)
		if want := map[string]int{"inv-1|p1": 2, "inv-1|p2": 1}[key]; len(lines) != want {
			t.Errorf("%s has %d lines, want %d", key, len(lines), want)
		}
	}
}

func TestItemRowsOverAFixture(t *testing.T) {
	spec, err := LoadProjectionSource(NewGojaRuntime(nil), nil, "parts.js", partsSource)
	if err != nil {
		t.Fatal(err)
	}
	res, err := DryRunProjectionOver(spec, partsLog)
	if err != nil {
		t.Fatal(err)
	}
	wantParts(t, res.Rows["parts"])

	// a draft's subtotal is absent: its lines aren't converted yet
	res, err = DryRunProjectionOver(spec, partsLog[:1])
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows["parts"]) != 3 {
		t.Fatalf("a draft has 3 parts, got %v", res.Rows["parts"])
	}
	for key, row := range res.Rows["parts"] {
		if row["subtotal"] != nil {
			t.Errorf("%s: subtotal %v before conversion", key, row["subtotal"])
		}
	}

	// a discard removes every part, and the stream's own state
	discarded := append(append([]events.Event{}, partsLog...),
		events.Event{Position: 3, AggregateID: "inv-1", Type: "Discarded", Data: json.RawMessage(`{"invoiceId":"inv-1"}`)})
	res, err = DryRunProjectionOver(spec, discarded)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows["parts"]) != 0 || len(res.Rows["parts_streams"]) != 0 {
		t.Errorf("a discard leaves %v and %v", res.Rows["parts"], res.Rows["parts_streams"])
	}
}

func TestItemRowsLive(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	spec, err := LoadProjectionFile(NewGojaRuntime(nil), app, writeTempFn(t, "parts.js", partsSource))
	if err != nil {
		t.Fatal(err)
	}
	if err := ReconcileSchemas(app, []*ProjectionSpec{spec}, ""); err != nil {
		t.Fatal(err)
	}
	if col, _ := app.FindCollectionByNameOrId("parts_streams"); col == nil || col.ListRule != nil {
		t.Fatal("parts_streams must exist and be superusers only")
	}
	apply := func(evs ...events.Event) {
		for _, ev := range evs {
			if err := spec.Consumer().Apply(context.Background(), ev); err != nil {
				t.Fatal(err)
			}
		}
	}
	read := func() map[string]map[string]any {
		recs, err := app.FindAllRecords("parts")
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]map[string]any{}
		for _, r := range recs {
			out[r.GetString("partId")] = r.PublicExport()
		}
		return out
	}
	apply(partsLog...)
	wantParts(t, read())

	apply(events.Event{Position: 3, AggregateID: "inv-1", Type: "Discarded", Data: json.RawMessage(`{}`)})
	if rows := read(); len(rows) != 0 {
		t.Errorf("a discard leaves %v", rows)
	}
	if recs, _ := app.FindAllRecords("parts_streams"); len(recs) != 0 {
		t.Errorf("a discard leaves the stream's state")
	}
}
