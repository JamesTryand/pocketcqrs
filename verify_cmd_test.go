package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const conditionalOutcomes = "testdata/eventmodelschema/examples/conditional-outcomes.json"

func runVerify(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newSchemaVerifyCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// hostFunctions imports the document into a directory, then adds the
// host's own hand file: a host's pb_functions.
func hostFunctions(t *testing.T, hook string) string {
	t.Helper()
	dir := t.TempDir()
	imp := newSchemaImportCommand()
	var out bytes.Buffer
	imp.SetOut(&out)
	imp.SetErr(&out)
	imp.SetArgs([]string{conditionalOutcomes, "--skip-scenarios", "--out", dir})
	if err := imp.Execute(); err != nil {
		t.Fatalf("import: %v\n%s", err, out.String())
	}
	src := "function RequestRefundOutcome0(state, command) { return " + hook + "; }\n"
	if err := os.WriteFile(filepath.Join(dir, "refund.hand.js"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Against the generated code: a HAND scenario is not a failure, so the
// command succeeds, and writes nothing.
func TestSchemaVerifyGenerated(t *testing.T) {
	out, err := runVerify(t, conditionalOutcomes)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "3 passed, 0 failed, 0 skipped, 1 hand") || !strings.Contains(out, "the generated code") {
		t.Errorf("summary:\n%s", out)
	}
}

// Against a host's functions: its hook decides, so the HAND scenario passes
// with a right hook, and fails the command with a wrong one.
func TestSchemaVerifyHost(t *testing.T) {
	right := hostFunctions(t, "command.payload.amount > 500")
	out, err := runVerify(t, conditionalOutcomes, "--functions", right)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "4 passed, 0 failed, 0 skipped, 0 hand") || !strings.Contains(out, right) {
		t.Errorf("summary:\n%s", out)
	}
	wrong := hostFunctions(t, "false")
	out, err = runVerify(t, conditionalOutcomes, "--functions", wrong)
	if err == nil || !strings.Contains(out, "3 passed, 1 failed") {
		t.Errorf("a wrong hook: err=%v\n%s", err, out)
	}
}
