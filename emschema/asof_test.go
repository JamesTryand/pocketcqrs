package emschema

import (
	"encoding/json"
	"testing"
	"time"
)

// asOfDocument builds a minimal EventModeling document exercising schema
// 2.6.0's readModelQuery.asOf: a time-entries read model with a last7Days
// dateRange filter, and a stateView scenario that pins asOf so the scenario
// passes deterministically regardless of the real wall-clock date — the
// exact gap dotnetcqrs's own asOf port (commit 2febf60, v0.7.0) fixed for
// project/timesheets's export-pm-slice (calendar drift flipped it PASS/FAIL
// on a rolling ~7-day cadence).
func asOfDocument(asOf *string) *Document {
	return &Document{
		EventModelingSchemaVersion: SchemaVersion,
		ID:                         "d1",
		Name:                       "Time Entries AsOf",
		Swimlanes:                  []Swimlane{{ID: "sys", Name: "System", Kind: "system"}},
		Events: map[string]Event{
			"time-logged": {
				Name: "Time Logged", SwimlaneID: "sys", Aggregate: "TimeEntry",
				Fields: []Field{
					{Name: "entryId", Type: "string", IDAttribute: true},
					{Name: "taskDate", Type: "date"},
				},
			},
		},
		Commands: map[string]Command{
			"log-time": {
				Name: "Log Time", Aggregate: "TimeEntry",
				Fields: []Field{
					{Name: "entryId", Type: "string"},
					{Name: "taskDate", Type: "date"},
				},
			},
		},
		ReadModels: map[string]ReadModel{
			"time-entries": {
				Name:              "Time Entries",
				BuiltFromEventIDs: []string{"time-logged"},
				Fields: []Field{
					{Name: "entryId", Type: "string", IDAttribute: true},
					{Name: "taskDate", Type: "date"},
				},
				Filters: []ReadModelFilter{
					{Param: "dateRange", Field: "taskDate", Kind: FilterDateRange,
						Presets: []string{DateRangePresetLast7Days}},
				},
			},
		},
		Screens: map[string]Screen{
			"scr-log":  {Name: "Log Time Screen"},
			"scr-view": {Name: "View Time Entries"},
		},
		Slices: []Slice{
			{
				ID: "log-time-slice", Name: "Log Time", Pattern: PatternStateChange,
				SwimlaneID: "sys", Status: "wireframe", ScreenID: "scr-log",
				CommandID: "log-time", EventIDs: []string{"time-logged"},
			},
			{
				ID: "view-time-entries-slice", Name: "View Time Entries", Pattern: PatternStateView,
				SwimlaneID: "sys", Status: "wireframe", ScreenID: "scr-view",
				ReadModelID: "time-entries",
				Scenarios: []Scenario{
					{
						ID: "last7days-pinned", Name: "a last7Days query pinned to asOf narrows deterministically",
						Kind: KindStateView,
						Given: []EventRef{
							// inside [2026-08-31, 2026-09-06] when asOf = 2026-09-06
							{EventID: "time-logged", Data: json.RawMessage(`{"entryId":"e1","taskDate":"2026-09-01"}`)},
							// outside that window
							{EventID: "time-logged", Data: json.RawMessage(`{"entryId":"e2","taskDate":"2026-08-01"}`)},
						},
						When: readModelQueryJSON("time-entries", `{"dateRange":{"kind":"last7Days"}}`, asOf),
						Then: json.RawMessage(`{"result":{"entries":[{"entryId":"e1","taskDate":"2026-09-01"}]}}`),
					},
				},
			},
		},
	}
}

// readModelQueryJSON builds a stateView scenario's `when`, optionally
// including asOf — building it via ReadModelQuery + json.Marshal rather
// than a hand-written literal keeps this test honest about the real field
// name/shape ReadModelQuery.AsOf decodes.
func readModelQueryJSON(readModelID, queryParams string, asOf *string) json.RawMessage {
	q := ReadModelQuery{ReadModelID: readModelID, QueryParams: json.RawMessage(queryParams), AsOf: asOf}
	encoded, err := json.Marshal(q)
	if err != nil {
		panic(err)
	}
	return encoded
}

func strPtr(s string) *string { return &s }

// TestImportAndVerifyAsOf is the load-bearing proof: without asOf, this
// scenario's pass/fail would depend on today's real date (exactly the
// export-pm-slice drift dotnetcqrs's port fixed); with asOf pinned, it must
// pass deterministically no matter when the suite actually runs.
func TestImportAndVerifyAsOf(t *testing.T) {
	doc := asOfDocument(strPtr("2026-09-06"))

	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatalf("expected the document to map cleanly: %v", err)
	}

	results, err := Verify(doc, mapped, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected one scenario result, got %d", len(results))
	}
	if res := results[0]; !res.Passed {
		t.Fatalf("the asOf-pinned scenario must pass regardless of the real clock: %s", res.Detail)
	}

	// Load-bearing check: an asOf far outside e1's date must exclude it,
	// proving asOf is actually driving the resolved window rather than
	// being silently ignored in favor of the live clock.
	late := asOfDocument(strPtr("2026-12-25"))
	lateResults, err := Verify(late, mapped, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(lateResults) != 1 || lateResults[0].Passed {
		t.Fatalf("expected a far-future asOf to exclude e1, proving asOf is load-bearing: %+v", lateResults)
	}
}

// TestAsOfMalformedRejected: an asOf that doesn't parse as a recognized
// date is refused with a clear error naming the bad value, the same
// "no vacuous pass" posture parseFilterDate already takes for row values.
func TestAsOfMalformedRejected(t *testing.T) {
	doc := asOfDocument(strPtr("not-a-date"))

	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatalf("expected the document to map cleanly: %v", err)
	}

	results, err := Verify(doc, mapped, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Passed {
		t.Fatalf("expected a malformed asOf to fail the scenario with a clear detail, got: %+v", results)
	}
	if results[0].Detail == "" {
		t.Fatal("expected a non-empty failure detail naming the bad asOf value")
	}
}

// TestAsOfAbsentUsesLiveClock: a scenario with no asOf keeps resolving
// against nowFunc() exactly as before this capability existed -- proven by
// overriding nowFunc for the duration of the test, the same mechanism
// TestResolveDateRangeFilterPresets's package-level sibling tests rely on.
func TestAsOfAbsentUsesLiveClock(t *testing.T) {
	orig := nowFunc
	defer func() { nowFunc = orig }()

	// Pin the "live" clock to the exact instant the asOf test above uses,
	// so an absent asOf produces the identical result the pinned test
	// asserts -- proving the two code paths converge, not just that this
	// one doesn't crash.
	fixed, err := parseFilterDateString("2026-09-06")
	if err != nil {
		t.Fatal(err)
	}
	nowFunc = func() time.Time { return fixed }

	doc := asOfDocument(nil)
	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatalf("expected the document to map cleanly: %v", err)
	}
	results, err := Verify(doc, mapped, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Passed {
		t.Fatalf("expected the live-clock (pinned via nowFunc) path to pass identically: %+v", results)
	}
}
