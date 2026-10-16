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

// TestPocketBaseFilterCoversScopesAndDateRange asks whether PocketBase's own
// generic GET /api/collections/{name}/records already covers two query
// shapes (a scopes-style relation semi-join, a dateRange range filter) for
// free, against a REAL read-model collection carrying BOTH a relation field
// and a date field — not the plain unscoped example docs/consuming.md shows.
//
// A support desk: `agents` (two team leads), `teams` (each with a `leadId`
// relation to `agents`), `tickets` (a `teamId` relation to `teams`, an
// `openedOn` date field, an `agentId` text field) — the same shape a
// `tickets` read model with a `leadAgentId` scope and a `dateRange` filter
// would need to serve via PocketBase's own filter= syntax instead of a
// generated route.
//
// One HTTP request combines BOTH: `filter=teamId.leadId = "<leadId>"
// && openedOn >= "..." && openedOn <= "..."` — relation-traversal semi-join
// AND date-range narrowing in the same query, over a public (unauthenticated)
// list rule so this reproduces what a same-origin frontend (docs/consuming.md
// Pattern 1) actually sends.
func TestPocketBaseFilterCoversScopesAndDateRange(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	agents := core.NewBaseCollection("agents")
	agents.ListRule = strPtr("")
	agents.ViewRule = strPtr("")
	agents.Fields.Add(&core.TextField{Name: "name", Required: true})
	if err := app.Save(agents); err != nil {
		t.Fatalf("creating agents: %v", err)
	}

	teams := core.NewBaseCollection("teams")
	teams.ListRule = strPtr("")
	teams.ViewRule = strPtr("")
	teams.Fields.Add(&core.TextField{Name: "name", Required: true})
	teams.Fields.Add(&core.RelationField{Name: "leadId", CollectionId: agents.Id, MaxSelect: 1, Required: true})
	if err := app.Save(teams); err != nil {
		t.Fatalf("creating teams: %v", err)
	}

	tickets := core.NewBaseCollection("tickets")
	tickets.ListRule = strPtr("")
	tickets.ViewRule = strPtr("")
	tickets.Fields.Add(&core.RelationField{Name: "teamId", CollectionId: teams.Id, MaxSelect: 1, Required: true})
	tickets.Fields.Add(&core.TextField{Name: "agentId", Required: true})
	tickets.Fields.Add(&core.DateField{Name: "openedOn", Required: true})
	tickets.Fields.Add(&core.NumberField{Name: "effort"})
	if err := app.Save(tickets); err != nil {
		t.Fatalf("creating tickets: %v", err)
	}

	lead1 := core.NewRecord(agents)
	lead1.Set("name", "Lead One")
	mustSave(t, app, lead1)
	lead2 := core.NewRecord(agents)
	lead2.Set("name", "Lead Two")
	mustSave(t, app, lead2)

	teamA := core.NewRecord(teams)
	teamA.Set("name", "Team A")
	teamA.Set("leadId", lead1.Id)
	mustSave(t, app, teamA)
	teamB := core.NewRecord(teams)
	teamB.Set("name", "Team B")
	teamB.Set("leadId", lead2.Id)
	mustSave(t, app, teamB)

	// tk1: team A (lead1), in range -> should match
	tk1 := core.NewRecord(tickets)
	tk1.Set("teamId", teamA.Id)
	tk1.Set("agentId", "a1")
	tk1.Set("openedOn", "2026-08-15 00:00:00.000Z")
	tk1.Set("effort", 8)
	mustSave(t, app, tk1)

	// tk2: team A (lead1), OUT of range -> must be excluded by the date filter
	tk2 := core.NewRecord(tickets)
	tk2.Set("teamId", teamA.Id)
	tk2.Set("agentId", "a1")
	tk2.Set("openedOn", "2026-07-01 00:00:00.000Z")
	tk2.Set("effort", 6)
	mustSave(t, app, tk2)

	// tk3: team B (lead2), in date range but WRONG lead -> must be
	// excluded by the relation semi-join, proving the join isn't vacuous
	tk3 := core.NewRecord(tickets)
	tk3.Set("teamId", teamB.Id)
	tk3.Set("agentId", "a2")
	tk3.Set("openedOn", "2026-08-20 00:00:00.000Z")
	tk3.Set("effort", 7)
	mustSave(t, app, tk3)

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

	filter := `teamId.leadId = "` + lead1.Id + `" && openedOn >= "2026-08-01 00:00:00.000Z" && openedOn <= "2026-08-31 23:59:59.999Z"`
	reqURL := srv.URL + "/api/collections/tickets/records?filter=" + url.QueryEscape(filter)

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
			ID       string `json:"id"`
			TeamID   string `json:"teamId"`
			AgentID  string `json:"agentId"`
			OpenedOn string `json:"openedOn"`
		} `json:"items"`
		TotalItems int `json:"totalItems"`
	}
	if err := readJSON(resp, &body); err != nil {
		t.Fatal(err)
	}

	if body.TotalItems != 1 {
		t.Fatalf("expected exactly 1 matching row (tk1), got %d: %+v", body.TotalItems, body.Items)
	}
	if body.Items[0].ID != tk1.Id {
		t.Fatalf("expected the matching row to be tk1 (%s), got %s", tk1.Id, body.Items[0].ID)
	}

	// Confirm the relation-only half in isolation: lead1's filter with NO date
	// bound must return both of lead1's team's tickets (tk1 AND tk2), proving the
	// exclusion above came from the date range, not some other narrowing.
	relOnlyFilter := `teamId.leadId = "` + lead1.Id + `"`
	relOnlyURL := srv.URL + "/api/collections/tickets/records?filter=" + url.QueryEscape(relOnlyFilter)
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
		t.Fatalf("expected lead1's relation filter alone to return both of their team's tickets (2), got %d", relBody.TotalItems)
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
