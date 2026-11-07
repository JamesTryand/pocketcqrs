package emschema

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// refusedAt loads path and returns the JSON pointers the importer refused.
// Loading must fail with an *UnsupportedError, never succeed quietly.
func refusedAt(t *testing.T, path string) []string {
	t.Helper()
	_, err := Load(path)
	if err == nil {
		t.Fatalf("%s loaded, but it declares features this importer cannot apply", filepath.Base(path))
	}
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("%s: want an UnsupportedError naming the features, got: %v", filepath.Base(path), err)
	}
	var at []string
	for _, f := range unsupported.Features {
		at = append(at, f.Pointer)
	}
	return at
}

func wantRefused(t *testing.T, got []string, want ...string) {
	t.Helper()
	have := map[string]bool{}
	for _, g := range got {
		have[g] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("not refused: %s (refused: %v)", w, got)
		}
	}
}

// Each worked example upstream added for a feature (3.2.0-3.17.0) must be
// refused, and refused at the place it declares the feature: dropped
// silently, the generated code would lack its access rules, refusals and
// removals while looking complete.
func TestNewerSchemaFeaturesAreRefusedWhereDeclared(t *testing.T) {
	cases := map[string][]string{
		"examples/order-fulfillment.json": {
			"/partitioning",
		},
	}
	for rel, want := range cases {
		t.Run(rel, func(t *testing.T) {
			wantRefused(t, refusedAt(t, fixture(t, rel)), want...)
		})
	}
}

// Every refusal says what the feature is and which schema version added it,
// so the person importing knows what was refused and why.
func TestRefusalNamesTheFeature(t *testing.T) {
	_, err := Load(fixture(t, "examples/order-fulfillment.json"))
	if err == nil {
		t.Fatal("order-fulfillment.json loaded")
	}
	msg := err.Error()
	for _, want := range []string{"partitioning", "3.7.0", "/partitioning", "nothing was generated"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not mention %q:\n%s", want, msg)
		}
	}
}

func TestMinimalNewDocumentStillLoads(t *testing.T) {
	if _, err := Load(fixture(t, "examples/minimal.json")); err != nil {
		t.Fatal(err)
	}
}

// writeDoc writes body as a document in a temp dir and returns its path.
func writeDoc(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "doc.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const docHead = `{"eventModelingSchemaVersion":"3.17.0","id":"d","name":"D",
	"swimlanes":[{"id":"s","name":"S","kind":"team"}],`

// The check is an allow-list: a key this importer does not read is refused
// whether or not anyone has heard of it, so a feature added upstream after
// 3.17.0 fails closed too, at any depth.
func TestUnknownKeysAreRefusedAtAnyDepth(t *testing.T) {
	p := writeDoc(t, docHead+`
		"readModels":{"r":{"name":"R","fields":[
			{"name":"lines","type":"object","cardinality":"list","subfields":[
				{"name":"x","type":"string","futureThing":true}]}]}},
		"slices":[]}`)
	wantRefused(t, refusedAt(t, p), "/readModels/r/fields/0/subfields/0/futureThing")
}

// A derivation kind this importer doesn't know carries a
// meaning the projection generator would otherwise skip without a word.
func TestUnknownDerivationKindIsRefused(t *testing.T) {
	p := writeDoc(t, docHead+`
		"readModels":{"r":{"name":"R","fields":[
			{"name":"status","type":"string","derivation":{"kind":"median","of":"amount"}}]}},
		"slices":[]}`)
	wantRefused(t, refusedAt(t, p),
		"/readModels/r/fields/0/derivation/kind",
		"/readModels/r/fields/0/derivation/of")
}

// A filter kind this importer does not know is refused the same way.
func TestUnknownFilterKindIsRefused(t *testing.T) {
	p := writeDoc(t, docHead+`
		"readModels":{"r":{"name":"R","fields":[{"name":"d","type":"date"}],
			"filters":[{"param":"p","field":"d","kind":"between"}]}},
		"slices":[]}`)
	wantRefused(t, refusedAt(t, p), "/readModels/r/filters/0/kind")
}

// Scenario `when` and `then` are kind-dependent and were raw JSON here, so
// an unknown key in a stateView scenario's `when` must still be seen.
func TestScenarioShapesAreChecked(t *testing.T) {
	p := writeDoc(t, docHead+`
		"events":{"e":{"name":"E","swimlaneId":"s"}},
		"readModels":{"r":{"name":"R","builtFromEventIds":["e"]}},
		"screens":{"sc":{"name":"Sc"}},
		"slices":[{"id":"v","name":"V","pattern":"stateView","swimlaneId":"s","status":"planned",
			"screenId":"sc","readModelId":"r","scenarios":[
			{"id":"x","name":"X","kind":"stateView","given":[{"eventId":"e"}],
			 "when":{"readModelId":"r","impersonating":{"subjectId":"u1"}},
			 "then":{"result":{}}}]}]}`)
	wantRefused(t, refusedAt(t, p), "/slices/0/scenarios/0/when/impersonating")
}

// Example data (event and command payloads, query params, expected results)
// is the domain's own data, not schema, so any key is fine there.
func TestExampleDataIsNotChecked(t *testing.T) {
	p := writeDoc(t, docHead+`
		"events":{"e":{"name":"E","swimlaneId":"s","fields":[{"name":"anything","type":"string"}]}},
		"commands":{"c":{"name":"C"}},
		"screens":{"sc":{"name":"Sc"}},
		"slices":[{"id":"w","name":"W","pattern":"stateChange","swimlaneId":"s","status":"planned",
			"screenId":"sc","commandId":"c","eventIds":["e"],"scenarios":[
			{"id":"x","name":"X","kind":"stateChange","given":[{"eventId":"e","data":{"anything":"a"}}],
			 "when":{"commandId":"c","data":{"anything":"b"}},
			 "then":{"events":[{"eventId":"e","data":{"anything":"c"}}]}}]}]}`)
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
}

// A split document is checked file by file: the manifest and every slice
// file. (Its inline dataSubjects is supported: see
// TestSplitDocumentCarriesDataSubjects.)
func TestSplitDocumentIsCheckedToo(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", `{"eventModelingSchemaVersion":"3.17.0","id":"d","name":"D",
		"swimlanes":[{"id":"s","name":"S","kind":"team"}],
		"registries":{"commands":"commands.json"},
		"slices":["slices/a.json"]}`)
	write("commands.json", `{"c":{"name":"C","partitioned":false}}`)
	write("slices/a.json", `{"id":"a","name":"A","pattern":"stateChange","swimlaneId":"s","status":"planned",
		"commandId":"c","eventIds":[],"partitionFrom":"shopId","scenarios":[]}`)
	wantRefused(t, refusedAt(t, dir),
		"commands.json/c/partitioned",
		"slices/a.json/partitionFrom")
}

// Boundary edges (ingresses, delay/schedule, effect, elapsed steps) are
// accepted, not built, and every one is listed: nothing is dropped silently.
func TestBoundaryEdgesAreAcceptedAndListed(t *testing.T) {
	p := writeDoc(t, docHead+`
		"ingresses":{"hook":{"name":"Hook","kind":"webhook"}},
		"events":{"e":{"name":"E","swimlaneId":"s"},"f":{"name":"F","swimlaneId":"s"}},
		"commands":{"c":{"name":"C"}},
		"automations":{"a":{"name":"A"}},
		"slices":[
		 {"id":"w","name":"W","pattern":"stateChange","swimlaneId":"s","status":"planned",
		  "commandId":"c","eventIds":["e"],"ingressId":"hook","scenarios":[
		   {"id":"keep","name":"Keep","kind":"stateChange","given":[],
		    "when":{"commandId":"c"},"then":{"events":[{"eventId":"e"}]}},
		   {"id":"timed","name":"Timed","kind":"stateChange","given":[{"eventId":"e"},{"elapsed":"P1D"}],
		    "when":{"commandId":"c"},"then":{"events":[{"eventId":"e"}]}}]},
		 {"id":"t","name":"T","pattern":"automation","swimlaneId":"s","status":"planned",
		  "automationId":"a","triggerEventIds":["e"],"resultEventIds":["f"],
		  "schedule":{"cron":"0 9 * * *"},"effect":{"kind":"email"},"scenarios":[]}]}`)
	doc, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(doc.LeftOut, "; ")
	for _, want := range []string{`ingress "hook"`, `slice "w" is started by ingress "hook"`,
		`slice "t" is started by a schedule`, `slice "t" has an effect`, `scenario "timed" of slice "w"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("the list does not mention %q: %s", want, joined)
		}
	}
	if len(doc.LeftOut) != 5 {
		t.Errorf("want 5 left-out edges, got %d: %s", len(doc.LeftOut), joined)
	}
	if n := len(doc.Slices[0].Scenarios); n != 1 || doc.Slices[0].Scenarios[0].ID != "keep" {
		t.Errorf("the scenario that waits for time must be removed and the other kept, got %d scenario(s)", n)
	}
	if got := Lint(doc).LeftOut; len(got) != 5 {
		t.Errorf("the report must carry the list, got %v", got)
	}
}

// Accepting edges does not relax anything else: an edge beside an unrelated
// unsupported feature is still refused, and the refusal names only the latter.
func TestBoundaryEdgesDoNotHideOtherRefusals(t *testing.T) {
	p := writeDoc(t, docHead+`
		"ingresses":{"hook":{"name":"Hook"}},
		"partitioning":{"key":"shopId"},
		"slices":[]}`)
	got := refusedAt(t, p)
	wantRefused(t, got, "/partitioning")
	for _, g := range got {
		if strings.HasPrefix(g, "/ingresses") {
			t.Errorf("an ingress must not be refused: %v", got)
		}
	}
}

// A document with all three kinds of edge maps to a domain model: the slice
// an ingress starts needs no screen, the automation a schedule starts needs
// no trigger events and gets no reactor (it would fire at once and ignore the
// wait), and the report carries the list.
func TestDocumentWithBoundaryEdgesMaps(t *testing.T) {
	p := writeDoc(t, docHead+`
		"ingresses":{"hook":{"name":"Hook"}},
		"events":{"e":{"name":"E","swimlaneId":"s","aggregate":"thing","fields":[{"name":"n","type":"string"}]},
		          "f":{"name":"F","swimlaneId":"s","aggregate":"thing","fields":[{"name":"n","type":"string"}]}},
		"commands":{"c":{"name":"C","aggregate":"thing","fields":[{"name":"n","type":"string"}]},
		            "d":{"name":"D","aggregate":"thing","fields":[{"name":"n","type":"string"}]}},
		"automations":{"a":{"name":"A"}},
		"slices":[
		 {"id":"w","name":"W","pattern":"stateChange","swimlaneId":"s","status":"planned",
		  "commandId":"c","eventIds":["e"],"ingressId":"hook","scenarios":[]},
		 {"id":"t","name":"T","pattern":"automation","swimlaneId":"s","status":"planned",
		  "automationId":"a","commandId":"d","resultEventIds":["f"],
		  "schedule":{"cron":"0 9 * * *"},"scenarios":[]}]}`)
	doc, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := Map(doc, Options{})
	if err != nil {
		t.Fatalf("a document with boundary edges must map: %v\n%v", err, mapped.Report.Errors)
	}
	for _, d := range mapped.Domains {
		if len(d.Reactors) != 0 {
			t.Errorf("the schedule-started automation must get no reactor, got %v", d.Reactors)
		}
	}
	if len(mapped.Report.LeftOut) != 3 {
		t.Errorf("want 3 edges in the report, got %v", mapped.Report.LeftOut)
	}
}
