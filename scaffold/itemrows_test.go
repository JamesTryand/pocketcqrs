package scaffold

import (
	"strings"
	"testing"
)

// loadsDomain: a delivery's parcels as a row per depot (rowsPerListItem,
// schema 3.16.0), with a total weight once every parcel is weighed
// (sumOfItems) and a status by event (byEvent); a cancellation removes every
// row.
func loadsDomain() Domain {
	parcels := Field{Name: "parcels", Type: "json"}
	planned := Event{Name: "DeliveryPlanned", Fields: []Field{{Name: "deliveryId", Type: "text"}, {Name: "customerId", Type: "text"}, parcels}}
	dispatched := Event{Name: "DeliveryDispatched", Fields: []Field{{Name: "deliveryId", Type: "text"}, parcels}}
	cancelled := Event{Name: "DeliveryCancelled", Fields: []Field{{Name: "deliveryId", Type: "text"}}}
	return Domain{
		Aggregate: "delivery",
		Commands: []Command{
			{Name: "Plan", Fields: planned.Fields, Events: []Event{planned}},
			{Name: "Dispatch", Fields: dispatched.Fields, Events: []Event{dispatched}},
			{Name: "Cancel", Fields: cancelled.Fields, Events: []Event{cancelled}},
		},
		ReadModels: []ReadModel{{
			Collection: "loads", Key: "loadId",
			RowsPerListItem: &RowsPerListItem{ListField: "parcels", GroupBy: "depotId"},
			Fields: []Field{
				{Name: "deliveryId", Type: "text"}, {Name: "depotId", Type: "text"}, {Name: "customerId", Type: "text"},
				{Name: "status", Type: "text", Derivation: &FieldDerivation{Kind: DerivationByEvent,
					Values: map[string]any{"DeliveryPlanned": "planned", "DeliveryDispatched": "dispatched"}}},
				parcels,
				{Name: "totalWeight", Type: "number", Derivation: &FieldDerivation{Kind: DerivationSumOfItems, Subfield: "weighedWeight"}},
			},
			On:        []string{"DeliveryPlanned", "DeliveryDispatched", "DeliveryCancelled"},
			RemovedBy: []string{"DeliveryCancelled"},
		}},
	}
}

// loadsBehaviour runs the generated Go projection against a PocketBase test
// app: rows per depot, the unassigned parcel in none, customerId kept from
// the plan, the total weight only once weighed, a dropped depot's row gone,
// and a cancellation leaving nothing.
const loadsBehaviour = `package delivery

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/events"
)

func totalWeight(r *core.Record) string {
	raw, _ := json.Marshal(r.PublicExport()["totalWeight"])
	return string(raw)
}

func TestLoads(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	loads := core.NewBaseCollection("loads")
	for _, f := range []string{"loadId", "deliveryId", "depotId", "customerId", "status"} {
		loads.Fields.Add(&core.TextField{Name: f})
	}
	loads.Fields.Add(&core.JSONField{Name: "parcels"}, &core.JSONField{Name: "totalWeight"}) // sumOfItems: json, so null stays null
	streams := core.NewBaseCollection("loads_streams")
	streams.Fields.Add(&core.TextField{Name: "stream"}, &core.JSONField{Name: "state"})
	if err := app.Save(loads); err != nil {
		t.Fatal(err)
	}
	if err := app.Save(streams); err != nil {
		t.Fatal(err)
	}
	p := NewLoads(app)
	apply := func(typ, data string) {
		if err := p.Apply(context.Background(), events.Event{AggregateID: "del-1", Type: typ, Data: json.RawMessage(data)}); err != nil {
			t.Fatal(err)
		}
	}
	rows := func() map[string]*core.Record {
		recs, err := app.FindAllRecords("loads")
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]*core.Record{}
		for _, r := range recs {
			out[r.GetString("loadId")] = r
		}
		return out
	}

	apply("DeliveryPlanned", ` + "`" + `{"deliveryId":"del-1","customerId":"c1","parcels":[{"depotId":"d1","declaredWeight":225},{"depotId":"d2","declaredWeight":100},{"depotId":"d3","declaredWeight":5}]}` + "`" + `)
	got := rows()
	if len(got) != 3 || got["del-1|d1"].GetString("status") != "planned" || totalWeight(got["del-1|d1"]) != "null" {
		t.Fatalf("after the plan: %v", got)
	}
	apply("DeliveryDispatched", ` + "`" + `{"deliveryId":"del-1","parcels":[{"depotId":"d1","weighedWeight":306},{"depotId":"d1","weighedWeight":306},{"depotId":"d2","weighedWeight":136},{"weighedWeight":22.44}]}` + "`" + `)
	got = rows()
	if len(got) != 2 {
		t.Fatalf("after the dispatch, want d1 and d2: %v", got)
	}
	for key, want := range map[string]float64{"del-1|d1": 612, "del-1|d2": 136} {
		r := got[key]
		if r == nil || totalWeight(r) != strconv.FormatFloat(want, 'f', -1, 64) || r.GetString("customerId") != "c1" || r.GetString("status") != "dispatched" {
			t.Errorf("%s: %v", key, r)
		}
	}
	apply("DeliveryCancelled", ` + "`" + `{"deliveryId":"del-1"}` + "`" + `)
	if got := rows(); len(got) != 0 {
		t.Errorf("after the cancellation: %v", got)
	}
	if recs, _ := app.FindAllRecords("loads_streams"); len(recs) != 0 {
		t.Errorf("the cancellation left the stream's state")
	}
}
`

func TestGoItemRowsRun(t *testing.T) {
	if err := loadsDomain().Validate(); err != nil {
		t.Fatal(err)
	}
	runGeneratedGo(t, loadsDomain(), loadsBehaviour)
}

func TestItemRowsAreChecked(t *testing.T) {
	for name, edit := range map[string]func(*ReadModel){
		"with rowKeyField":     func(rm *ReadModel) { rm.RowKeyField = "deliveryId" },
		"listField not a list": func(rm *ReadModel) { rm.RowsPerListItem.ListField = "customerId" },
		"groupBy not a field":  func(rm *ReadModel) { rm.RowsPerListItem.GroupBy = "ghost" },
		"sumOfItems on text":   func(rm *ReadModel) { rm.Fields[5].Type = "text" },
		"sumOfItems unnamed":   func(rm *ReadModel) { rm.Fields[5].Derivation.Subfield = "" },
		"a toggle": func(rm *ReadModel) {
			rm.Fields[3].Derivation = &FieldDerivation{Kind: DerivationToggle, OnEventIDs: []string{"a"}, OffEventIDs: []string{"b"}}
		},
		"sumOfItems elsewhere": func(rm *ReadModel) { rm.RowsPerListItem = nil },
	} {
		d := loadsDomain()
		edit(&d.ReadModels[0])
		if err := d.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The JS projection keeps its streams' state in a superusers-only companion.
func TestJSItemRowsDeclareTheirState(t *testing.T) {
	files, err := loadsDomain().Generate()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f.Name, "loads.js") {
			for _, want := range []string{"//@schema loads_streams stream:text state:json", "//@rule loads_streams superuser", "itemRows: op"} {
				if !strings.Contains(f.Source, want) {
					t.Errorf("loads.js lacks %q:\n%s", want, f.Source)
				}
			}
			return
		}
	}
	t.Fatal("no loads.js generated")
}

// A hand projection (schema 3.17.0) is not generated, on either target:
// only a .example to start from. Its Go query route still is.
func TestHandProjectionIsAnExample(t *testing.T) {
	d := grantRoundDomain()
	d.ReadModels[0].Hand = true
	js, err := d.Generate()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]File{}
	for _, f := range js {
		names[f.Name] = f
	}
	if _, ok := names["awards.js"]; ok {
		t.Error("awards.js was generated for a hand projection")
	}
	if f, ok := names["awards.js.example"]; !ok || f.Kind != "example" || !strings.Contains(f.Source, "hand-written projection") {
		t.Errorf("no payments.js.example: %v", names)
	}
	gofiles, err := d.GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	gonames := map[string]bool{}
	for _, f := range gofiles {
		gonames[f.Name] = true
	}
	if gonames["awards.go"] || !gonames["awards.go.example"] || !gonames["awards_query.go"] {
		t.Errorf("Go files: %v", gonames)
	}
}
