package scaffold

import (
	"strings"
	"testing"
)

// partsDomain: an invoice's lines as a row per project (rowsPerListItem,
// schema 3.16.0), with a subtotal once every line is converted (sumOfItems)
// and a status by event (byEvent); a discard removes every row.
func partsDomain() Domain {
	lines := Field{Name: "lines", Type: "json"}
	drafted := Event{Name: "InvoiceDrafted", Fields: []Field{{Name: "invoiceId", Type: "text"}, {Name: "customerId", Type: "text"}, lines}}
	issued := Event{Name: "InvoiceIssued", Fields: []Field{{Name: "invoiceId", Type: "text"}, lines}}
	discarded := Event{Name: "InvoiceDiscarded", Fields: []Field{{Name: "invoiceId", Type: "text"}}}
	return Domain{
		Aggregate: "invoice",
		Commands: []Command{
			{Name: "Draft", Fields: drafted.Fields, Events: []Event{drafted}},
			{Name: "Issue", Fields: issued.Fields, Events: []Event{issued}},
			{Name: "Discard", Fields: discarded.Fields, Events: []Event{discarded}},
		},
		ReadModels: []ReadModel{{
			Collection: "parts", Key: "partId",
			RowsPerListItem: &RowsPerListItem{ListField: "lines", GroupBy: "projectId"},
			Fields: []Field{
				{Name: "invoiceId", Type: "text"}, {Name: "projectId", Type: "text"}, {Name: "customerId", Type: "text"},
				{Name: "status", Type: "text", Derivation: &FieldDerivation{Kind: DerivationByEvent,
					Values: map[string]any{"InvoiceDrafted": "draft", "InvoiceIssued": "issued"}}},
				lines,
				{Name: "subtotal", Type: "number", Derivation: &FieldDerivation{Kind: DerivationSumOfItems, Subfield: "convertedAmount"}},
			},
			On:        []string{"InvoiceDrafted", "InvoiceIssued", "InvoiceDiscarded"},
			RemovedBy: []string{"InvoiceDiscarded"},
		}},
	}
}

// partsBehaviour runs the generated Go projection against a PocketBase test
// app: rows per project, the unassigned line in none, customerId kept from
// the draft, the subtotal only once converted, a dropped project's row
// gone, and a discard leaving nothing.
const partsBehaviour = `package invoice

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/events"
)

func TestParts(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	parts := core.NewBaseCollection("parts")
	for _, f := range []string{"partId", "invoiceId", "projectId", "customerId", "status"} {
		parts.Fields.Add(&core.TextField{Name: f})
	}
	parts.Fields.Add(&core.JSONField{Name: "lines"}, &core.NumberField{Name: "subtotal"})
	streams := core.NewBaseCollection("parts_streams")
	streams.Fields.Add(&core.TextField{Name: "stream"}, &core.JSONField{Name: "state"})
	if err := app.Save(parts); err != nil {
		t.Fatal(err)
	}
	if err := app.Save(streams); err != nil {
		t.Fatal(err)
	}
	p := NewParts(app)
	apply := func(typ, data string) {
		if err := p.Apply(context.Background(), events.Event{AggregateID: "inv-1", Type: typ, Data: json.RawMessage(data)}); err != nil {
			t.Fatal(err)
		}
	}
	rows := func() map[string]*core.Record {
		recs, err := app.FindAllRecords("parts")
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]*core.Record{}
		for _, r := range recs {
			out[r.GetString("partId")] = r
		}
		return out
	}

	apply("InvoiceDrafted", ` + "`" + `{"invoiceId":"inv-1","customerId":"c1","lines":[{"projectId":"p1","localAmount":225},{"projectId":"p2","localAmount":100},{"projectId":"p3","localAmount":5}]}` + "`" + `)
	got := rows()
	if len(got) != 3 || got["inv-1|p1"].GetString("status") != "draft" || got["inv-1|p1"].Get("subtotal") != 0.0 {
		t.Fatalf("after the draft: %v", got)
	}
	apply("InvoiceIssued", ` + "`" + `{"invoiceId":"inv-1","lines":[{"projectId":"p1","convertedAmount":306},{"projectId":"p1","convertedAmount":306},{"projectId":"p2","convertedAmount":136},{"convertedAmount":22.44}]}` + "`" + `)
	got = rows()
	if len(got) != 2 {
		t.Fatalf("after the issue, want p1 and p2: %v", got)
	}
	for key, want := range map[string]float64{"inv-1|p1": 612, "inv-1|p2": 136} {
		r := got[key]
		if r == nil || r.GetFloat("subtotal") != want || r.GetString("customerId") != "c1" || r.GetString("status") != "issued" {
			t.Errorf("%s: %v", key, r)
		}
	}
	apply("InvoiceDiscarded", ` + "`" + `{"invoiceId":"inv-1"}` + "`" + `)
	if got := rows(); len(got) != 0 {
		t.Errorf("after the discard: %v", got)
	}
	if recs, _ := app.FindAllRecords("parts_streams"); len(recs) != 0 {
		t.Errorf("the discard left the stream's state")
	}
}
`

func TestGoItemRowsRun(t *testing.T) {
	if err := partsDomain().Validate(); err != nil {
		t.Fatal(err)
	}
	runGeneratedGo(t, partsDomain(), partsBehaviour)
}

func TestItemRowsAreChecked(t *testing.T) {
	for name, edit := range map[string]func(*ReadModel){
		"with rowKeyField":     func(rm *ReadModel) { rm.RowKeyField = "invoiceId" },
		"listField not a list": func(rm *ReadModel) { rm.RowsPerListItem.ListField = "customerId" },
		"groupBy not a field":  func(rm *ReadModel) { rm.RowsPerListItem.GroupBy = "ghost" },
		"sumOfItems on text":   func(rm *ReadModel) { rm.Fields[5].Type = "text" },
		"sumOfItems unnamed":   func(rm *ReadModel) { rm.Fields[5].Derivation.Subfield = "" },
		"a toggle": func(rm *ReadModel) {
			rm.Fields[3].Derivation = &FieldDerivation{Kind: DerivationToggle, OnEventIDs: []string{"a"}, OffEventIDs: []string{"b"}}
		},
		"sumOfItems elsewhere": func(rm *ReadModel) { rm.RowsPerListItem = nil },
	} {
		d := partsDomain()
		edit(&d.ReadModels[0])
		if err := d.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The JS projection keeps its streams' state in a superusers-only companion.
func TestJSItemRowsDeclareTheirState(t *testing.T) {
	files, err := partsDomain().Generate()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f.Name, "parts.js") {
			for _, want := range []string{"//@schema parts_streams stream:text state:json", "//@rule parts_streams superuser", "itemRows: op"} {
				if !strings.Contains(f.Source, want) {
					t.Errorf("parts.js lacks %q:\n%s", want, f.Source)
				}
			}
			return
		}
	}
	t.Fatal("no parts.js generated")
}

// A hand projection (schema 3.17.0) is not generated, on either target:
// only a .example to start from. Its Go query route still is.
func TestHandProjectionIsAnExample(t *testing.T) {
	d := payRunDomain()
	d.ReadModels[0].Hand = true
	js, err := d.Generate()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]File{}
	for _, f := range js {
		names[f.Name] = f
	}
	if _, ok := names["payments.js"]; ok {
		t.Error("payments.js was generated for a hand projection")
	}
	if f, ok := names["payments.js.example"]; !ok || f.Kind != "example" || !strings.Contains(f.Source, "hand-written projection") {
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
	if gonames["payments.go"] || !gonames["payments.go.example"] || !gonames["payments_query.go"] {
		t.Errorf("Go files: %v", gonames)
	}
}
