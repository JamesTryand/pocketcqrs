package functions

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// TestPocketBaseFilterCoversScopesAndDateRange is the piece (B) investigation
// platform/eventmodeling-verify-gaps' Stage 2b execution plan asked for: does
// PocketBase's own generic GET /api/collections/{name}/records already cover
// item 4's two query shapes (a scopes-style relation semi-join, a dateRange
// range filter) for free, against a REAL read-model collection carrying
// BOTH a relation field and a date field — not the plain unscoped example
// docs/consuming.md shows.
//
// Modeled directly on project/timesheets' real need: `staff` (a manager and
// two other staff members), `projects` (each with a `managerId` relation to
// `staff`), `time_entries` (a `projectId` relation to `projects`, a `taskDate`
// date field, a `staffId` text field) — the same shape a `time-entries`
// read model with a `pmStaffId` scope and a `dateRange` filter would need to
// serve via PocketBase's own filter= syntax instead of a generated route.
//
// One HTTP request combines BOTH: `filter=projectId.managerId = "<pmId>"
// && taskDate >= "..." && taskDate <= "..."` — relation-traversal semi-join
// AND date-range narrowing in the same query, over a public (unauthenticated)
// list rule so this reproduces what a same-origin frontend (docs/consuming.md
// Pattern 1) actually sends.
func TestPocketBaseFilterCoversScopesAndDateRange(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	staff := core.NewBaseCollection("staff")
	staff.ListRule = strPtr("")
	staff.ViewRule = strPtr("")
	staff.Fields.Add(&core.TextField{Name: "name", Required: true})
	if err := app.Save(staff); err != nil {
		t.Fatalf("creating staff: %v", err)
	}

	projects := core.NewBaseCollection("projects")
	projects.ListRule = strPtr("")
	projects.ViewRule = strPtr("")
	projects.Fields.Add(&core.TextField{Name: "name", Required: true})
	projects.Fields.Add(&core.RelationField{Name: "managerId", CollectionId: staff.Id, MaxSelect: 1, Required: true})
	if err := app.Save(projects); err != nil {
		t.Fatalf("creating projects: %v", err)
	}

	timeEntries := core.NewBaseCollection("time_entries")
	timeEntries.ListRule = strPtr("")
	timeEntries.ViewRule = strPtr("")
	timeEntries.Fields.Add(&core.RelationField{Name: "projectId", CollectionId: projects.Id, MaxSelect: 1, Required: true})
	timeEntries.Fields.Add(&core.TextField{Name: "staffId", Required: true})
	timeEntries.Fields.Add(&core.DateField{Name: "taskDate", Required: true})
	timeEntries.Fields.Add(&core.NumberField{Name: "hours"})
	if err := app.Save(timeEntries); err != nil {
		t.Fatalf("creating time_entries: %v", err)
	}

	pm1 := core.NewRecord(staff)
	pm1.Set("name", "PM One")
	mustSave(t, app, pm1)
	pm2 := core.NewRecord(staff)
	pm2.Set("name", "PM Two")
	mustSave(t, app, pm2)

	projA := core.NewRecord(projects)
	projA.Set("name", "Project A")
	projA.Set("managerId", pm1.Id)
	mustSave(t, app, projA)
	projB := core.NewRecord(projects)
	projB.Set("name", "Project B")
	projB.Set("managerId", pm2.Id)
	mustSave(t, app, projB)

	// te1: project A (pm1), in range -> should match
	te1 := core.NewRecord(timeEntries)
	te1.Set("projectId", projA.Id)
	te1.Set("staffId", "s1")
	te1.Set("taskDate", "2026-08-15 00:00:00.000Z")
	te1.Set("hours", 8)
	mustSave(t, app, te1)

	// te2: project A (pm1), OUT of range -> must be excluded by the date filter
	te2 := core.NewRecord(timeEntries)
	te2.Set("projectId", projA.Id)
	te2.Set("staffId", "s1")
	te2.Set("taskDate", "2026-07-01 00:00:00.000Z")
	te2.Set("hours", 6)
	mustSave(t, app, te2)

	// te3: project B (pm2), in date range but WRONG manager -> must be
	// excluded by the relation semi-join, proving the join isn't vacuous
	te3 := core.NewRecord(timeEntries)
	te3.Set("projectId", projB.Id)
	te3.Set("staffId", "s2")
	te3.Set("taskDate", "2026-08-20 00:00:00.000Z")
	te3.Set("hours", 7)
	mustSave(t, app, te3)

	pbRouter, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	mux, err := pbRouter.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	filter := `projectId.managerId = "` + pm1.Id + `" && taskDate >= "2026-08-01 00:00:00.000Z" && taskDate <= "2026-08-31 23:59:59.999Z"`
	reqURL := srv.URL + "/api/collections/time_entries/records?filter=" + url.QueryEscape(filter)

	resp, err := http.Get(reqURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", reqURL, resp.StatusCode)
	}

	var body struct {
		Items []struct {
			ID        string `json:"id"`
			ProjectID string `json:"projectId"`
			StaffID   string `json:"staffId"`
			TaskDate  string `json:"taskDate"`
		} `json:"items"`
		TotalItems int `json:"totalItems"`
	}
	if err := readJSON(resp, &body); err != nil {
		t.Fatal(err)
	}

	if body.TotalItems != 1 {
		t.Fatalf("expected exactly 1 matching row (te1), got %d: %+v", body.TotalItems, body.Items)
	}
	if body.Items[0].ID != te1.Id {
		t.Fatalf("expected the matching row to be te1 (%s), got %s", te1.Id, body.Items[0].ID)
	}

	// Confirm the relation-only half in isolation: pm1's filter with NO date
	// bound must return both of pm1's own entries (te1 AND te2), proving the
	// exclusion above came from the date range, not some other narrowing.
	relOnlyFilter := `projectId.managerId = "` + pm1.Id + `"`
	relOnlyURL := srv.URL + "/api/collections/time_entries/records?filter=" + url.QueryEscape(relOnlyFilter)
	relResp, err := http.Get(relOnlyURL)
	if err != nil {
		t.Fatal(err)
	}
	defer relResp.Body.Close()
	var relBody struct {
		TotalItems int `json:"totalItems"`
	}
	if err := readJSON(relResp, &relBody); err != nil {
		t.Fatal(err)
	}
	if relBody.TotalItems != 2 {
		t.Fatalf("expected pm1's relation filter alone to return both of pm1's entries (2), got %d", relBody.TotalItems)
	}
}

func strPtr(s string) *string { return &s }

func mustSave(t *testing.T, app core.App, rec *core.Record) {
	t.Helper()
	if err := app.Save(rec); err != nil {
		t.Fatalf("saving %s record: %v", rec.Collection().Name, err)
	}
}

func readJSON(resp *http.Response, v any) error {
	return json.NewDecoder(resp.Body).Decode(v)
}
