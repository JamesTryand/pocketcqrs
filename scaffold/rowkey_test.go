package scaffold

import (
	"strings"
	"testing"
)

// grantRoundDomain: one grant round records many awards; "awards" has a row
// per award (rowKeyField, schema 3.14.0), plain merge plus a removal, and
// "flags" a toggle keyed the same way.
func grantRoundDomain() Domain {
	awarded := Event{Name: "GrantAwarded", Fields: []Field{{Name: "awardId", Type: "text"}, {Name: "amount", Type: "number"}}}
	reversed := Event{Name: "AwardReversed", Fields: []Field{{Name: "awardId", Type: "text"}}}
	return Domain{
		Aggregate: "grantRound",
		Commands: []Command{
			{Name: "Award", Fields: []Field{{Name: "awardId", Type: "text"}, {Name: "amount", Type: "number"}}, Events: []Event{awarded}},
			{Name: "Reverse", Fields: []Field{{Name: "awardId", Type: "text"}}, Events: []Event{reversed}},
		},
		ReadModels: []ReadModel{
			{Collection: "awards", Key: "awardId", RowKeyField: "awardId",
				Fields:    []Field{{Name: "amount", Type: "number"}},
				On:        []string{"GrantAwarded", "AwardReversed"},
				RemovedBy: []string{"AwardReversed"}},
			{Collection: "flags", Key: "awardId", RowKeyField: "awardId",
				Fields: []Field{{Name: "reversed", Type: "bool", Derivation: &FieldDerivation{
					Kind: DerivationToggle, OnEventIDs: []string{"AwardReversed"}, OffEventIDs: []string{"GrantAwarded"}}}},
				On: []string{"GrantAwarded", "AwardReversed"}},
		},
	}
}

// Both Go projection shapes keyed by a rowKeyField compile, with no stream
// key left in them; the generated code is built in its own module.
func TestGoRowKeyFieldCompiles(t *testing.T) {
	files, err := grantRoundDomain().GenerateGo()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.Contains(f.Source, "Projection struct") && strings.Contains(f.Source, "ev.AggregateID") {
			t.Errorf("%s still keys a row by the stream:\n%s", f.Name, f.Source)
		}
	}
	runGeneratedGo(t, grantRoundDomain(), "package grantRound\n")
}
