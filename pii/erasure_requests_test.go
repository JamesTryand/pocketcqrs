package pii

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/consumers"
)

// The queue shows each request where it stands (dotnetcqrs's
// The_queue_shows_each_request_where_it_stands): a collection an
// administrator screen lists, holding no personal data, readable by
// superusers only.
func TestTheQueueShowsEachRequestWhereItStands(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	g := newRig(t, PeriodRetentionPolicy{Years: 6, Activity: fixedActivity(epoch), Reason: "tax"})
	if err := EnsureErasureRequests(app); err != nil {
		t.Fatal(err)
	}
	if col, _ := app.FindCollectionByNameOrId(ErasureRequestsCollection); col == nil || col.ListRule != nil {
		t.Fatal("the queue must exist and be superusers only")
	}
	g.engine.Register(ErasureRequests{App: app})
	ctx := context.Background()

	g.send("held-one", RequestErasureCommand, `{"reason":"data-subject-request"}`, "u:manager-1")
	g.settle()
	rows, err := ListErasureRequests(ctx, app, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows %+v", rows)
	}
	r := rows[0]
	if r.SubjectID != "held-one" || r.Stage != StageHeld || r.Reason != "tax" || r.RequestedBy != "u:manager-1" ||
		!r.ReviewAt.Equal(epoch.AddDate(6, 0, 0)) || r.LegalHold || !r.RequestedAt.Equal(epoch) {
		t.Fatalf("row %+v", r)
	}
	if held, _ := ListErasureRequests(ctx, app, StageHeld); len(held) != 1 {
		t.Fatal("not listed as held")
	}
	if erased, _ := ListErasureRequests(ctx, app, StageErased); len(erased) != 0 {
		t.Fatal("listed as erased")
	}
	g.send("held-one", PlaceLegalHoldCommand, `{"reason":"audit"}`, "")
	g.settle()
	if rows, _ := ListErasureRequests(ctx, app, ""); len(rows) != 1 || !rows[0].LegalHold {
		t.Fatalf("legal hold not shown: %+v", rows)
	}
	_ = time.Now
}

func TestRegisterErasureGovernance(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	store, r := bareSubjects(t)
	engine := consumers.NewEngine(store, nil)
	if _, err := RegisterErasureGovernance(context.Background(), app, engine, r, store, AllowAllRetentionPolicy{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	names := strings.Join(engine.Names(), ",")
	if !strings.Contains(names, ErasureGovernorName) || !strings.Contains(names, "pii:erasure-requests") {
		t.Fatalf("registered %s", names)
	}
	if _, err := app.FindCollectionByNameOrId(ErasureRequestsCollection); err != nil {
		t.Fatal("no queue collection")
	}
}
