package functions

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/projections"
)

// A JS projection commits each event's ops and its position in one data.db
// transaction: an event whose second op fails leaves no trace of its
// first, and its position stays where it was.
func TestJSProjectionCommitsAnEventWhole(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	spec, err := LoadProjectionFile(NewGojaRuntime(nil), app, writeTempFn(t, "notes.js", `//@trigger projection notes on Noted
//@schema notes noteId:text text:text
//@key noteId
function project(event) {
  var ops = [{ upsert: { key: event.data.noteId, fields: { text: event.data.text } } }];
  if (event.data.bad) { ops.push({ collection: 'nowhere', upsert: { key: 'x', fields: {} } }); }
  return ops;
}
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := ReconcileSchemas(app, []*ProjectionSpec{spec}, ""); err != nil {
		t.Fatal(err)
	}
	p := spec.Consumer()
	var c consumers.Consumer = p
	cm, ok := c.(consumers.Committer)
	if !ok {
		t.Fatal("a JS projection is not a committer")
	}
	ctx := context.Background()
	if err := cm.Commit(ctx, events.Event{Position: 1, Type: "Noted", Data: json.RawMessage(`{"noteId":"n1","text":"a"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := cm.Commit(ctx, events.Event{Position: 2, Type: "Noted", Data: json.RawMessage(`{"noteId":"n2","text":"b","bad":true}`)}); err == nil {
		t.Fatal("the failing op was swallowed")
	}
	if recs, _ := app.FindAllRecords("notes"); len(recs) != 1 {
		t.Fatalf("the failed event's first op survived: %d rows", len(recs))
	}
	if pos, err := (projections.Positions{App: app}).Checkpoint(ctx, "notes"); err != nil || pos != 1 {
		t.Fatalf("position %d, %v; want 1", pos, err)
	}
	own, ok := c.(consumers.OwnCheckpoints)
	if !ok {
		t.Fatal("a JS projection doesn't read its position from data.db")
	}
	if pos, _ := own.Checkpoints().Checkpoint(ctx, "notes"); pos != 1 {
		t.Fatalf("its own store reads %d", pos)
	}
}

// The dry run mirrors Commit: it reports the position it reached, every
// event counted (one the projection doesn't handle too, as Commit records
// it), and an event that fails leaves nothing behind.
func TestDryRunReportsItsPosition(t *testing.T) {
	spec, err := LoadProjectionSource(NewGojaRuntime(nil), nil, "notes.js", `//@trigger projection notes on Noted
//@schema notes noteId:text text:text
//@key noteId
function project(event) {
  return { upsert: { key: event.data.noteId, fields: { text: event.data.text } } };
}
`)
	if err != nil {
		t.Fatal(err)
	}
	res, err := DryRunProjectionOver(spec, []events.Event{
		{Position: 3, Type: "Noted", Data: json.RawMessage(`{"noteId":"n1","text":"a"}`)},
		{Position: 5, Type: "Other", Data: json.RawMessage(`{}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Position != 5 {
		t.Fatalf("position %d, want 5", res.Position)
	}
}
