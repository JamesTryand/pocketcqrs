package emschema

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostDir writes a domain's generated JS into a functions directory, then
// the host's own files over it: what a host's pb_functions holds.
func hostDir(t *testing.T, mapped *Mapped, own map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for _, d := range mapped.Domains {
		files, err := d.Generate()
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f.Name, ".example") {
				continue
			}
			if err := os.WriteFile(filepath.Join(dir, f.Name), []byte(f.Source), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, src := range own {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func hostResults(t *testing.T, rel string, own map[string]string) map[string]ScenarioResult {
	t.Helper()
	doc, err := Load(fixture(t, rel))
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	results, err := VerifyWith(doc, mapped, t.TempDir(), VerifyOptions{Host: hostDir(t, mapped, own)})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]ScenarioResult{}
	for _, r := range results {
		out[r.ScenarioID] = r
	}
	return out
}

// Host mode runs the host's own hooks: the over-threshold refund, HAND against
// the generated code, passes with a hook that flags it and fails with one
// that doesn't. Everything else still passes.
func TestHostModeRunsTheHostsHooks(t *testing.T) {
	right := hostResults(t, "examples/conditional-outcomes.json", map[string]string{
		"refund.hand.js": "function RequestRefundOutcome0(state, command) { return command.payload.amount > 500; }\n",
	})
	for id, r := range right {
		if !r.Passed || r.Hand {
			t.Errorf("%s: passed=%v hand=%v: %s", id, r.Passed, r.Hand, r.Detail)
		}
	}
	wrong := hostResults(t, "examples/conditional-outcomes.json", map[string]string{
		"refund.hand.js": "function RequestRefundOutcome0(state, command) { return false; }\n",
	})
	if r := wrong["refund-over-threshold"]; r.Passed || r.Hand {
		t.Errorf("a hook that never flags: passed=%v hand=%v: %s", r.Passed, r.Hand, r.Detail)
	}
}

// Host mode runs the host's own decider, not the generated one: a decider
// that refuses everything fails the scenarios the generated one passes.
func TestHostModeRunsTheHostsDecider(t *testing.T) {
	results := hostResults(t, "examples/conditional-outcomes.json", map[string]string{
		"booking.js":     "//@trigger decider booking\n//@handles BookingMade BookingFlagged\n//@commands MakeBooking\nfunction initialState() { return {}; }\nfunction decide(command, state) { throw new Error('closed'); }\nfunction evolve(state, event) { return state; }\n",
		"refund.hand.js": "function RequestRefundOutcome0(state, command) { return false; }\n",
	})
	failed := 0
	for _, r := range results {
		if !r.Passed && strings.Contains(r.Detail, "closed") {
			failed++
		}
	}
	if failed == 0 {
		for _, r := range results {
			t.Logf("%s: %s", r.ScenarioID, r.Detail)
		}
		t.Fatal("the host's refusing decider wasn't run")
	}
}

// A file the host doesn't have is a skip that says so, not the generated
// code run in its place.
func TestHostModeWithoutTheFileSkips(t *testing.T) {
	doc, err := Load(fixture(t, "examples/conditional-outcomes.json"))
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	results, err := VerifyWith(doc, mapped, t.TempDir(), VerifyOptions{Host: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if !r.Skipped || !strings.Contains(r.Detail, "the host has no") {
			t.Errorf("%s: skipped=%v: %s", r.ScenarioID, r.Skipped, r.Detail)
		}
	}
}

// A hand projection (schema 3.17.0) runs from the host's own file: its view
// scenario passes instead of being HAND.
const handViewDoc = `{"eventModelingSchemaVersion":"3.17.0","id":"h","name":"H",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],
	"events":{"placed":{"name":"Placed","swimlaneId":"s","aggregate":"order","fields":[{"name":"orderId","type":"string","idAttribute":true},{"name":"total","type":"decimal"}]}},
	"commands":{"place":{"name":"Place","aggregate":"order","fields":[{"name":"orderId","type":"string","idAttribute":true},{"name":"total","type":"decimal"}]}},
	"readModels":{"big-orders":{"name":"Big Orders","builtFromEventIds":["placed"],"projection":"hand",
		"fields":[{"name":"orderId","type":"string","idAttribute":true},{"name":"total","type":"decimal"}]}},
	"screens":{"sc":{"name":"Sc"}},
	"slices":[
		{"id":"p","name":"P","pattern":"stateChange","swimlaneId":"s","status":"planned","screenId":"sc","commandId":"place","eventIds":["placed"],"scenarios":[]},
		{"id":"v","name":"V","pattern":"stateView","swimlaneId":"s","status":"planned","screenId":"sc","readModelId":"big-orders","scenarios":[
			{"id":"only-big","name":"only big","kind":"stateView",
			 "given":[{"eventId":"placed","data":{"orderId":"o1","total":10}},{"eventId":"placed","data":{"orderId":"o2","total":900}}],
			 "when":{"readModelId":"big-orders"},"then":{"result":{"entries":[{"orderId":"o2","total":900}]}}}]}]}`

func TestHostModeRunsAHandProjection(t *testing.T) {
	doc, err := Load(writeDoc(t, handViewDoc))
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatal(err)
	}
	collection := mapped.Domains[0].ReadModels[0].Collection
	projection := "//@trigger projection " + collection + " on Placed\n//@schema " + collection + " orderId:text total:number\n//@key orderId\n" +
		"function project(event) {\n  if (event.data.total < 500) { return; }\n  return { upsert: { key: event.data.orderId, fields: { total: event.data.total } } };\n}\n"
	results, err := VerifyWith(doc, mapped, t.TempDir(), VerifyOptions{Host: hostDir(t, mapped, map[string]string{collection + ".js": projection})})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.ScenarioID == "only-big" && (!r.Passed || r.Hand) {
			t.Fatalf("passed=%v hand=%v: %s", r.Passed, r.Hand, r.Detail)
		}
	}
}
