//go:build smoke

package smoke

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSchemaImportRefusalExitsNonZero and TestSchemaImportRefusalWritesNoPbData
// prove F-9 and F-10's fix against a real built binary and a real process
// exit code -- neither is observable from an in-process cmd.Execute() call
// (schema_cmd_test.go's unit tests), since both bugs were specifically about
// what happens OUTSIDE the command's own RunE: PocketBase's bootstrap
// running before it (F-10), and pb.Execute() discarding RootCmd.Execute()'s
// return value on the way back out (F-9). Run from a fresh, empty working
// directory each time so a stray pb_data/ has nowhere to hide.
func TestSchemaImportRefusalExitsNonZero(t *testing.T) {
	bin := build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(t.TempDir(), "pocketcqrs"))
	dir := t.TempDir()

	bogus := filepath.Join(dir, "bogus.json")
	if err := os.WriteFile(bogus, []byte(`{"not":"a schema"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "schema", "import", "bogus.json")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("refused import exited 0; output:\n%s", out)
	}
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() == 0 {
		t.Fatalf("expected a non-zero exit code, got %v; output:\n%s", err, out)
	}
}

func TestSchemaImportRefusalWritesNoPbData(t *testing.T) {
	bin := build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(t.TempDir(), "pocketcqrs"))
	dir := t.TempDir()

	bogus := filepath.Join(dir, "bogus.json")
	if err := os.WriteFile(bogus, []byte(`{"not":"a schema"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "schema", "import", "bogus.json")
	cmd.Dir = dir
	_, _ = cmd.CombinedOutput() // expected to fail; exit code covered separately above

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "pb_data" {
			t.Fatalf("a refused import created %s, a bootstrap side effect it should never have (F-10)",
				filepath.Join(dir, "pb_data"))
		}
	}
}

// `schema verify` gets the same short-circuit: a failing scenario is a
// non-zero process exit (the check a build runs), and nothing bootstraps a
// pb_data/. A hand file whose hook never flags fails the over-limit
// scenario of upstream's conditional-outcomes.json.
func TestSchemaVerifyFailureExitsNonZeroWithNoPbData(t *testing.T) {
	bin := build(t, "github.com/jamestryand/pocketcqrs", filepath.Join(t.TempDir(), "pocketcqrs"))
	dir := t.TempDir()
	doc, err := filepath.Abs("../testdata/eventmodelschema/examples/conditional-outcomes.json")
	if err != nil {
		t.Fatal(err)
	}
	functions := filepath.Join(dir, "functions")
	if out, err := exec.Command(bin, "schema", "import", doc, "--skip-scenarios", "--out", functions).CombinedOutput(); err != nil {
		t.Fatalf("import: %v\n%s", err, out)
	}
	hand := "function SubmitExpenseOutcome0(state, command) { return false; }\n"
	if err := os.WriteFile(filepath.Join(functions, "expense.hand.js"), []byte(hand), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "schema", "verify", doc, "--functions", functions)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() == 0 {
		t.Fatalf("a failing scenario exited %v; output:\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "pb_data")); err == nil {
		t.Fatal("verify bootstrapped a pb_data/ (F-10)")
	}
}
