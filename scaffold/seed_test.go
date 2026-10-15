package scaffold

import "testing"

// flagDomain: flagging stamps a reason and clearing who cleared it, and a
// toggle derives from both, so both events also merge their fields.
func flagDomain() Domain {
	flagged := Event{Name: "Flagged", Fields: []Field{{Name: "entryId", Type: "text"}, {Name: "reason", Type: "text"}}}
	cleared := Event{Name: "Cleared", Fields: []Field{{Name: "entryId", Type: "text"}, {Name: "clearedBy", Type: "text"}}}
	return Domain{
		Aggregate: "entry",
		Commands: []Command{
			{Name: "Flag", Fields: flagged.Fields, Events: []Event{flagged}},
			{Name: "Clear", Fields: cleared.Fields, Events: []Event{cleared}},
		},
		ReadModels: []ReadModel{{
			Collection: "flags", Key: "entryId",
			Fields: []Field{{Name: "reason", Type: "text"}, {Name: "clearedBy", Type: "text"},
				{Name: "isCleared", Type: "bool", Derivation: &FieldDerivation{Kind: DerivationToggle,
					OnEventIDs: []string{"Cleared"}, OffEventIDs: []string{"Flagged"}}}},
			On: []string{"Flagged", "Cleared"},
		}},
	}
}

const flagBehaviour = `package entry

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/events"
)

func TestFlags(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	col := core.NewBaseCollection("flags")
	col.Fields.Add(&core.TextField{Name: "entryId"}, &core.TextField{Name: "reason"}, &core.TextField{Name: "clearedBy"}, &core.BoolField{Name: "isCleared"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	p := NewFlags(app)
	for _, ev := range []events.Event{
		{AggregateID: "e1", Type: "Flagged", Data: json.RawMessage(` + "`" + `{"entryId":"e1","reason":"late"}` + "`" + `)},
		{AggregateID: "e1", Type: "Cleared", Data: json.RawMessage(` + "`" + `{"entryId":"e1","clearedBy":"m1"}` + "`" + `)},
	} {
		if err := p.Apply(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	rec, err := app.FindFirstRecordByData("flags", "entryId", "e1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.GetString("reason") != "late" || rec.GetString("clearedBy") != "m1" || !rec.GetBool("isCleared") {
		t.Fatalf("row: %v", rec.PublicExport())
	}
}
`

func TestGoToggleEventsStillMerge(t *testing.T) {
	runGeneratedGo(t, flagDomain(), flagBehaviour)
}
