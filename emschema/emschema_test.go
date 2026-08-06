package emschema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtures live in the repo, copied from eventmodelschema@1b4a01c, so these
// tests run against the REAL format rather than against a shape this
// package invented for itself.
func fixture(t *testing.T, rel string) string {
	t.Helper()
	return filepath.Join("..", "testdata", "eventmodelschema", rel)
}

func TestLoadWorkedExample(t *testing.T) {
	doc, err := Load(fixture(t, "examples/order-fulfillment.json"))
	if err != nil {
		t.Fatal(err)
	}
	if doc.ID != "order-fulfillment" || len(doc.Slices) != 4 {
		t.Fatalf("unexpected document: id=%q slices=%d", doc.ID, len(doc.Slices))
	}
	if len(doc.Swimlanes) != 3 || len(doc.Events) != 3 || len(doc.Commands) != 3 {
		t.Fatalf("registries not read: %d swimlanes, %d events, %d commands",
			len(doc.Swimlanes), len(doc.Events), len(doc.Commands))
	}
	// the typed-fields layer, which v1 did not have
	placed := doc.Events["order-placed"]
	if placed.Aggregate != "Order" {
		t.Errorf("the aggregate tag was not read: %+v", placed)
	}
	var idField, listField *Field
	for i := range placed.Fields {
		if placed.Fields[i].IDAttribute {
			idField = &placed.Fields[i]
		}
		if placed.Fields[i].Cardinality == "list" {
			listField = &placed.Fields[i]
		}
	}
	if idField == nil || idField.Name != "orderId" {
		t.Errorf("idAttribute not read: %+v", placed.Fields)
	}
	if listField == nil || len(listField.Subfields) != 2 {
		t.Errorf("list field with subfields not read: %+v", placed.Fields)
	}

	// the boundary elements the worked example deliberately leaves untagged
	if doc.Events["shipment-notified"].Aggregate != "" {
		t.Error("shipment-notified is meant to be untagged; the fixture has drifted")
	}
}

// TestJoinMatchesSingleDocument proves the Go reimplementation of join.js is
// faithful: the split layout and the single document are the same model.
func TestJoinMatchesSingleDocument(t *testing.T) {
	single, err := Load(fixture(t, "examples/order-fulfillment.json"))
	if err != nil {
		t.Fatal(err)
	}
	joined, err := Load(fixture(t, "examples/order-fulfillment-split"))
	if err != nil {
		t.Fatal(err)
	}
	// $schema is re-stamped by the join, so compare everything else
	single.Schema, joined.Schema = "", ""
	a, _ := json.Marshal(single)
	b, _ := json.Marshal(joined)
	if string(a) != string(b) {
		t.Fatalf("joining the split layout did not reproduce the document:\n single: %s\n joined: %s", a, b)
	}
}

func TestMinimalDocumentLintsClean(t *testing.T) {
	doc, err := Load(fixture(t, "examples/minimal.json"))
	if err != nil {
		t.Fatal(err)
	}
	rep := Lint(doc)
	if err := rep.Err(); err != nil {
		t.Fatalf("the minimal example must lint clean: %v", err)
	}
}

func TestWorkedExampleLintsClean(t *testing.T) {
	doc, err := Load(fixture(t, "examples/order-fulfillment.json"))
	if err != nil {
		t.Fatal(err)
	}
	rep := Lint(doc)
	if err := rep.Err(); err != nil {
		t.Fatalf("the worked example must lint clean: %v", err)
	}
	// ...and the lossy list is not empty, because this document exercises
	// every notation. A silent clean report here would mean the enumeration
	// never ran.
	if len(rep.Lossy) == 0 {
		t.Error("a document with chapters, hotspots, screens and pii must report what is lost")
	}
	var sawPII bool
	for _, l := range rep.Lossy {
		if strings.Contains(l, "pii") && strings.Contains(l, "customerEmail") {
			sawPII = true
		}
	}
	if !sawPII {
		t.Errorf("the pii flag on customerEmail must be named, not just counted: %v", rep.Lossy)
	}
}

// TestLintCatchesDanglingReferences is the whole reason this layer exists:
// the source schema is structural only and verifies no id resolves.
func TestLintCatchesDanglingReferences(t *testing.T) {
	doc, err := Load(fixture(t, "examples/order-fulfillment.json"))
	if err != nil {
		t.Fatal(err)
	}
	// break one reference of each kind
	for i := range doc.Slices {
		if doc.Slices[i].Pattern == PatternStateChange {
			doc.Slices[i].CommandID = "no-such-command"
			doc.Slices[i].EventIDs = []string{"no-such-event"}
		}
		if doc.Slices[i].Pattern == PatternStateView {
			doc.Slices[i].ReadModelID = "no-such-read-model"
		}
	}
	doc.Events["order-placed"] = Event{Name: "Order Placed", SwimlaneID: "no-such-lane"}

	rep := Lint(doc)
	err = rep.Err()
	if err == nil {
		t.Fatal("dangling references must be refused")
	}
	for _, want := range []string{"no-such-command", "no-such-event", "no-such-read-model", "no-such-lane"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("lint did not report %q: %v", want, err)
		}
	}
}

// TestLintRefusesTranslation: v1's fourth pattern was removed in v2, and the
// version string cannot be used to tell the generations apart — so the shape
// is what gives it away, and the refusal has to explain the reversal.
func TestLintRefusesTranslation(t *testing.T) {
	doc := &Document{
		EventModelingSchemaVersion: "1.0.0", // exactly what a v2 document says too
		Swimlanes:                  []Swimlane{{ID: "s", Name: "S", Kind: "team"}},
		Slices:                     []Slice{{ID: "t", Name: "T", Pattern: "translation", SwimlaneID: "s"}},
	}
	err := Lint(doc).Err()
	if err == nil {
		t.Fatal("a translation slice must be refused")
	}
	if !strings.Contains(err.Error(), "automation") {
		t.Errorf("the refusal must say what to use instead: %v", err)
	}
}

// TestLintRefusesPatternFieldsThatDoNotBelong: the source schema is
// allOf+if/then under one unevaluatedProperties, so a property is legal only
// on the pattern whose branch declares it. A screenId on an automation slice
// is invalid, not merely surplus — and that is the same trap an export must
// avoid when synthesizing screens.
func TestLintRefusesPatternFieldsThatDoNotBelong(t *testing.T) {
	doc, err := Load(fixture(t, "examples/order-fulfillment.json"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range doc.Slices {
		if doc.Slices[i].Pattern == PatternAutomation {
			doc.Slices[i].ScreenID = "place-order-screen"
		}
	}
	lerr := Lint(doc).Err()
	if lerr == nil || !strings.Contains(lerr.Error(), "screenId") {
		t.Fatalf("screenId on an automation slice must be refused: %v", lerr)
	}
}

func TestNameDerivation(t *testing.T) {
	cases := []struct{ typeName, id, name string }{
		{"OrderPlaced", "order-placed", "Order Placed"},
		{"OrderPDFGenerated", "order-pdf-generated", "Order PDF Generated"}, // acronym run stays whole
		{"Shipped", "shipped", "Shipped"},
		{"V2Migrated", "v2-migrated", "V2 Migrated"},
	}
	for _, c := range cases {
		if got := DeriveID(c.typeName); got != c.id {
			t.Errorf("DeriveID(%q) = %q, want %q", c.typeName, got, c.id)
		}
		if got := DeriveName(c.typeName); got != c.name {
			t.Errorf("DeriveName(%q) = %q, want %q", c.typeName, got, c.name)
		}
	}

	// and the other direction, from a real document's name/id pair
	if got := TypeName("Order Placed", "order-placed"); got != "OrderPlaced" {
		t.Errorf("TypeName = %q", got)
	}
	// a name that is unusable as an identifier falls back to the id
	if got := TypeName("!!!", "order-placed"); got != "OrderPlaced" {
		t.Errorf("TypeName fallback = %q", got)
	}
	// the round trip holds for the derivation itself, which is what the
	// loss test asserts against for un-mapped elements
	for _, c := range cases {
		if got := TypeName(DeriveName(c.typeName), DeriveID(c.typeName)); got != c.typeName {
			t.Errorf("round trip of %q gave %q", c.typeName, got)
		}
	}
}

func TestFoldType(t *testing.T) {
	cases := []struct {
		field Field
		want  string
	}{
		{Field{Type: "string"}, "text"},
		{Field{Type: "uuid"}, "text"},
		{Field{Type: "boolean"}, "bool"},
		{Field{Type: "integer"}, "number"},
		{Field{Type: "long"}, "number"},
		{Field{Type: "decimal"}, "number"},
		{Field{Type: "double"}, "number"},
		{Field{Type: "date"}, "date"},
		{Field{Type: "dateTime"}, "date"},
		{Field{Type: "custom"}, "json"},
		// the two collapsing rules, which matter as much as the table
		{Field{Type: "string", Cardinality: "list"}, "json"},
		{Field{Type: "string", Subfields: []Field{{Name: "a", Type: "string"}}}, "json"},
	}
	for _, c := range cases {
		if got := FoldType(c.field); got != c.want {
			t.Errorf("FoldType(%+v) = %q, want %q", c.field, got, c.want)
		}
	}

	// every fold that loses something must be explainable, or the loss is
	// silent — which is the failure mode this project keeps paying for
	for _, f := range []Field{
		{Name: "x", Type: "uuid"}, {Name: "x", Type: "long"}, {Name: "x", Type: "dateTime"},
		{Name: "x", Type: "custom"}, {Name: "x", Type: "string", Cardinality: "list"},
	} {
		if FoldNote("event e", f) == "" {
			t.Errorf("a lossy fold of %+v produced no note", f)
		}
	}
	// ...and a faithful one says nothing
	if note := FoldNote("event e", Field{Name: "x", Type: "string"}); note != "" {
		t.Errorf("a faithful fold should be silent, got %q", note)
	}
}

// TestManifestPathsCannotEscape: a manifest is data, and data must not be
// able to name a file outside the tree it came from.
func TestManifestPathsCannotEscape(t *testing.T) {
	dir := t.TempDir()
	manifest := `{"eventModelingSchemaVersion":"1.0.0","id":"x","name":"X",
	  "swimlanes":[{"id":"s","name":"S","kind":"team"}],
	  "registries":{"events":"../../../etc/passwd"},"slices":[]}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("a manifest path escaping its directory must be refused, got %v", err)
	}
}

// TestUnknownRegistryIsRefused: importing the rest of a document that means
// more than this version understands would lose the difference silently.
func TestUnknownRegistryIsRefused(t *testing.T) {
	dir := t.TempDir()
	manifest := `{"eventModelingSchemaVersion":"1.0.0","id":"x","name":"X",
	  "swimlanes":[{"id":"s","name":"S","kind":"team"}],
	  "registries":{"policies":"policies.json"},"slices":[]}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "policies") {
		t.Fatalf("an unknown registry must be refused by name, got %v", err)
	}
}

// TestVersionIsReportedNotTrusted: upstream bumped to 2.0.0, which fixes the
// signal going forward but cannot fix documents already written. A document
// authored between the v2 schema changes and the bump declares 1.x while
// being a v2 document, and a genuine v1 document declares the same — so the
// header is a note and the SHAPE is the check.
func TestVersionIsReportedNotTrusted(t *testing.T) {
	base := func(version string) *Document {
		return &Document{
			EventModelingSchemaVersion: version,
			Swimlanes:                  []Swimlane{{ID: "s", Name: "S", Kind: "team"}},
			Events:                     map[string]Event{"e": {Name: "E", SwimlaneID: "s"}},
			Commands:                   map[string]Command{"c": {Name: "C"}},
			Screens:                    map[string]Screen{"sc": {Name: "Sc"}},
			Slices: []Slice{{
				ID: "sl", Name: "Sl", Pattern: PatternStateChange, SwimlaneID: "s",
				Status: "created", ScreenID: "sc", CommandID: "c", EventIDs: []string{"e"},
				Scenarios: []Scenario{},
			}},
		}
	}

	// a current document lints clean and says nothing about its version
	rep := Lint(base(SchemaVersion))
	if err := rep.Err(); err != nil {
		t.Fatalf("a %s document must lint clean: %v", SchemaVersion, err)
	}
	for _, w := range rep.Warnings {
		if strings.Contains(w, "eventModelingSchemaVersion") {
			t.Errorf("a matching version should not be remarked on: %s", w)
		}
	}

	// an older header is NOTED, not refused: the shape is v2-valid, so the
	// document imports
	rep = Lint(base("1.0.0"))
	if err := rep.Err(); err != nil {
		t.Fatalf("an old version header must not refuse a structurally valid document: %v", err)
	}
	var noted bool
	for _, w := range rep.Warnings {
		if strings.Contains(w, "1.0.0") && strings.Contains(w, SchemaVersion) {
			noted = true
		}
	}
	if !noted {
		t.Errorf("an older declared version should be reported: %v", rep.Warnings)
	}

	// ...and the SHAPE is what actually refuses a v1 document, whatever its
	// header claims — here a 2.0.0 header on a translation slice
	v1 := base(SchemaVersion)
	v1.Slices[0].Pattern = "translation"
	if err := Lint(v1).Err(); err == nil {
		t.Error("a translation slice must be refused even when the header says 2.0.0")
	}
}
