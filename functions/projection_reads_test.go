package functions

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamestryand/pocketcqrs/events"
)

// P1 (James, 2026-10-16): a JS projection's `pb` reads only the collections
// the projection itself declares. Its own rows are a fold of the events it
// has seen, so reading them back stays deterministic; another read model is
// someone else's fold, at whatever point that fold had reached.

const readsOtherProjection = `//@trigger projection tally on Ping
//@schema tallies pingId:text seen:number
//@key pingId
function project(event) {
  var other = pb.query('tasks', '', 10);
  return { upsert: { key: event.aggregateId, fields: { seen: 1 } } };
}
`

const readModifyWrite = `//@trigger projection tally on Ping
//@schema tallies pingId:text seen:number
//@key pingId
function project(event) {
  var rows = pb.query('tallies', "pingId = '" + event.aggregateId + "'", 1) || [];
  var seen = rows.length ? rows[0].seen : 0;
  return { upsert: { key: event.aggregateId, fields: { seen: seen + 1 } } };
}
`

func pings(ids ...string) []events.Event {
	out := make([]events.Event, 0, len(ids))
	for i, id := range ids {
		out = append(out, events.Event{Position: int64(i + 1), Aggregate: "ping", AggregateID: id, Type: "Ping",
			Data: json.RawMessage(`{}`)})
	}
	return out
}

func TestALiveProjectionCannotReadAnotherCollection(t *testing.T) {
	rt := NewGojaRuntime(nil)
	rt.SetReader(stubReader{})
	spec, err := LoadProjectionSource(rt, nil, "tally.js", readsOtherProjection)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rt.runProjection(spec, pings("p1")[0])
	if err == nil || !strings.Contains(err.Error(), `"tasks"`) || !strings.Contains(err.Error(), "tallies") {
		t.Fatalf("a read of another collection must fail, naming it and what may be read: %v", err)
	}
}

func TestALiveProjectionReadsItsOwnCollection(t *testing.T) {
	rt := NewGojaRuntime(nil)
	rt.SetReader(stubReader{})
	spec, err := LoadProjectionSource(rt, nil, "rmw.js", `//@trigger projection rmw on Ping
//@schema pings pingId:text seen:text
//@key pingId
function project(event) {
  var prior = pb.findRecord('pings', event.aggregateId);
  return { upsert: { key: event.aggregateId, fields: { seen: prior ? 'read' : 'not read' } } };
}
`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := rt.runProjection(spec, pings("p1")[0])
	if err != nil {
		t.Fatal(err)
	}
	ops, _, err := normalizeOps(got)
	if err != nil || len(ops) != 1 || ops[0].fields["seen"] != "read" {
		t.Fatalf("its own collection must stay readable: %+v %v", ops, err)
	}
}

// A fixture run serves the rows it has simulated so far, so a read-modify-
// write projection verifies as it runs live, not against an empty table.
func TestAFixtureRunReadsTheRowsItHasSimulated(t *testing.T) {
	rt := NewGojaRuntime(nil)
	rt.SetReader(stubReader{})
	spec, err := LoadProjectionSource(rt, nil, "tally.js", readModifyWrite)
	if err != nil {
		t.Fatal(err)
	}
	res, err := DryRunProjectionOver(spec, pings("p1", "p1", "p2"))
	if err != nil {
		t.Fatal(err)
	}
	rows := res.Rows["tallies"]
	if fmt.Sprint(rows["p1"]["seen"]) != "2" || fmt.Sprint(rows["p2"]["seen"]) != "1" {
		t.Fatalf("expected p1 seen twice and p2 once, got %v", rows)
	}
}

// The whole-log dry run does the same: it simulates history, so it reads the
// simulated rows rather than today's live ones (which made a read-modify-
// write projection never --diff clean).
func TestAWholeLogDryRunReadsTheRowsItHasSimulated(t *testing.T) {
	store, err := events.Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, id := range []string{"p1", "p1"} {
		stream, _ := store.LoadStream(t.Context(), "ping", id)
		if _, err := store.Append(t.Context(), "ping", id, int64(len(stream)),
			[]events.NewEvent{{Type: "Ping", Data: json.RawMessage(`{}`)}}); err != nil {
			t.Fatal(err)
		}
	}
	rt := NewGojaRuntime(nil)
	rt.SetReader(liveTallies{})
	spec, err := LoadProjectionSource(rt, nil, "tally.js", readModifyWrite)
	if err != nil {
		t.Fatal(err)
	}
	res, err := DryRunProjection(store, spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows["tallies"]["p1"]["seen"]; fmt.Sprint(got) != "2" {
		t.Fatalf("expected the simulation's own count, 2, not the live row's: %v", got)
	}
}

func TestAFixtureRunCannotReadAnotherCollection(t *testing.T) {
	rt := NewGojaRuntime(nil)
	spec, err := LoadProjectionSource(rt, nil, "tally.js", readsOtherProjection)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DryRunProjectionOver(spec, pings("p1")); err == nil || !strings.Contains(err.Error(), `"tasks"`) {
		t.Fatalf("a fixture run must refuse a read of another collection: %v", err)
	}
}

// A filter the simulation can't evaluate is refused, not answered with
// nothing: an empty answer is how a scenario passes vacuously.
func TestAFixtureRunRefusesAFilterItCannotSimulate(t *testing.T) {
	rt := NewGojaRuntime(nil)
	spec, err := LoadProjectionSource(rt, nil, "tally.js", strings.Replace(readModifyWrite,
		`"pingId = '" + event.aggregateId + "'"`, `"seen > 0"`, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DryRunProjectionOver(spec, pings("p1")); err == nil || !strings.Contains(err.Error(), "seen > 0") {
		t.Fatalf("an unsimulated filter must be refused, naming it: %v", err)
	}
}

func TestTheSimulatedFilterMatchesTermsJoinedByAnd(t *testing.T) {
	rows := map[string]map[string]any{
		"a": {"k": "a", "n": float64(1), "on": true, "who": "it's"},
		"b": {"k": "b", "n": float64(2), "on": false, "who": "x"},
		"c": {"k": "c", "n": float64(1), "on": false, "who": nil},
		"d": {"k": "d", "n": float64(3), "on": true, "who": ""}, // PocketBase's null text
	}
	for filter, want := range map[string]string{
		``:                     "abcd",
		`n = 1`:                "ac",
		`n = 1 && on = false`:  "c",
		`k = "b"`:              "b",
		`k != 'b'`:             "acd",
		`who = 'it\'s'`:        "a",
		`who = null`:           "cd",
		`on = true && k = 'a'`: "a",
		`k = 'a && b'`:         "",
		` k  =  'c' `:          "c",
		`n = 1 && n = 2`:       "",
	} {
		got, err := simulatedQuery(rows, filter)
		if err != nil {
			t.Errorf("%q: %v", filter, err)
			continue
		}
		keys := ""
		for _, r := range got {
			keys += r["k"].(string)
		}
		if keys != want {
			t.Errorf("%q matched %q, want %q", filter, keys, want)
		}
	}
	for _, bad := range []string{`n > 1`, `k ~ 'a'`, `n = 1 || n = 2`, `n = 1 on = true`, `k = && n = 1`, `k = 'a`, `(n = 1)`, `n =`, `= 1`} {
		if _, err := simulatedQuery(rows, bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

type liveTallies struct{}

func (liveTallies) FindRecord(string, string) (map[string]any, error) { return nil, nil }
func (liveTallies) Query(collection, filter string, limit int) ([]map[string]any, error) {
	return []map[string]any{{"pingId": "p1", "seen": float64(40)}}, nil
}

// What a simulated read returns is the script's to change: a copy, so the
// simulation's rows move only by the ops the projection returns. The limit
// applies as AppReader's does.
func TestASimulatedReadIsACopyWithinTheLimit(t *testing.T) {
	spec := &ProjectionSpec{Name: "tally", Schemas: []*SchemaSpec{{Collection: "tallies", Key: "pingId"}}}
	rows := map[string]map[string]map[string]any{"tallies": {
		"p1": {"pingId": "p1", "seen": float64(1)},
		"p2": {"pingId": "p2", "seen": float64(2)},
	}}
	r := simulatedReader{spec: spec, rows: rows}
	got, err := r.Query("tallies", "", 1)
	if err != nil || len(got) != 1 || got[0]["pingId"] != "p1" {
		t.Fatalf("limit 1: %v %v", got, err)
	}
	got[0]["seen"] = float64(99)
	if rows["tallies"]["p1"]["seen"] != float64(1) {
		t.Fatal("changing a read row changed the simulation")
	}
	if got, _ := r.Query("tallies", "", 0); len(got) != 2 {
		t.Fatalf("limit 0 reads up to 100: %v", got)
	}
}
