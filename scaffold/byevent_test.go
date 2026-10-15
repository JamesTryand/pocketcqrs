package scaffold

import "testing"

// invoiceDomain: "invoices" is a plain merge with a byEvent status (schema
// 3.16.0); "counts" counts notes per invoice and sets a status by event,
// one of them a count trigger too.
func invoiceDomain() Domain {
	staged := Event{Name: "InvoiceStaged", Fields: []Field{{Name: "invoiceId", Type: "text"}, {Name: "total", Type: "number"}}}
	noted := Event{Name: "InvoiceNoted", Fields: []Field{{Name: "invoiceId", Type: "text"}, {Name: "note", Type: "text"}}}
	sent := Event{Name: "InvoiceSent", Fields: []Field{{Name: "invoiceId", Type: "text"}}}
	status := Field{Name: "status", Type: "text", Derivation: &FieldDerivation{Kind: DerivationByEvent,
		Values: map[string]any{"InvoiceStaged": "staged", "InvoiceSent": "sent", "InvoiceNoted": "noted"}}}
	return Domain{
		Aggregate: "invoice",
		Commands: []Command{
			{Name: "Stage", Fields: staged.Fields, Events: []Event{staged}},
			{Name: "Note", Fields: noted.Fields, Events: []Event{noted}},
			{Name: "Send", Fields: []Field{{Name: "invoiceId", Type: "text"}}, Events: []Event{sent}},
		},
		ReadModels: []ReadModel{
			{Collection: "invoices", Key: "invoiceId",
				Fields: []Field{{Name: "total", Type: "number"}, status},
				On:     []string{"InvoiceStaged", "InvoiceNoted", "InvoiceSent"}},
			{Collection: "counts", Key: "invoiceId",
				Fields: []Field{{Name: "notes", Type: "number", Derivation: &FieldDerivation{Kind: DerivationCount,
					IncrementOnEventIDs: []string{"InvoiceNoted"}}}, status},
				On: []string{"InvoiceStaged", "InvoiceNoted", "InvoiceSent"}},
		},
	}
}

// Both Go projection shapes with a byEvent field compile in their own module.
func TestGoByEventCompiles(t *testing.T) {
	if err := invoiceDomain().Validate(); err != nil {
		t.Fatal(err)
	}
	runGeneratedGo(t, invoiceDomain(), "package invoice\n")
}

// A byEvent literal of the wrong type for its field is refused.
func TestByEventLiteralTypeIsChecked(t *testing.T) {
	d := invoiceDomain()
	d.ReadModels[0].Fields[1].Derivation.Values = map[string]any{"InvoiceSent": 1.0}
	if err := d.Validate(); err == nil {
		t.Error("a number literal for a text field was accepted")
	}
}
