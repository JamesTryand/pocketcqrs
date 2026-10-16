package scaffold

import "testing"

// flagDomain: flagging stamps a reason and clearing who cleared it, and a
// toggle derives from both, so both events also merge their fields.
func flagDomain() Domain {
	flagged := Event{Name: "Flagged", Fields: []Field{{Name: "ticketId", Type: "text"}, {Name: "reason", Type: "text"}}}
	cleared := Event{Name: "Cleared", Fields: []Field{{Name: "ticketId", Type: "text"}, {Name: "clearedBy", Type: "text"}}}
	return Domain{
		Aggregate: "ticket",
		Commands: []Command{
			{Name: "Flag", Fields: flagged.Fields, Events: []Event{flagged}},
			{Name: "Clear", Fields: cleared.Fields, Events: []Event{cleared}},
		},
		ReadModels: []ReadModel{{
			Collection: "flags", Key: "ticketId",
			Fields: []Field{{Name: "reason", Type: "text"}, {Name: "clearedBy", Type: "text"},
				{Name: "isCleared", Type: "bool", Derivation: &FieldDerivation{Kind: DerivationToggle,
					OnEventIDs: []string{"Cleared"}, OffEventIDs: []string{"Flagged"}}}},
			On: []string{"Flagged", "Cleared"},
		}},
	}
}

const flagBehaviour = `package ticket

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
	col.Fields.Add(&core.TextField{Name: "ticketId"}, &core.TextField{Name: "reason"}, &core.TextField{Name: "clearedBy"}, &core.BoolField{Name: "isCleared"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	p := NewFlags(app)
	for _, ev := range []events.Event{
		{AggregateID: "t1", Type: "Flagged", Data: json.RawMessage(` + "`" + `{"ticketId":"t1","reason":"late"}` + "`" + `)},
		{AggregateID: "t1", Type: "Cleared", Data: json.RawMessage(` + "`" + `{"ticketId":"t1","clearedBy":"sup1"}` + "`" + `)},
	} {
		if err := p.Apply(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	rec, err := app.FindFirstRecordByData("flags", "ticketId", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.GetString("reason") != "late" || rec.GetString("clearedBy") != "sup1" || !rec.GetBool("isCleared") {
		t.Fatalf("row: %v", rec.PublicExport())
	}
}
`

func TestGoToggleEventsStillMerge(t *testing.T) {
	runGeneratedGo(t, flagDomain(), flagBehaviour)
}

// A generated Go projection commits each event and its position in one
// data.db transaction: the engine calls Commit, and the position reads
// back from data.db.
const flagCommitBehaviour = `package ticket

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/events"
)

func TestCommit(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	col := core.NewBaseCollection("flags")
	col.Fields.Add(&core.TextField{Name: "ticketId"}, &core.TextField{Name: "reason"}, &core.TextField{Name: "clearedBy"}, &core.BoolField{Name: "isCleared"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	var c consumers.Consumer = NewFlags(app)
	cm, ok := c.(consumers.Committer)
	if !ok {
		t.Fatal("not a committer")
	}
	ctx := context.Background()
	if err := cm.Commit(ctx, events.Event{Position: 7, AggregateID: "t1", Type: "Flagged", Data: json.RawMessage(` + "`" + `{"ticketId":"t1","reason":"late"}` + "`" + `)}); err != nil {
		t.Fatal(err)
	}
	if err := cm.Commit(ctx, events.Event{Position: 8, AggregateID: "t1", Type: "Flagged", Data: json.RawMessage(` + "`" + `not json` + "`" + `)}); err == nil {
		t.Fatal("a bad event committed")
	}
	pos, err := c.(consumers.OwnCheckpoints).Checkpoints().Checkpoint(ctx, "flags")
	if err != nil || pos != 7 {
		t.Fatalf("position %d, %v", pos, err)
	}
	rec, err := app.FindFirstRecordByData("flags", "ticketId", "t1")
	if err != nil || rec.GetString("reason") != "late" {
		t.Fatalf("row: %v %v", rec, err)
	}
}
`

func TestGoProjectionCommits(t *testing.T) {
	runGeneratedGo(t, flagDomain(), flagCommitBehaviour)
}
