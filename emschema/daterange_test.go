package emschema

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jamestryand/pocketcqrs/scaffold"
)

// dateRangeDocument builds a minimal EventModeling document exercising
// the dateRange filter (schema 2.4.0, readModel.filters): a tickets read
// model with an openedOn column and a declared `dateRange` filter over it.
func dateRangeDocument() *Document {
	return &Document{
		EventModelingSchemaVersion: SchemaVersion,
		ID:                         "d1",
		Name:                       "Tickets DateRange",
		Swimlanes:                  []Swimlane{{ID: "sys", Name: "System", Kind: "system"}},
		Events: map[string]Event{
			"ticket-opened": {
				Name: "Ticket Opened", SwimlaneID: "sys", Aggregate: "Ticket",
				Fields: []Field{
					{Name: "ticketId", Type: "string", IDAttribute: true},
					{Name: "agentId", Type: "string"},
					{Name: "openedOn", Type: "date"},
				},
			},
		},
		Commands: map[string]Command{
			"open-ticket": {
				Name: "Open Ticket", Aggregate: "Ticket",
				Fields: []Field{
					{Name: "ticketId", Type: "string"},
					{Name: "agentId", Type: "string"},
					{Name: "openedOn", Type: "date"},
				},
			},
		},
		ReadModels: map[string]ReadModel{
			"tickets": {
				Name:              "Tickets",
				BuiltFromEventIDs: []string{"ticket-opened"},
				Fields: []Field{
					{Name: "ticketId", Type: "string", IDAttribute: true},
					{Name: "agentId", Type: "string"},
					{Name: "openedOn", Type: "date"},
				},
				Filters: []ReadModelFilter{
					{Param: "dateRange", Field: "openedOn", Kind: FilterDateRange,
						Presets: []string{DateRangePresetLast7Days, DateRangePresetLastCalendarMonth, DateRangePresetCustom}},
				},
			},
		},
		Screens: map[string]Screen{
			"scr-log":  {Name: "Open Ticket Screen"},
			"scr-view": {Name: "View Tickets"},
		},
		Slices: []Slice{
			{
				ID: "open-ticket-slice", Name: "Open Ticket", Pattern: PatternStateChange,
				SwimlaneID: "sys", Status: "wireframe", ScreenID: "scr-log",
				CommandID: "open-ticket", EventIDs: []string{"ticket-opened"},
			},
			{
				ID: "view-tickets-slice", Name: "View Tickets", Pattern: PatternStateView,
				SwimlaneID: "sys", Status: "wireframe", ScreenID: "scr-view",
				ReadModelID: "tickets",
				Scenarios: []Scenario{
					{
						ID: "lead-sees-tickets-in-range", Name: "a dateRange query narrows rows to the custom bounds",
						Kind: KindStateView,
						Given: []EventRef{
							{EventID: "ticket-opened", Data: json.RawMessage(`{"ticketId":"t1","agentId":"a1","openedOn":"2026-08-15"}`)},
							{EventID: "ticket-opened", Data: json.RawMessage(`{"ticketId":"t2","agentId":"a1","openedOn":"2026-07-01"}`)},
						},
						When: json.RawMessage(`{"readModelId":"tickets",` +
							`"queryParams":{"dateRange":{"kind":"custom","from":"2026-08-01","to":"2026-08-31"}}}`),
						Then: json.RawMessage(`{"result":{"entries":[{"ticketId":"t1","agentId":"a1","openedOn":"2026-08-15"}]}}`),
					},
				},
			},
		},
	}
}

// TestImportAndVerifyDateRangeFilter is the round-trip proof this feature
// needs: Map() must carry a declared filter through to scaffold.ReadModel,
// and Verify() must prove — through the real generated JS projection, the
// same path a live import takes — that a custom dateRange query narrows the
// projected rows to only those inside [from, to], not just report every row
// unfiltered.
func TestImportAndVerifyDateRangeFilter(t *testing.T) {
	doc := dateRangeDocument()

	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatalf("expected the document to map cleanly: %v", err)
	}
	if len(mapped.Domains) != 1 {
		t.Fatalf("expected one aggregate, got %d", len(mapped.Domains))
	}
	d := mapped.Domains[0]
	if len(d.ReadModels) != 1 {
		t.Fatalf("expected one read model, got %d", len(d.ReadModels))
	}
	rm := d.ReadModels[0]
	if len(rm.Filters) != 1 {
		t.Fatalf("expected one filter carried through, got %d: %+v", len(rm.Filters), rm.Filters)
	}
	f := rm.Filters[0]
	if f.Param != "dateRange" || f.Field != "openedOn" || f.Kind != scaffold.FilterDateRange {
		t.Fatalf("filter not carried through correctly: %+v", f)
	}
	if len(f.Presets) != 3 {
		t.Fatalf("expected 3 presets carried through, got %d: %+v", len(f.Presets), f.Presets)
	}

	results, err := Verify(doc, mapped, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected one scenario result, got %d", len(results))
	}
	res := results[0]
	if !res.Passed {
		t.Fatalf("the dateRange narrowing scenario must pass against the generated projection: %s", res.Detail)
	}

	// Load-bearing check: perturb the expected result to include the
	// out-of-range row too, and confirm the assertion actually fails — a
	// scenario that would pass either way proves nothing about filtering.
	perturbed := dateRangeDocument()
	perturbed.Slices[1].Scenarios[0].Then = json.RawMessage(
		`{"result":{"entries":[{"ticketId":"t1","agentId":"a1","openedOn":"2026-08-15"},` +
			`{"ticketId":"t2","agentId":"a1","openedOn":"2026-07-01"}]}}`)
	badResults, err := Verify(perturbed, mapped, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(badResults) != 1 || badResults[0].Passed {
		t.Fatalf("expected the perturbed (unfiltered) expectation to fail, proving the filter is load-bearing: %+v", badResults)
	}
}

// TestDateRangeFilterUnknownKindRejected mirrors the groupBy toggle-subfield
// rejection: a filter with a kind this verifier does not know how to apply
// is refused by the mapper with a clear error, rather than silently doing
// nothing (which would make the missing-filter gap invisible again).
func TestDateRangeFilterUnknownKindRejected(t *testing.T) {
	doc := dateRangeDocument()
	rm := doc.ReadModels["tickets"]
	rm.Filters[0].Kind = "amountRange"
	doc.ReadModels["tickets"] = rm

	_, err := Map(doc, Options{})
	if err == nil {
		t.Fatal("expected an unsupported filter kind to be refused")
	}
	if !strings.Contains(err.Error(), "dateRange") || !strings.Contains(err.Error(), "amountRange") {
		t.Fatalf("the refusal must name the unsupported kind and what is supported: %v", err)
	}
}

// TestDateRangeFilterUnknownFieldRejected: a filter naming a field the read
// model does not itself declare is refused at the mapper, the same
// reasoning mapScopes uses for a via read model that does not exist.
func TestDateRangeFilterUnknownFieldRejected(t *testing.T) {
	doc := dateRangeDocument()
	rm := doc.ReadModels["tickets"]
	rm.Filters[0].Field = "notAField"
	doc.ReadModels["tickets"] = rm

	_, err := Map(doc, Options{})
	if err == nil {
		t.Fatal("expected a filter naming an unknown field to be refused")
	}
	if !strings.Contains(err.Error(), "notAField") {
		t.Fatalf("the refusal must name the bad field: %v", err)
	}
}

// TestScaffoldValidateRejectsBadFilter is the third layer of the same
// rejection, mirroring the groupBy precedent: a hand-built scaffold.Domain
// (the dashboard wizard's shape, or a test) with a filter declaring no
// presets is refused by Validate() directly, not just by the importer.
func TestScaffoldValidateRejectsBadFilter(t *testing.T) {
	d := scaffold.Domain{
		Aggregate: "ticket",
		Commands: []scaffold.Command{
			{Name: "logTime", Once: true, Events: []scaffold.Event{{Name: "timeLogged", NoFields: true}}},
		},
		ReadModels: []scaffold.ReadModel{
			{
				Collection: "tickets", Key: "ticketId",
				Fields: []scaffold.Field{{Name: "ticketId", Type: "text"}, {Name: "openedOn", Type: "date"}},
				Filters: []scaffold.ReadModelFilter{
					{Param: "dateRange", Field: "openedOn", Kind: scaffold.FilterDateRange},
				},
			},
		},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("expected Validate to reject a dateRange filter with no presets")
	} else if !strings.Contains(err.Error(), "dateRange") || !strings.Contains(err.Error(), "presets") {
		t.Fatalf("the refusal must name the filter and the missing presets: %v", err)
	}
}

// TestResolveDateRangeFilterPresets checks last7Days/lastCalendarMonth
// resolve to the expected bounds against a FIXED reference instant — real
// production behaviour depends on the wall clock, so this is the only place
// that behaviour is checked deterministically.
func TestResolveDateRangeFilterPresets(t *testing.T) {
	ref := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	from, to, err := resolveDateRangeFilter(DateRangePresetLast7Days, nil, ref)
	if err != nil {
		t.Fatalf("last7Days: %v", err)
	}
	wantFrom := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	if !from.Equal(wantFrom) || !to.Equal(wantTo) {
		t.Fatalf("last7Days: got [%s, %s], want [%s, %s]", from, to, wantFrom, wantTo)
	}

	from, to, err = resolveDateRangeFilter(DateRangePresetLastCalendarMonth, nil, ref)
	if err != nil {
		t.Fatalf("lastCalendarMonth: %v", err)
	}
	wantFrom = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	wantTo = time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	if !from.Equal(wantFrom) || !to.Equal(wantTo) {
		t.Fatalf("lastCalendarMonth: got [%s, %s], want [%s, %s]", from, to, wantFrom, wantTo)
	}

	_, _, err = resolveDateRangeFilter(DateRangePresetCustom, map[string]any{}, ref)
	if err == nil {
		t.Fatal("expected custom with no from/to to be refused")
	}
}
