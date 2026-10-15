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
		"examples/refusals.json": {
			"/commands/claim-entry-for-invoice/refuseWhen",
			"/commands/update-entry/refuseWhen",
		},
		"examples/conditional-outcomes.json": {"/commands/log-entry/refuseWhen", "/slices/0/outcomes"},
		"examples/state-ownership.json":      {"/commands/update-expense/requiredOwnership/stateField"},
		"examples/rows-per-list-item.json":   {"/readModels/invoice-parts/rowsPerListItem"},
		"examples/hand-projection.json":      {"/readModels/late-orders/projection"},
		"examples/repeated-outcomes.json":    {"/readModels/payments/rowKeyField", "/slices/0/outcomes"},
		"examples/order-fulfillment.json": {
			"/dataSubjects",
			"/ingresses",
			"/partitioning",
			"/slices/5/scenarios/0/given/1/elapsed",
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
	_, err := Load(fixture(t, "examples/refusals.json"))
	if err == nil {
		t.Fatal("refusals.json loaded")
	}
	msg := err.Error()
	for _, want := range []string{"refuseWhen", "3.10.0", "/commands/update-entry/refuseWhen", "nothing was generated"} {
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

// A derivation kind added later (3.16.0's sumOfItems and byEvent) carries a
// meaning the projection generator would otherwise skip without a word.
func TestUnknownDerivationKindIsRefused(t *testing.T) {
	p := writeDoc(t, docHead+`
		"readModels":{"r":{"name":"R","fields":[
			{"name":"status","type":"string","derivation":{"kind":"byEvent","values":{"e":"done"}}}]}},
		"slices":[]}`)
	wantRefused(t, refusedAt(t, p),
		"/readModels/r/fields/0/derivation/kind",
		"/readModels/r/fields/0/derivation/values")
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
// an unknown key in a stateView scenario's `when` and a 3.5.0 `elapsed` step
// in `given` must still be seen.
func TestScenarioShapesAreChecked(t *testing.T) {
	p := writeDoc(t, docHead+`
		"events":{"e":{"name":"E","swimlaneId":"s"}},
		"readModels":{"r":{"name":"R","builtFromEventIds":["e"]}},
		"screens":{"sc":{"name":"Sc"}},
		"slices":[{"id":"v","name":"V","pattern":"stateView","swimlaneId":"s","status":"planned",
			"screenId":"sc","readModelId":"r","scenarios":[
			{"id":"x","name":"X","kind":"stateView","given":[{"eventId":"e"},{"elapsed":"P1D"}],
			 "when":{"readModelId":"r","impersonating":{"subjectId":"u1"}},
			 "then":{"result":{}}}]}]}`)
	wantRefused(t, refusedAt(t, p),
		"/slices/0/scenarios/0/given/1/elapsed",
		"/slices/0/scenarios/0/when/impersonating")
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

// A split document is checked file by file: the manifest (3.2.0 and 3.7.0
// put dataSubjects and partitioning inline there) and every slice file.
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
		"dataSubjects":{"erasure":{"self":true}},
		"registries":{"commands":"commands.json"},
		"slices":["slices/a.json"]}`)
	write("commands.json", `{"c":{"name":"C","refuseWhen":[]}}`)
	write("slices/a.json", `{"id":"a","name":"A","pattern":"stateChange","swimlaneId":"s","status":"planned",
		"commandId":"c","eventIds":[],"outcomes":[[]],"scenarios":[]}`)
	wantRefused(t, refusedAt(t, dir),
		"manifest.json/dataSubjects",
		"commands.json/c/refuseWhen",
		"slices/a.json/outcomes")
}
