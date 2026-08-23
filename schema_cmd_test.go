package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newSchemaImportCommand must return an error when the document is refused,
// so main()'s short-circuit (which calls Execute() directly and turns a
// non-nil error into os.Exit(1)) actually has something to act on. This is
// the unit-level half of F-9's fix; smoke/schema_import_test.go proves the
// real process exit code and the absence of a pb_data/ side effect (F-10),
// neither of which this in-process test can observe.
func TestSchemaImportReturnsErrorOnRefusal(t *testing.T) {
	dir := t.TempDir()
	bogus := filepath.Join(dir, "bogus.json")
	if err := os.WriteFile(bogus, []byte(`{"not":"a schema"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newSchemaImportCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{bogus})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("a refused document was accepted (no error returned)")
	}
	if !strings.Contains(out.String(), "REFUSED") {
		t.Errorf("refusal reason was not printed:\n%s", out.String())
	}
}

func TestSchemaImportReturnsErrorOnMissingFile(t *testing.T) {
	cmd := newSchemaImportCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{filepath.Join(t.TempDir(), "nonexistent.json")})

	if err := cmd.Execute(); err == nil {
		t.Fatal("a nonexistent document was accepted (no error returned)")
	}
}

// The success path must still return nil, or every successful import would
// also start exiting non-zero once main() acts on Execute()'s return value.
func TestSchemaImportReturnsNilOnSuccess(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "good.json")
	body := `{"swimlanes":[{"aggregate":"widget","elements":[
		{"kind":"command","name":"CreateWidget"},
		{"kind":"event","name":"WidgetCreated"}
	]}]}`
	if err := os.WriteFile(doc, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newSchemaImportCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{doc, "--skip-scenarios"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("a valid document was refused: %v\n%s", err, out.String())
	}
}

// import touches no *components field and no core.App (see
// newSchemaImportCommand's doc comment) -- this is what makes the
// main()-level short-circuit safe. Assert it directly, not just via
// inspection: a future edit adding app-dependent behavior to this command
// without also removing it from the short-circuit would silently reintroduce
// F-10 (bootstrap runs, RunE doesn't) or a nil-pointer panic (RunE runs
// without the app it now needs). Global flags the app's own RootCmd would
// have registered (--dir, --dev) are correctly unrecognized here, not
// silently accepted and ignored -- this command was never wired to act on
// them even before the short-circuit existed.
// widgetDoc is a real, minimally-valid EventModeling document (the same
// shape as testdata/eventmodelschema/examples/minimal.json, renamed) — the
// ad-hoc "swimlanes[].elements[]" shape the older tests in this file use
// happens to map to zero domains (Map silently produces nothing an
// untagged element can own), which is fine for asserting "no error" but
// useless for asserting what got written.
const widgetDoc = `{
	"swimlanes": [{ "id": "widgets", "name": "Widgets", "kind": "team" }],
	"events": { "widget-created": { "name": "Widget Created", "swimlaneId": "widgets" } },
	"commands": { "create-widget": { "name": "Create Widget" } },
	"screens": { "create-widget-screen": { "name": "Create Widget" } },
	"slices": [{
		"id": "create-widget-slice", "name": "Create Widget", "pattern": "stateChange",
		"swimlaneId": "widgets", "status": "created", "screenId": "create-widget-screen",
		"commandId": "create-widget", "eventIds": ["widget-created"], "scenarios": []
	}]
}`

// TestSchemaImportDefaultLangIsJS: --lang go is additive. With no --lang
// flag at all, import must still write .js files exactly as before the
// flag existed — backward compatibility is a stated design decision
// (task_plan.md's "--lang go|js on pocketcqrs schema import (default js,
// backward compatible)"), not an incidental default.
func TestSchemaImportDefaultLangIsJS(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "widget.json")
	if err := os.WriteFile(doc, []byte(widgetDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")

	cmd := newSchemaImportCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{doc, "--skip-scenarios", "--out", outDir, "--aggregate", "create-widget=widget"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("import failed: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(outDir, "widget.js")); err != nil {
		t.Errorf("expected widget.js with no --lang flag, got: %v", err)
	}
}

// TestSchemaImportLangGoWritesGoFiles: --lang go writes .go files and
// prints the suggested wiring instead of the JS "copy into functionsDir"
// message.
func TestSchemaImportLangGoWritesGoFiles(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "widget.json")
	if err := os.WriteFile(doc, []byte(widgetDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")

	cmd := newSchemaImportCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{doc, "--skip-scenarios", "--out", outDir, "--aggregate", "create-widget=widget", "--lang", "go"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("import failed: %v\n%s", err, out.String())
	}
	content, err := os.ReadFile(filepath.Join(outDir, "widget.go"))
	if err != nil {
		t.Fatalf("expected widget.go with --lang go, got: %v", err)
	}
	if !strings.Contains(string(content), "package widget") {
		t.Errorf("expected a package clause in the generated file:\n%s", content)
	}
	// The printed suggestion names decider.Register(registry, "widget",
	// widget.Widget()) -- pin that the ctor it names ("Widget") is the same
	// one the file actually declares, not just that both happen to say
	// "widget" somewhere. A drift between printGoWiringSuggestions' naming
	// and GenerateGo's own would otherwise only surface for a caller who
	// pastes the suggestion and hits a compile error.
	if !strings.Contains(string(content), "func Widget()") {
		t.Errorf("suggested ctor Widget() has no matching func in the generated file:\n%s", content)
	}
	if !strings.Contains(out.String(), `decider.Register(registry, "widget", widget.Widget())`) {
		t.Errorf("expected a suggested registration line:\n%s", out.String())
	}
	if strings.Contains(out.String(), "functionsDir") {
		t.Errorf("the JS-specific completion message must not print for --lang go:\n%s", out.String())
	}
}

// TestSchemaImportLangGoSuggestionMatchesGeneratedCode uses a mixed-case
// aggregate override ("supportTicket") where GoPackageName (lowercases only
// the first rune) and scaffold.ExportName (upper-cases only the first rune)
// diverge from each other more visibly than the all-lowercase "widget"
// fixture does: the package clause must read "package supportTicket" and
// the ctor "func SupportTicket()", exactly what the printed suggestion
// names -- the two are backed by the same scaffold helpers precisely so
// they can't drift.
func TestSchemaImportLangGoSuggestionMatchesGeneratedCode(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "widget.json")
	if err := os.WriteFile(doc, []byte(widgetDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")

	cmd := newSchemaImportCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{doc, "--skip-scenarios", "--out", outDir, "--aggregate", "create-widget=supportTicket", "--lang", "go"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("import failed: %v\n%s", err, out.String())
	}
	content, err := os.ReadFile(filepath.Join(outDir, "supportTicket.go"))
	if err != nil {
		t.Fatalf("expected supportTicket.go with --lang go, got: %v", err)
	}
	if !strings.Contains(string(content), "package supportTicket") {
		t.Errorf("expected package supportTicket in the generated file:\n%s", content)
	}
	if !strings.Contains(string(content), "func SupportTicket()") {
		t.Errorf("expected func SupportTicket() in the generated file:\n%s", content)
	}
	if !strings.Contains(out.String(), `decider.Register(registry, "supportTicket", supportTicket.SupportTicket())`) {
		t.Errorf("expected a suggestion matching the generated package/ctor exactly:\n%s", out.String())
	}
}

// TestSchemaImportLangGoWarnsOnIgnoredDocsFlag: --docs writes nothing for
// --lang go (domain docs are a JS-only concept), and previously said
// nothing about it either -- an explicit flag producing silent nothing is
// exactly the failure shape this codebase's own Warnings()/NoFields
// precedent exists to avoid. It must say so instead.
func TestSchemaImportLangGoWarnsOnIgnoredDocsFlag(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "widget.json")
	if err := os.WriteFile(doc, []byte(widgetDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")
	docsDir := filepath.Join(dir, "docs")

	cmd := newSchemaImportCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{doc, "--skip-scenarios", "--out", outDir, "--docs", docsDir,
		"--aggregate", "create-widget=widget", "--lang", "go"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("import failed: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "--docs") || !strings.Contains(out.String(), "ignored") {
		t.Errorf("expected a warning that --docs was ignored for --lang go:\n%s", out.String())
	}
}

// TestSchemaImportLangGoScenarioMessageNamesJS: scenarios always run against
// the JS mapping (emschema.Verify has no Go counterpart), so the completion
// text for --lang go must not claim the Go files themselves were checked.
func TestSchemaImportLangGoScenarioMessageNamesJS(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "widget.json")
	if err := os.WriteFile(doc, []byte(widgetDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(dir, "out")

	cmd := newSchemaImportCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{doc, "--out", outDir, "--aggregate", "create-widget=widget", "--lang", "go"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("import failed: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "checked against the generated code") {
		t.Errorf("--lang go must not claim the Go output itself was scenario-checked:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "JS mapping") {
		t.Errorf("expected the scenario message to name the JS mapping for --lang go:\n%s", out.String())
	}
}

func TestSchemaImportRejectsUnknownLang(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "good.json")
	if err := os.WriteFile(doc, []byte(`{"swimlanes":[{"aggregate":"w","elements":[{"kind":"command","name":"C"}]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newSchemaImportCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{doc, "--skip-scenarios", "--lang", "rust"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected an error for an unsupported --lang value")
	} else if !strings.Contains(err.Error(), `--lang wants "js" or "go"`) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSchemaImportRejectsAppOnlyFlags(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "good.json")
	if err := os.WriteFile(doc, []byte(`{"swimlanes":[{"aggregate":"w","elements":[{"kind":"command","name":"C"}]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newSchemaImportCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{doc, "--dev=false", "--skip-scenarios"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("--dev was silently accepted; expected \"unknown flag\"")
	} else if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("expected an unknown-flag error, got: %v", err)
	}
}
