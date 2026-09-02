package functions

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/decider"
	"github.com/jamestryand/pocketcqrs/events"
)

func writeTempFn(t *testing.T, name, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	writeFn(t, filepath.Dir(path), name, src)
	return path
}

func TestLoadDeciderFileAndDryRun(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	// history: two note streams
	if _, err := store.Append(ctx, "note", "n1", 0, []events.NewEvent{
		{Type: "NoteCreated", Data: json.RawMessage(`{"text":"a"}`)},
		{Type: "NoteArchived", Data: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, "note", "n2", 0, []events.NewEvent{
		{Type: "NoteCreated", Data: json.RawMessage(`{"text":"b"}`)},
	}); err != nil {
		t.Fatal(err)
	}

	rt := NewGojaRuntime(nil)
	spec, err := LoadDeciderFile(rt, writeTempFn(t, "note.js", `//@trigger decider note
//@handles NoteCreated NoteArchived
`+noteDeciderJS))
	if err != nil {
		t.Fatal(err)
	}

	// all-streams fold
	res, err := DryRunDecider(store, spec, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Streams != 2 || res.Events != 3 {
		t.Fatalf("unexpected result: %+v", res)
	}

	// single-stream fold exposes the final state
	res, err = DryRunDecider(store, spec, "n1")
	if err != nil {
		t.Fatal(err)
	}
	state := res.State.(map[string]any)
	if state["text"] != "a" || state["archived"] != true {
		t.Fatalf("unexpected final state: %v", state)
	}
}

func TestDryRunDecide(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if _, err := store.Append(ctx, "note", "n1", 0, []events.NewEvent{
		{Type: "NoteCreated", Data: json.RawMessage(`{"text":"a"}`)},
	}); err != nil {
		t.Fatal(err)
	}

	rt := NewGojaRuntime(nil)
	spec, err := LoadDeciderFile(rt, writeTempFn(t, "note.js", `//@trigger decider note
//@handles NoteCreated NoteArchived
`+noteDeciderJS))
	if err != nil {
		t.Fatal(err)
	}

	// when: a valid command on the folded stream -> outcome events, no append
	res, err := DryRunDecide(store, spec, "n1",
		decider.Command{Name: "ArchiveNote", Payload: json.RawMessage(`{}`)},
		map[string]any{"now": "dry", "actor": "test"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rejected {
		t.Fatalf("a valid command was reported as rejected: %s", res.Message)
	}
	if len(res.Produced) != 1 || res.Produced[0].Type != "NoteArchived" {
		t.Fatalf("unexpected produced events: %+v", res.Produced)
	}

	// when: a domain-invalid command -> a REJECTION VERDICT, not an error.
	// The error return is reserved for a request that could not be answered;
	// a decider saying no IS the answer.
	res, err = DryRunDecide(store, spec, "n1",
		decider.Command{Name: "CreateNote", Payload: json.RawMessage(`{"text":"dup"}`)}, nil)
	if err != nil {
		t.Fatalf("a domain rejection must not surface as an error: %v", err)
	}
	if !res.Rejected {
		t.Fatal("expected a domain rejection")
	}
	if res.Message == "" {
		t.Fatal("a rejection must carry the decider's reason")
	}
	if len(res.Produced) != 0 {
		t.Fatalf("a rejection must produce nothing: %+v", res.Produced)
	}

	// and nothing was appended
	stream, _ := store.LoadStream(ctx, "note", "n1")
	if len(stream) != 1 {
		t.Fatalf("dry run appended to the stream: %+v", stream)
	}
}

// TestDryRunDecideSeparatesRejectionFromFailure is the test that fails if the
// two ever collapse back into one channel. A rejection and an unusable
// candidate took the same path before this split, which made a working
// decider's "no" indistinguishable from a broken file.
func TestDryRunDecideSeparatesRejectionFromFailure(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if _, err := store.Append(ctx, "note", "n1", 0, []events.NewEvent{
		{Type: "NoteCreated", Data: json.RawMessage(`{"text":"a"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	rt := NewGojaRuntime(nil)

	// a decider whose //@handles does not cover history in the log is
	// UNUSABLE — the contract gate must fail it as an error, never report it
	// as the domain refusing a command
	incomplete, err := LoadDeciderFile(rt, writeTempFn(t, "incomplete.js", `//@trigger decider note
//@handles NoteArchived
`+noteDeciderJS))
	if err != nil {
		t.Fatal(err)
	}
	res, err := DryRunDecide(store, incomplete, "n1",
		decider.Command{Name: "ArchiveNote", Payload: json.RawMessage(`{}`)}, nil)
	if err == nil {
		t.Fatalf("an unusable candidate must be an error, got verdict %+v", res)
	}
	if res != nil && res.Rejected {
		t.Fatal("an unusable candidate was misreported as a domain rejection")
	}

	// an unreadable stream is likewise a failure of the request, not a
	// verdict: nothing was asked of the domain at all
	good, err := LoadDeciderFile(rt, writeTempFn(t, "good.js", `//@trigger decider note
//@handles NoteCreated NoteArchived
`+noteDeciderJS))
	if err != nil {
		t.Fatal(err)
	}
	store.Close() // force the load to fail
	if _, err := DryRunDecide(store, good, "n1",
		decider.Command{Name: "ArchiveNote", Payload: json.RawMessage(`{}`)}, nil); err == nil {
		t.Fatal("a store failure must be an error, not a verdict")
	}
}

func TestDryRunProjectionMultiCollection(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if _, err := store.Append(ctx, "sale", "s1", 0, []events.NewEvent{
		{Type: "SalePlaced", Data: json.RawMessage(`{"cust":"c1","sku":"p1","qty":2}`)},
	}); err != nil {
		t.Fatal(err)
	}

	rt := NewGojaRuntime(nil)
	src := `//@trigger projection sales on SalePlaced
//@schema customers cust:text total:number
//@key cust
//@schema products sku:text sold:number
//@key sku
function project(event) {
	return [
		{ collection: "customers", upsert: { key: event.data.cust, fields: { total: event.data.qty } } },
		{ collection: "products", upsert: { key: event.data.sku, fields: { sold: event.data.qty } } },
	];
}
`
	spec, err := LoadProjectionFile(rt, nil, writeTempFn(t, "sales.js", src))
	if err != nil {
		t.Fatal(err)
	}

	res, err := DryRunProjection(store, spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Upserts != 2 || len(res.Rows) != 2 {
		t.Fatalf("unexpected simulation: %+v", res)
	}
	if res.Rows["customers"]["c1"]["total"] != int64(2) && res.Rows["customers"]["c1"]["total"] != float64(2) {
		t.Fatalf("unexpected customers rows: %+v", res.Rows["customers"])
	}
	if res.Rows["products"]["p1"]["sold"] == nil {
		t.Fatalf("unexpected products rows: %+v", res.Rows["products"])
	}

	// an op without collection is ambiguous for a multi-schema projection
	ambSrc := `//@trigger projection amb on SalePlaced
//@schema customers cust:text
//@key cust
//@schema products sku:text
//@key sku
function project(event) { return { upsert: { key: "x", fields: {} } }; }
`
	ambSpec, err := LoadProjectionFile(rt, nil, writeTempFn(t, "amb.js", ambSrc))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DryRunProjection(store, ambSpec); err == nil {
		t.Fatal("expected ambiguity error")
	}

	// an op naming an undeclared collection is rejected
	undSrc := `//@trigger projection und on SalePlaced
//@schema customers cust:text
//@key cust
function project(event) { return { collection: "nope", upsert: { key: "x", fields: {} } }; }
`
	undSpec, err := LoadProjectionFile(rt, nil, writeTempFn(t, "und.js", undSrc))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DryRunProjection(store, undSpec); err == nil {
		t.Fatal("expected undeclared collection error")
	}
}

func TestDryRunProjectionAndDiff(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	for i, id := range []string{"o1", "o2"} {
		if _, err := store.Append(ctx, "order", id, 0, []events.NewEvent{
			{Type: "OrderPlaced", Data: json.RawMessage(fmt.Sprintf(`{"customerRef":"c%d"}`, i%2))},
		}); err != nil {
			t.Fatal(err)
		}
	}

	rt := NewGojaRuntime(nil)
	src := `//@trigger projection counts on OrderPlaced
//@schema counts customerRef:text n:number
//@key customerRef
function project(event) {
	var rows = pb.query("counts", "", 500) || [];
	var n = 0;
	for (var i = 0; i < rows.length; i++) { if (rows[i].customerRef === event.data.customerRef) n = rows[i].n || 0; }
	return { upsert: { key: event.data.customerRef, fields: { n: n + 1 } } };
}
`
	// note: pb binding is inert (no reader) so the query returns nil;
	// the simulation still exercises ops collection deterministically
	spec, err := LoadProjectionFile(rt, nil, writeTempFn(t, "counts.js", src))
	if err != nil {
		t.Fatal(err)
	}

	res, err := DryRunProjection(store, spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 2 || res.Upserts != 2 || len(res.Rows["counts"]) != 2 {
		t.Fatalf("unexpected simulation: %+v", res)
	}

	// diff: identical
	diffs := DiffRows(res.Rows["counts"], []map[string]any{
		{"customerRef": "c0", "n": res.Rows["counts"]["c0"]["n"]},
		{"customerRef": "c1", "n": res.Rows["counts"]["c1"]["n"]},
	}, "customerRef")
	if len(diffs) != 0 {
		t.Fatalf("expected no diffs, got %v", diffs)
	}

	// diff: missing, changed, extra rows
	diffs = DiffRows(res.Rows["counts"], []map[string]any{
		{"customerRef": "c0", "n": 99},
		{"customerRef": "cX", "n": 1},
	}, "customerRef")
	joined := strings.Join(diffs, "\n")
	if len(diffs) != 3 || !strings.Contains(joined, "missing") ||
		!strings.Contains(joined, "field n") || !strings.Contains(joined, "not produced") {
		t.Fatalf("unexpected diffs: %v", diffs)
	}
}

// TestDryRunProjectionOverFixture: "given exactly these events, what rows
// result?" — the shape an eventmodelschema stateView scenario asserts, and a
// way to reproduce a projection defect from a hand-written log with no
// instance state involved.
func TestDryRunProjectionOverFixture(t *testing.T) {
	rt := NewGojaRuntime(nil)
	spec, err := LoadProjectionSource(rt, nil, "titles.js", `//@trigger projection titles on NoteCreated NoteArchived
//@schema note_titles noteId:text title:text
//@key noteId
function project(event) {
  if (event.type === 'NoteArchived') { return [{ delete: event.aggregateId }]; }
  return [{ upsert: { key: event.aggregateId, fields: { noteId: event.aggregateId, title: event.data.title } } }];
}
`)
	if err != nil {
		t.Fatal(err)
	}

	// the fixture carries a THIRD event type the projection does not
	// declare, so a run that ignored its trigger filter would fail here
	res, err := DryRunProjectionOver(spec, []events.Event{
		{Position: 1, AggregateID: "n1", Type: "NoteCreated", Data: json.RawMessage(`{"title":"first"}`)},
		{Position: 2, AggregateID: "n2", Type: "NoteCreated", Data: json.RawMessage(`{"title":"second"}`)},
		{Position: 3, AggregateID: "n1", Type: "NoteRenamed", Data: json.RawMessage(`{"title":"ignored"}`)},
		{Position: 4, AggregateID: "n2", Type: "NoteArchived", Data: json.RawMessage(`{}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 3 {
		t.Errorf("expected 3 matching events (NoteRenamed is not declared), got %d", res.Events)
	}
	rows := res.Rows["note_titles"]
	if len(rows) != 1 {
		t.Fatalf("expected one surviving row after the archive, got %v", rows)
	}
	if rows["n1"]["title"] != "first" {
		t.Errorf("unexpected row state: %v", rows)
	}
}

// TestDryRunProjectionOverFixtureIsolatesReads is the M7 caveat, enforced.
// A read-modify-write projection querying live collections mid-simulation
// gives an answer that depends on the real database — which is exactly how a
// fixture assertion passes vacuously. Over a fixture, `pb` reads nothing.
func TestDryRunProjectionOverFixtureIsolatesReads(t *testing.T) {
	rt := NewGojaRuntime(nil)
	rt.SetReader(stubReader{})
	spec, err := LoadProjectionSource(rt, nil, "rmw.js", `//@trigger projection rmw on Ping
//@schema pings pingId:text seen:text
//@key pingId
function project(event) {
  var prior = pb.findRecord('pings', event.aggregateId);
  return [{ upsert: { key: event.aggregateId, fields: { pingId: event.aggregateId, seen: prior ? 'live' : 'isolated' } } }];
}
`)
	if err != nil {
		t.Fatal(err)
	}
	fixture := []events.Event{{Position: 1, AggregateID: "p1", Type: "Ping", Data: json.RawMessage(`{}`)}}

	res, err := DryRunProjectionOver(spec, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows["pings"]["p1"]["seen"]; got != "isolated" {
		t.Fatalf("a fixture run must not read live collections, got %v", got)
	}

	// ...and the ordinary whole-log run still reads, because it is
	// simulating against real history where those rows genuinely exist
	live, err := rt.runProjectionWith(spec, fixture[0], false)
	if err != nil {
		t.Fatal(err)
	}
	ops, _, err := normalizeOps(live)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].fields["seen"] != "live" {
		t.Fatalf("a non-isolated run should still see the reader: %+v", ops)
	}
}

// TestDryRunProjectionOverFixtureAccumulatesIncrements is Finding 3's count/
// sum case, verified over an ISOLATED fixture: a count-derivation projection
// must never need to read its own running total back through `pb` (that
// reads nothing over a fixture, per the M7 invariant above) — the host
// itself accumulates an increment op's delta across the fixture, the same
// way it accumulates a plain upsert's fields. Two increments and one
// decrement on the same row must net to 1, entirely from op deltas, with no
// reader installed at all.
func TestDryRunProjectionOverFixtureAccumulatesIncrements(t *testing.T) {
	rt := NewGojaRuntime(nil)
	spec, err := LoadProjectionSource(rt, nil, "projects.js", `//@trigger projection projects on StaffAssignedToProject StaffUnassignedFromProject
//@schema projects projectId:text staffCount:number
//@key projectId
function project(event) {
  switch (event.type) {
    case 'StaffAssignedToProject':
      return { increment: { key: event.data.projectId, field: 'staffCount', delta: 1 } };
    case 'StaffUnassignedFromProject':
      return { increment: { key: event.data.projectId, field: 'staffCount', delta: -1 } };
    default:
      return;
  }
}
`)
	if err != nil {
		t.Fatal(err)
	}

	fixture := []events.Event{
		{Position: 1, AggregateID: "a1", Type: "StaffAssignedToProject", Data: json.RawMessage(`{"projectId":"p1"}`)},
		{Position: 2, AggregateID: "a2", Type: "StaffAssignedToProject", Data: json.RawMessage(`{"projectId":"p1"}`)},
		{Position: 3, AggregateID: "a1", Type: "StaffUnassignedFromProject", Data: json.RawMessage(`{"projectId":"p1"}`)},
	}
	res, err := DryRunProjectionOver(spec, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows["projects"]["p1"]["staffCount"]; got != float64(1) {
		t.Fatalf("expected staffCount to net to 1 across the fixture with no reads, got %v (rows: %v)", got, res.Rows)
	}
}

// TestDryRunProjectionOverFixtureAccumulatesGroupByBump is Finding 4's
// groupBy derivation (schema 2.3.0) proven under the SAME isolation
// TestDryRunProjectionOverFixtureIsolatesReads exercises for a plain
// count/sum: two contributing events for the same group key must
// accumulate onto one nested entry, and a different group key must land in
// its own entry, purely from the ops returned — no pb.query read-back,
// which emschema.Verify's stateView scenario checking depends on to avoid a
// vacuous pass.
func TestDryRunProjectionOverFixtureAccumulatesGroupByBump(t *testing.T) {
	rt := NewGojaRuntime(nil)
	spec, err := LoadProjectionSource(rt, nil, "payrollPeriods.js", `//@trigger projection payrollPeriods on TimeLogged
//@schema payrollPeriods periodId:text staffTotals:json
//@key periodId
function project(event) {
  return { groupByBump: {
    key: event.data.periodId,
    field: 'staffTotals',
    groupField: 'staffId',
    groupValue: event.data.staffId,
    subfield: 'outOfHoursHours',
    delta: event.data.hours,
  } };
}
`)
	if err != nil {
		t.Fatal(err)
	}

	fixture := []events.Event{
		{Position: 1, AggregateID: "t1", Type: "TimeLogged", Data: json.RawMessage(`{"periodId":"pp1","staffId":"s1","hours":3}`)},
		{Position: 2, AggregateID: "t2", Type: "TimeLogged", Data: json.RawMessage(`{"periodId":"pp1","staffId":"s1","hours":2}`)},
		{Position: 3, AggregateID: "t3", Type: "TimeLogged", Data: json.RawMessage(`{"periodId":"pp1","staffId":"s2","hours":4}`)},
	}
	res, err := DryRunProjectionOver(spec, fixture)
	if err != nil {
		t.Fatal(err)
	}
	rows, ok := res.Rows["payrollPeriods"]["pp1"]["staffTotals"].([]map[string]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("expected 2 staffTotals entries, got %#v (rows: %v)", res.Rows["payrollPeriods"]["pp1"]["staffTotals"], res.Rows)
	}
	byStaff := map[string]any{}
	for _, r := range rows {
		byStaff[fmt.Sprint(r["staffId"])] = r["outOfHoursHours"]
	}
	if byStaff["s1"] != float64(5) {
		t.Errorf("expected s1 outOfHoursHours=5 (3+2 accumulated), got %v", byStaff["s1"])
	}
	if byStaff["s2"] != float64(4) {
		t.Errorf("expected s2 outOfHoursHours=4 (its own entry), got %v", byStaff["s2"])
	}
}

// stubReader stands in for live collections.
type stubReader struct{}

func (stubReader) FindRecord(collection, id string) (map[string]any, error) {
	return map[string]any{"id": id}, nil
}
func (stubReader) Query(collection, filter string, limit int) ([]map[string]any, error) {
	return nil, nil
}
