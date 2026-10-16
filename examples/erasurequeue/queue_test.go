package erasurequeue

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/pii"
	"github.com/jamestryand/pocketcqrs/retention"
)

// The queue shows each request where it stands (dotnetcqrs's
// The_queue_shows_each_request_where_it_stands): an ordinary projection over
// the dataSubject stream, holding no personal data, superusers only.
func TestTheQueueShowsEachRequestWhereItStands(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reg := decider.NewRegistry(store)
	retention.RegisterDataSubjects(reg)
	if err := Ensure(app); err != nil {
		t.Fatal(err)
	}
	if col, _ := app.FindCollectionByNameOrId(Collection); col == nil || col.ListRule != nil {
		t.Fatal("the queue must exist and be superusers only")
	}
	engine := consumers.NewEngine(store, nil)
	engine.Register(Projection{App: app})
	ctx := context.Background()
	send := func(id, cmd, payload, actor string) {
		t.Helper()
		if _, err := reg.HandleWithMeta(ctx, pii.SubjectAggregate, id, decider.Command{Name: cmd, Payload: json.RawMessage(payload)},
			map[string]any{"actor": actor, "now": "2026-01-01T00:00:00.000Z"}); err != nil {
			t.Fatal(err)
		}
		if err := engine.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	send("held-one", pii.RequestErasureCommand, `{"reason":"asked"}`, "u:clerk-1")
	send("held-one", retention.HoldErasureCommand, `{"reason":"tax","reviewAt":"2032-01-01T00:00:00Z"}`, retention.SystemActor)
	rows, err := List(ctx, app, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows %+v", rows)
	}
	r := rows[0]
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if r.SubjectID != "held-one" || r.Stage != retention.StageHeld || r.Reason != "tax" || r.RequestedBy != "u:clerk-1" ||
		!r.ReviewAt.Equal(time.Date(2032, 1, 1, 0, 0, 0, 0, time.UTC)) || r.LegalHold || !r.RequestedAt.Equal(epoch) {
		t.Fatalf("row %+v", r)
	}
	if held, _ := List(ctx, app, retention.StageHeld); len(held) != 1 {
		t.Fatal("not listed as held")
	}
	if erased, _ := List(ctx, app, retention.StageErased); len(erased) != 0 {
		t.Fatal("listed as erased")
	}
	send("held-one", retention.PlaceLegalHoldCommand, `{"reason":"audit"}`, "admin")
	if rows, _ := List(ctx, app, ""); len(rows) != 1 || !rows[0].LegalHold {
		t.Fatalf("legal hold not shown: %+v", rows)
	}
	send("direct", pii.EraseSubjectCommand, `{}`, "admin")
	if erased, _ := List(ctx, app, retention.StageErased); len(erased) != 1 || erased[0].SubjectID != "direct" {
		t.Fatalf("a direct erasure not shown: %+v", erased)
	}
}
