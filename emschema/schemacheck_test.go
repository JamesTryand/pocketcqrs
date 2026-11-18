package emschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The embedded schema (go:embed cannot reach testdata/) is the vendored copy,
// byte for byte, so the two cannot drift apart.
func TestEmbeddedSchemaIsTheVendoredCopy(t *testing.T) {
	vendored, err := os.ReadFile(fixture(t, "eventmodeling.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	norm := func(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }
	if !bytes.Equal(norm(embeddedSchema), norm(vendored)) {
		t.Fatal("emschema/schema/eventmodeling.schema.json differs from testdata/eventmodelschema/eventmodeling.schema.json")
	}
}

// readAccessWithout loads upstream's read-access example and lets the test
// break it.
func readAccessWithout(t *testing.T, edit func(doc map[string]any)) []byte {
	t.Helper()
	raw, err := os.ReadFile(fixture(t, "examples/read-access.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	edit(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func viewScenarios(doc map[string]any) []any {
	slice := doc["slices"].([]any)[3].(map[string]any)
	return slice["scenarios"].([]any)
}

// A view scenario's caller needs a subjectId (schema 3.8.0). The importer used
// to accept a caller given only a role, because the allow-list checks keys it
// does not read, not keys the schema requires. Now the schema check refuses
// it, naming each caller and nothing else.
func TestACallerWithoutSubjectIdIsRefusedNamingEachCaller(t *testing.T) {
	if _, err := Parse("read-access.json", readAccessWithout(t, func(map[string]any) {})); err != nil {
		t.Fatalf("the unbroken example was refused: %v", err)
	}

	raw := readAccessWithout(t, func(doc map[string]any) {
		for _, i := range []int{0, 2} {
			caller := viewScenarios(doc)[i].(map[string]any)["when"].(map[string]any)["caller"].(map[string]any)
			delete(caller, "subjectId")
		}
	})
	_, err := Parse("read-access.json", raw)
	var se *SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("want a SchemaError, got %v", err)
	}
	want := []SchemaProblem{
		{Pointer: "/slices/3/scenarios/0/when/caller", Message: "missing property 'subjectId'"},
		{Pointer: "/slices/3/scenarios/2/when/caller", Message: "missing property 'subjectId'"},
	}
	if len(se.Problems) != len(want) {
		t.Fatalf("want exactly %d problems (the echoes of each one dropped), got %d:\n%v", len(want), len(se.Problems), err)
	}
	for i := range want {
		if se.Problems[i] != want[i] {
			t.Errorf("problem %d: want %+v, got %+v", i, want[i], se.Problems[i])
		}
	}
	if !strings.Contains(err.Error(), "nothing was generated") {
		t.Errorf("the message should say nothing was generated:\n%v", err)
	}
}

// A refusal always gives a reason: an enum value that matches nothing is
// reported, though it is not under a oneOf alternative or beside anything.
func TestAnInvalidEnumValueIsNamed(t *testing.T) {
	raw := readAccessWithout(t, func(doc map[string]any) {
		doc["swimlanes"].([]any)[0].(map[string]any)["kind"] = "not-a-kind"
	})
	_, err := Parse("read-access.json", raw)
	var se *SchemaError
	if !errors.As(err, &se) || len(se.Problems) == 0 {
		t.Fatalf("want a SchemaError with a reason, got %v", err)
	}
	if se.Problems[0].Pointer != "/swimlanes/0/kind" {
		t.Errorf("want the problem at /swimlanes/0/kind, got %+v", se.Problems)
	}
}

// A split document is checked as the whole document it joins into, and a
// problem is named in the file it is in.
func TestASplitDocumentIsCheckedAndNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", `{"eventModelingSchemaVersion":"4.2.0","id":"d","name":"D",
		"swimlanes":[{"id":"s","name":"S","kind":"team"}],
		"registries":{"events":"registries/events.json","commands":"registries/commands.json","screens":"registries/screens.json"},
		"slices":["slices/place.json"]}`)
	write("registries/events.json", `{"placed":{"name":"Placed","swimlaneId":"s"}}`)
	write("registries/commands.json", `{"place":{"name":"Place"}}`)
	write("registries/screens.json", `{"place-screen":{"name":"Place"}}`)
	slice := `{"id":"place-slice","name":"Place","pattern":"stateChange","swimlaneId":"s",%s
		"screenId":"place-screen","commandId":"place","eventIds":["placed"],"scenarios":[]}`
	write("slices/place.json", strings.Replace(slice, "%s", `"status":"created",`, 1))
	if _, err := LoadManifest(dir); err != nil {
		t.Fatalf("the valid split document was refused: %v", err)
	}

	write("slices/place.json", strings.Replace(slice, "%s", "", 1))
	_, err := LoadManifest(dir)
	var se *SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("want a SchemaError, got %v", err)
	}
	if len(se.Problems) != 1 || se.Problems[0].Pointer != "slices/place.json" || se.Problems[0].Message != "missing property 'status'" {
		t.Errorf("want one problem, the slice file's missing status, got %+v", se.Problems)
	}

	// and in a registry file, under the key the problem is at
	write("slices/place.json", strings.Replace(slice, "%s", `"status":"created",`, 1))
	write("registries/events.json", `{"placed":{"name":"Placed"}}`)
	_, err = LoadManifest(dir)
	if !errors.As(err, &se) {
		t.Fatalf("want a SchemaError, got %v", err)
	}
	if len(se.Problems) != 1 || se.Problems[0].Pointer != "registries/events.json/placed" {
		t.Errorf("want one problem in the events file, at placed, got %+v", se.Problems)
	}
}
