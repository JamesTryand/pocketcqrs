package scaffold

import "testing"

// shipmentDomain: "shipments" is a plain merge with a byEvent status (schema
// 3.16.0); "counts" counts notes per shipment and sets a status by event,
// one of them a count trigger too.
func shipmentDomain() Domain {
	staged := Event{Name: "ShipmentStaged", Fields: []Field{{Name: "shipmentId", Type: "text"}, {Name: "total", Type: "number"}}}
	noted := Event{Name: "ShipmentNoted", Fields: []Field{{Name: "shipmentId", Type: "text"}, {Name: "note", Type: "text"}}}
	sent := Event{Name: "ShipmentSent", Fields: []Field{{Name: "shipmentId", Type: "text"}}}
	status := Field{Name: "status", Type: "text", Derivation: &FieldDerivation{Kind: DerivationByEvent,
		Values: map[string]any{"ShipmentStaged": "staged", "ShipmentSent": "sent", "ShipmentNoted": "noted"}}}
	return Domain{
		Aggregate: "shipment",
		Commands: []Command{
			{Name: "Stage", Fields: staged.Fields, Events: []Event{staged}},
			{Name: "Note", Fields: noted.Fields, Events: []Event{noted}},
			{Name: "Send", Fields: []Field{{Name: "shipmentId", Type: "text"}}, Events: []Event{sent}},
		},
		ReadModels: []ReadModel{
			{Collection: "shipments", Key: "shipmentId",
				Fields: []Field{{Name: "total", Type: "number"}, status},
				On:     []string{"ShipmentStaged", "ShipmentNoted", "ShipmentSent"}},
			{Collection: "counts", Key: "shipmentId",
				Fields: []Field{{Name: "notes", Type: "number", Derivation: &FieldDerivation{Kind: DerivationCount,
					IncrementOnEventIDs: []string{"ShipmentNoted"}}}, status},
				On: []string{"ShipmentStaged", "ShipmentNoted", "ShipmentSent"}},
		},
	}
}

// Both Go projection shapes with a byEvent field compile in their own module.
func TestGoByEventCompiles(t *testing.T) {
	if err := shipmentDomain().Validate(); err != nil {
		t.Fatal(err)
	}
	runGeneratedGo(t, shipmentDomain(), "package shipment\n")
}

// A byEvent literal of the wrong type for its field is refused.
func TestByEventLiteralTypeIsChecked(t *testing.T) {
	d := shipmentDomain()
	d.ReadModels[0].Fields[1].Derivation.Values = map[string]any{"ShipmentSent": 1.0}
	if err := d.Validate(); err == nil {
		t.Error("a number literal for a text field was accepted")
	}
}
