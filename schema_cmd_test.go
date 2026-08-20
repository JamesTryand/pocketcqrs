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
