package projections

import (
	"context"
	"errors"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/consumers"
)

// A projection's position lives in data.db: none until the first commit,
// then the last committed event's, and saved explicitly (a rebuild's 0).
func TestPositions(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	ctx := context.Background()
	p := Positions{App: app}
	if _, err := p.Checkpoint(ctx, "notes"); !errors.Is(err, consumers.ErrNoCheckpoint) {
		t.Fatalf("a projection that never committed: %v", err)
	}
	if err := p.SaveCheckpoint(ctx, "notes", 0); err != nil {
		t.Fatal(err)
	}
	if pos, err := p.Checkpoint(ctx, "notes"); err != nil || pos != 0 {
		t.Fatalf("an explicit 0 is a position: %d, %v", pos, err)
	}
}

// Commit applies an event's row ops and records its position in one
// transaction: a failure part-way leaves neither the rows nor the position.
func TestCommitIsOneTransaction(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	col := core.NewBaseCollection("notes")
	col.Fields.Add(&core.TextField{Name: "noteId"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	write := func(id string) func(tx core.App) error {
		return func(tx core.App) error {
			c, err := tx.FindCollectionByNameOrId("notes")
			if err != nil {
				return err
			}
			rec := core.NewRecord(c)
			rec.Set("noteId", id)
			return tx.Save(rec)
		}
	}
	if err := Commit(ctx, app, "notes", 1, write("n1")); err != nil {
		t.Fatal(err)
	}
	broken := func(tx core.App) error {
		if err := write("n2")(tx); err != nil {
			return err
		}
		return errors.New("the second op failed")
	}
	if err := Commit(ctx, app, "notes", 2, broken); err == nil {
		t.Fatal("the failure was swallowed")
	}
	recs, err := app.FindAllRecords("notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].GetString("noteId") != "n1" {
		t.Fatalf("the failed event's first op survived: %d rows", len(recs))
	}
	if pos, _ := (Positions{App: app}).Checkpoint(ctx, "notes"); pos != 1 {
		t.Fatalf("the position moved past the failed event: %d", pos)
	}
}
