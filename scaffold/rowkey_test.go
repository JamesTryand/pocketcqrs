package scaffold

import (
	"strings"
	"testing"
)

// payRunDomain: one pay run records many payments; "payments" has a row per
// payment (rowKeyField, schema 3.14.0), plain merge plus a removal, and
// "flags" a toggle keyed the same way.
func payRunDomain() Domain {
	paid := Event{Name: "PersonPaid", Fields: []Field{{Name: "paymentId", Type: "text"}, {Name: "amount", Type: "number"}}}
	reversed := Event{Name: "PaymentReversed", Fields: []Field{{Name: "paymentId", Type: "text"}}}
	return Domain{
		Aggregate: "payRun",
		Commands: []Command{
			{Name: "Pay", Fields: []Field{{Name: "paymentId", Type: "text"}, {Name: "amount", Type: "number"}}, Events: []Event{paid}},
			{Name: "Reverse", Fields: []Field{{Name: "paymentId", Type: "text"}}, Events: []Event{reversed}},
		},
		ReadModels: []ReadModel{
			{Collection: "payments", Key: "paymentId", RowKeyField: "paymentId",
				Fields:    []Field{{Name: "amount", Type: "number"}},
				On:        []string{"PersonPaid", "PaymentReversed"},
				RemovedBy: []string{"PaymentReversed"}},
			{Collection: "flags", Key: "paymentId", RowKeyField: "paymentId",
				Fields: []Field{{Name: "reversed", Type: "bool", Derivation: &FieldDerivation{
					Kind: DerivationToggle, OnEventIDs: []string{"PaymentReversed"}, OffEventIDs: []string{"PersonPaid"}}}},
				On: []string{"PersonPaid", "PaymentReversed"}},
		},
	}
}

// Both Go projection shapes keyed by a rowKeyField compile, with no stream
// key left in them; the generated code is built in its own module.
func TestGoRowKeyFieldCompiles(t *testing.T) {
	files, err := payRunDomain().GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.Contains(f.Source, "Projection struct") && strings.Contains(f.Source, "ev.AggregateID") {
			t.Errorf("%s still keys a row by the stream:\n%s", f.Name, f.Source)
		}
	}
	runGeneratedGo(t, payRunDomain(), "package payRun\n")
}
