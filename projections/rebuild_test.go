package projections

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"

	"github.com/jamestryand/pocketcqrs/consumers"
	"github.com/jamestryand/pocketcqrs/events"
	"github.com/jamestryand/pocketcqrs/writeguard"
)

// titles is a projection: a row per task, holding its title.
type titles struct{ app core.App }

func (titles) Name() string          { return "titles" }
func (titles) Collections() []string { return []string{"titles"} }
func (p titles) Apply(ctx context.Context, ev events.Event) error {
	var data struct{ Title string }
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		return err
	}
	col, err := p.app.FindCollectionByNameOrId("titles")
	if err != nil {
		return err
	}
	rec := core.NewRecord(col)
	rec.Set("taskId", ev.AggregateID)
	rec.Set("title", data.Title)
	return p.app.SaveWithContext(writeguard.MarkInternal(ctx), rec)
}

func titleRows(t *testing.T, app core.App) map[string]string {
	t.Helper()
	recs, err := app.FindRecordsByFilter("titles", "", "", -1, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range recs {
		if _, dup := out[r.GetString("taskId")]; dup {
			t.Fatalf("two rows for task %s: the projection was replayed without being wiped", r.GetString("taskId"))
		}
		out[r.GetString("taskId")] = r.GetString("title")
	}
	return out
}

// A rebuild requested through RebuildRequests (as the CLI and the admin API
// do) is carried out by the running engine: the projection's rows are
// wiped and rebuilt from the log, so a row that drifted from the events is
// right again. Requesting twice is one rebuild.
func TestRequestedRebuildRebuildsFromTheLog(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	col := core.NewBaseCollection("titles")
	col.Fields.Add(&core.TextField{Name: "taskId"}, &core.TextField{Name: "title"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	for _, id := range []string{"t1", "t2"} {
		if _, err := store.Append(ctx, "task", id, 0, []events.NewEvent{{Type: "TaskCreated", Data: json.RawMessage(`{"title":"` + id + ` title"}`)}}); err != nil {
			t.Fatal(err)
		}
	}

	requests, err := NewRebuildRequests(app.DB())
	if err != nil {
		t.Fatal(err)
	}
	engine := consumers.NewEngine(store, func(string, ...any) {})
	engine.Rebuilds = requests
	engine.Wipe = Wipe(app)
	engine.Register(titles{app: app})
	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// a row drifts from the log (a bug, a hand edit, a changed rule)
	recs, _ := app.FindRecordsByFilter("titles", "taskId = 't1'", "", 1, 0)
	recs[0].Set("title", "drifted")
	if err := app.SaveWithContext(writeguard.MarkInternal(ctx), recs[0]); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if err := requests.Request(ctx, "titles"); err != nil {
			t.Fatal(err)
		}
	}
	if pending, _ := requests.Pending(ctx); len(pending) != 1 {
		t.Fatalf("two requests for one projection are one request: %v", pending)
	}
	if got := titleRows(t, app); got["t1"] != "drifted" {
		t.Fatalf("recording a request changes nothing by itself: %v", got)
	}

	if err := engine.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got := titleRows(t, app)
	if len(got) != 2 || got["t1"] != "t1 title" || got["t2"] != "t2 title" {
		t.Fatalf("the rebuild restores exactly what the log says: %v", got)
	}
	if pending, _ := requests.Pending(ctx); len(pending) != 0 {
		t.Fatalf("the request is taken off: %v", pending)
	}
	if pos, _ := store.Checkpoint(ctx, "titles"); pos != 2 {
		t.Fatalf("caught up again: checkpoint %d", pos)
	}
}
